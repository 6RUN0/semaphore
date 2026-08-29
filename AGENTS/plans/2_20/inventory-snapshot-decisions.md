# Inventory Snapshot — Design Decisions

Decisions behind the inventory snapshot feature that the code cannot explain on
its own. Each one was a real choice with a rejected alternative; the code shows
what was chosen, not why the other option was worse.

## The API process runs git with project keys, on a GET path

`services/server/inventory_source.go` performs `ls-remote`, `clone` and `pull`
through `db_lib.AccessKeyInstaller` while serving `GET /project/{id}/inventory/
{iid}/snapshot`. Before this feature, git ran only on the task execution path.

Why: a host list is only useful while a person is filling in the task form, and
the form cannot wait for a task to be scheduled, dispatched to a runner and
reported back. Reading the inventory out of the repository is the only way to
answer at form-filling latency.

Consequences accepted deliberately:

- Any project member, including a read-only one, can cause a checkout. Semaphore
  already exposes this shape through `GetRepositoryBranches`, and the cost is
  bounded by the remote-head cache, the source fingerprint comparison and the
  walk budgets in `db_lib/InventorySource.go`.
- The decrypted key reaches the API process. It already does so for every task
  the process schedules, so this widens *when* it happens, not *what* is exposed.

Rejected: resolving through a task for every inventory. It is correct but far
too slow for a picker, and it makes reading a host list require permission to
run tasks.

## Freshness is checked on read, not on a schedule

There is no distributed scheduler to hang a periodic refresh on, so `ensureFresh`
runs as part of the read. Two nodes reaching it at once write identical rows, so
the race is harmless; it is only wasteful, which is why the rebuild is guarded by
double-checked locking rather than by coordination between nodes.

Executable inventories are the exception: the server never runs them, so their
host list can only come from a runner, and a stored resolve is treated as current
until another resolve replaces it.

## `tmp_path` must be node-local

The checkout directory name is derived from the repository id, so it is identical
on every node. Serialisation relies on `checkoutLock`, which is an in-process
lock. If `tmp_path` is placed on shared storage (an RWX volume, NFS), two nodes
will `git pull` into the same working tree at the same time and the lock will not
see it.

This requirement is inherited rather than introduced: `TaskPool.repoLock` has
always assumed the same thing. A file lock on the checkout directory would remove
the assumption, and would be the right fix if shared `tmp_path` ever has to be
supported.

## The resolve claim is taken before its task exists

`ClaimInventorySnapshotResolve` runs before the task row is created, and
`AssignInventorySnapshotResolveTask` fills in the task id afterwards. The order
matters: claiming after creating the task would let two dispatches both create a
task and only then discover that one of them lost.

Every later transition is scoped to the claim owner, so a claim taken over after
`MaxInventoryResolveAge` cannot be corrupted by the previous owner's late report.

## Picked hosts reach the playbook twice, and neither way is a variable by default

The hosts chosen in the task form become `--limit`, which is Ansible's own
mechanism and therefore **narrows** the play rather than defining it. The play
still runs against whatever its `hosts:` says, intersected with the limit. Three
consequences are worth stating because none of them is visible from the code:

- `hosts: all` with nothing picked runs against the entire inventory. The same
  happens when the template has `allow_override_limit` off, because the form's
  value is then ignored outright.
- `hosts: webservers` with `db01` picked runs nothing at all and **succeeds**:
  Ansible skips a play with no matching host and exits zero. A task that did
  nothing looks exactly like a task that worked.
- A limit that matches nothing anywhere in the inventory is a different case: it
  fails loudly instead.

Because of the second point, a playbook can instead select hosts itself, through
a variable. Two are provided, both carrying the same effective limit — the
template's value, replaced by the task's where the template permits it:

- `semaphore_vars.limit` — under the existing namespace, available without any
  configuration.
- the name in `target_host_var` (`SEMAPHORE_TARGET_HOST_VAR`) — off unless
  configured. It exists so that a playbook that already reads its own variable
  keeps working without being rewritten:
  `hosts: "{{ involved_hosts | default('all') }}"`.

Both are left **undefined** when nothing is picked, rather than set to an empty
string. Jinja's `default()` substitutes an undefined value and not an empty one,
so defining them empty would silently turn such a play into one matching no host
— reintroducing the very failure the variable is meant to avoid. A playbook that
reads the variable without `default()` therefore fails on exactly the runs where
nothing was picked, which is the run its author is least likely to try.

### Selecting hosts by variable widens what running a task means

This is a change of authority, not only of convenience, and it is the reason the
variable is off by default.

