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

## Known limitation: concurrent migration

Two processes starting against the same database at the same time (a rolling
update with `maxSurge` rounding up to 1, a blue-green restart) can both decide a
migration is unapplied. One applies it, the other fails on the objects that now
exist and rolls back, dropping what the first created.

This is a property of the migration mechanism as a whole, not of this feature's
migration, which is purely additive and otherwise rolling-safe. Fixing it needs a
migration-wide advisory lock with per-dialect implementations, and belongs to the
migration mechanism rather than here.