`--limit` can only **narrow**: the play runs against its `hosts:` intersected
with the limit, so picking `db01` on a `hosts: webservers` template runs nothing.
A playbook rewritten to `hosts: "{{ involved_hosts | default('all') }}"` reads the
same value as a **definition** instead, and the intersection no longer protects
anything because `--limit` carries that same value. Permission to run a template
that targets `webservers` becomes permission to run its content against any host
in the inventory bound to it.

Nothing checks the picked hosts against the inventory either: `params.Limit`
arrives from the request body, is not validated for membership anywhere, and the
combobox in the task form accepts free text. The bound is Ansible's own — a
pattern only matches hosts the inventory actually contains — so the reach is the
whole inventory of that template, not the whole fleet.

Two conditions gate all of this, and both are set by someone more privileged than
the person running the task: an administrator configures `target_host_var`, and
whoever owns the template both rewrites the playbook to read it and turns on
`allow_override_limit`. Leave `allow_override_limit` off and the variable carries
the template's own limit regardless of what the form shows — which is worth
saying out loud, because the form still lets hosts be picked and gives no sign
that they were ignored.

### Turning it on is the risky direction, not turning it off

Nothing is written to the database, so rollback is removing the key and
restarting; behaviour returns to `semaphore_vars.limit`. Playbooks that came to
depend on the alias then stop receiving it and fall back to `default('all')` —
the whole inventory — which is quiet rather than loud.

The breaking direction is enabling: the moment the name is set, a project
environment variable of that name stops reaching playbooks in every project on
the server. That is why the collision is reported in the task log rather than
merely being allowed to happen.

### The name is resolved on the server, never by the executor

`target_host_var` is configured on the server, but the code that publishes the
variable runs wherever the task runs — and a remote runner is a separate process
with its own configuration file. Reading `util.Config` there would make the
setting silently ineffective on every runner it was not copied to, and make one
template behave differently depending on which node picked the task up.

So the server resolves the name once, in `TaskRunner.populateDetails`, and it
travels to the executor on `db.Task.TargetHostVar`. Runners need no configuration
of their own. The field is assigned unconditionally because `db.Task` is bound
from the request body: a value the caller supplied must never survive.

### What the name may be, and what it displaces

`target_host_var` may not start with `ansible_` or `semaphore_`, and may not name
a variable Ansible reserves. The list of reserved names covers the magic
variables from Ansible's "Special Variables" appendix, the Jinja globals
(`lookup`, `q`, `query`), and the few Play keywords a name might plausibly
collide with. It is a guard against the worst names, not a proof of safety:
Ansible also reserves every Play, Role, Block and Task keyword, and that set
changes between releases. Rejected at startup.

For the duration of a run the name belongs to Semaphore: a project environment
variable of the same name is dropped and the task log says so. This is
unconditional, including when nothing was picked — otherwise a caller could set
the variable through `task.environment` and choose the hosts a playbook targets,
and could do it precisely when the template forbids overriding the limit, since a
forbidden override with no template limit is what produces an empty one. An
environment *secret* of that name still wins, because secrets are passed as
separate `--extra-vars` arguments after this one.

### Picked hosts are host patterns, not templates

The limit is user input that now reaches Ansible as the *value of a variable*,
and extra vars are rendered as templates. A pattern containing `{{ ... }}` would
be evaluated on the control node when the play is rendered — before hosts are
even selected, so it runs even when the pattern matches nothing. Both the task's
limit and the template's are therefore rejected if they contain a brace
(`db.ValidateAnsibleLimit`). No legitimate host pattern needs one: ranges,
exclusions, intersections, wildcards and regexes all pass.

The same reasoning closes `task.environment`, which is bound from the request
body and merged over the project's own variables, so the same actor could define
any variable with any content (`db.ValidateTaskEnvironment`). Values are walked
recursively, because a variable may hold an object or a list and Ansible renders
the strings inside those too.

Both checks are limited to Ansible tasks: the risk is Jinja rendering, and the
other apps pass these values as plain `-var` or `NAME=value` arguments, where a
brace is only a character.

This is narrower than Ansible's own answer, which is to mark user input unsafe so
it is never rendered. That would mean emitting the extra vars as tagged YAML
instead of JSON — changing the argument for every task, and not verifiable
without running Ansible. Rejecting a brace closes the same hole and costs only
the rare value that is meant to be a template. If such a value is ever needed,
the unsafe-tagging route is the thing to build, not an exception to this check.

## Known limitation: concurrent migration

Two processes starting against the same database at the same time (a rolling
update with `maxSurge` rounding up to 1, a blue-green restart) can both decide a
migration is unapplied. One applies it, the other fails on the objects that now
exist and rolls back, dropping what the first created.

This is a property of the migration mechanism as a whole, not of this feature's
migration, which is purely additive and otherwise rolling-safe. Fixing it needs a
migration-wide advisory lock with per-dialect implementations, and belongs to the
migration mechanism rather than here.
