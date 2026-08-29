package db_lib

import (
	"fmt"
	"strings"
	"testing"

	"github.com/semaphoreui/semaphore/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseINIInventory(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected InventoryHosts
	}{
		{
			name: "hosts before the first header are ungrouped",
			content: `
web01.example.com
web02.example.com

[web]
web01.example.com

[db]
db01.example.com
`,
			expected: InventoryHosts{
				Hosts: []string{"db01.example.com", "web01.example.com", "web02.example.com"},
				Groups: []InventoryGroup{
					{Name: "db", Hosts: []string{"db01.example.com"}, Children: []string{}},
					{Name: "ungrouped", Hosts: []string{"web02.example.com"}, Children: []string{}},
					{Name: "web", Hosts: []string{"web01.example.com"}, Children: []string{}},
				},
			},
		},
		{
			name: "children sections resolve transitively",
			content: `
[web]
web01

[db]
db01

[prod:children]
web
db

[prod:vars]
ansible_user=deploy
`,
			expected: InventoryHosts{
				Hosts: []string{"db01", "web01"},
				Groups: []InventoryGroup{
					{Name: "db", Hosts: []string{"db01"}, Children: []string{}},
					{Name: "prod", Hosts: []string{"db01", "web01"}, Children: []string{"db", "web"}},
					{Name: "web", Hosts: []string{"web01"}, Children: []string{}},
				},
			},
		},
		{
			name: "grandchildren resolve through two levels",
			content: `
[web]
web01

[prod:children]
web

[dc1:children]
prod
`,
			expected: InventoryHosts{
				Hosts: []string{"web01"},
				Groups: []InventoryGroup{
					{Name: "dc1", Hosts: []string{"web01"}, Children: []string{"prod"}},
					{Name: "prod", Hosts: []string{"web01"}, Children: []string{"web"}},
					{Name: "web", Hosts: []string{"web01"}, Children: []string{}},
				},
			},
		},
		{
			name: "host variables are discarded",
			content: `
[web]
web01 ansible_host=10.0.0.1 ansible_port=22
web02	ansible_user=deploy
`,
			expected: InventoryHosts{
				Hosts: []string{"web01", "web02"},
				Groups: []InventoryGroup{
					{Name: "web", Hosts: []string{"web01", "web02"}, Children: []string{}},
				},
			},
		},
		{
			name: "comments and blank lines are skipped",
			content: `
# a hash comment
; a semicolon comment

[web]
   web01
# [staging]
; web99
`,
			expected: InventoryHosts{
				Hosts: []string{"web01"},
				Groups: []InventoryGroup{
					{Name: "web", Hosts: []string{"web01"}, Children: []string{}},
				},
			},
		},
		{
			// An inventory authored on Windows would otherwise yield host
			// names ending in a carriage return, which resolve to nothing.
			name:    "CRLF line endings",
			content: "[web]\r\nweb01\r\nweb02\r\n",
			expected: InventoryHosts{
				Hosts: []string{"web01", "web02"},
				Groups: []InventoryGroup{
					{Name: "web", Hosts: []string{"web01", "web02"}, Children: []string{}},
				},
			},
		},
		{
			name: "the implicit all group is not emitted",
			content: `
[all]
web01

[all:vars]
ansible_user=deploy
`,
			expected: InventoryHosts{
				Hosts: []string{"web01"},
				Groups: []InventoryGroup{
					{Name: "ungrouped", Hosts: []string{"web01"}, Children: []string{}},
				},
			},
		},
		{
			name: "a group referenced only as a child still exists",
			content: `
[prod:children]
web
`,
			expected: InventoryHosts{
				Hosts: []string{},
				Groups: []InventoryGroup{
					{Name: "prod", Hosts: []string{}, Children: []string{"web"}},
					{Name: "web", Hosts: []string{}, Children: []string{}},
				},
			},
		},
		{
			name: "a group holding only variables still exists",
			content: `
[web:vars]
ansible_user=deploy
`,
			expected: InventoryHosts{
				Hosts: []string{},
				Groups: []InventoryGroup{
					{Name: "web", Hosts: []string{}, Children: []string{}},
				},
			},
		},
		{
			name: "a host in two groups is listed once",
			content: `
[web]
web01

[prod]
web01
`,
			expected: InventoryHosts{
				Hosts: []string{"web01"},
				Groups: []InventoryGroup{
					{Name: "prod", Hosts: []string{"web01"}, Children: []string{}},
					{Name: "web", Hosts: []string{"web01"}, Children: []string{}},
				},
			},
		},
		{
			name:    "empty inventory",
			content: "",
			expected: InventoryHosts{
				Hosts:  []string{},
				Groups: []InventoryGroup{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseINIInventory(tt.content)

			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// A recursive group definition makes ansible-playbook fail, but the parser
// still has to terminate and report what it could read.
func TestParseINIInventoryBreaksGroupCycles(t *testing.T) {
	result, err := ParseINIInventory(`
[a]
h1

[b]
h2

[a:children]
b

[b:children]
a
`)

	require.NoError(t, err)
	assert.Equal(t, []string{"h1", "h2"}, result.Hosts)
	require.Len(t, result.Groups, 2)
	assert.Equal(t, []string{"h1", "h2"}, result.Groups[0].Hosts)
	assert.Equal(t, []string{"h1", "h2"}, result.Groups[1].Hosts)
}

func TestParseINIInventoryExpandsRanges(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected []string
	}{
		{
			name:     "zero padded numeric range",
			content:  "web[01:03].example.com",
			expected: []string{"web01.example.com", "web02.example.com", "web03.example.com"},
		},
		{
			name:     "unpadded numeric range",
			content:  "web[8:11]",
			expected: []string{"web10", "web11", "web8", "web9"},
		},
		{
			name:     "numeric range with a step",
			content:  "node[1:5:2]",
			expected: []string{"node1", "node3", "node5"},
		},
		{
			name:     "alphabetic range",
			content:  "host[a:c]",
			expected: []string{"hosta", "hostb", "hostc"},
		},
		{
			// Ansible resolves alphabetic bounds through
			// string.ascii_letters, where lowercase precedes uppercase.
			name:     "alphabetic range crossing case",
			content:  "host[y:B]",
			expected: []string{"hostA", "hostB", "hosty", "hostz"},
		},
		{
			name:     "two ranges combine",
			content:  "r[1:2]-n[a:b]",
			expected: []string{"r1-na", "r1-nb", "r2-na", "r2-nb"},
		},
		{
			name:     "an omitted begin bound defaults to zero",
			content:  "web[:2]",
			expected: []string{"web0", "web1", "web2"},
		},
		{
			// Python's range() is empty here, so the entry names no host.
			name:     "a descending numeric range names nothing",
			content:  "web[5:1]",
			expected: []string{},
		},
		{
			// detect_range requires the first colon to sit between the
			// brackets, so this is a literal host name.
			name:     "a colon before the bracket is not a range",
			content:  "2001:db8::1[a]",
			expected: []string{"2001:db8::1[a]"},
		},
		{
			name:     "brackets without a colon are literal",
			content:  "web[01]",
			expected: []string{"web[01]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseINIInventory(tt.content)

			require.NoError(t, err)
			assert.Equal(t, tt.expected, result.Hosts)
		})
	}
}

func TestParseINIInventoryRejectsBrokenRanges(t *testing.T) {
	tests := []struct {
		name    string
		content string
		message string
	}{
		{
			name:    "too many bounds",
			content: "web[1:2:3:4]",
			message: "begin:end or begin:end:step",
		},
		{
			name:    "missing end bound",
			content: "web[1:]",
			message: "must specify an end value",
		},
		{
			name:    "zero step",
			content: "web[1:9:0]",
			message: "step must be a positive number",
		},
		{
			name:    "padding widths differ",
			content: "web[01:100]",
			message: "equal-length begin and end formats",
		},
		{
			// Ansible would expand this to a, b, c through a substring
			// search over ascii_letters. Inventing host names is worse than
			// refusing the range.
			name:    "multi-character alphabetic bounds",
			content: "web[ab:cd]",
			message: "neither a number nor a single letter",
		},
		{
			name:    "descending alphabetic range",
			content: "web[c:a]",
			message: "must have begin <= end",
		},
		{
			name:    "negative bound",
			content: "web[-5:5]",
			message: "must not be negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseINIInventory(tt.content)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.message)
		})
	}
}

// A host range is user-controlled input reachable by anyone who can read the
// inventory, so a single line must not be able to allocate without bound.
func TestParseINIInventoryRefusesOversizedRange(t *testing.T) {
	_, err := ParseINIInventory("web[1:1000000]")

	require.Error(t, err)

	var tooLarge InventoryTooLargeError
	require.ErrorAs(t, err, &tooLarge)
	assert.Equal(t, maxInventoryHosts, tooLarge.Limit)
}

// The budget spans the whole document: several ranges that individually fit
// must not add up past the cap.
func TestParseINIInventoryRefusesOversizedInventory(t *testing.T) {
	lines := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		lines = append(lines, fmt.Sprintf("rack%d-node[1:9000]", i))
	}

	_, err := ParseINIInventory(strings.Join(lines, "\n"))

	var tooLarge InventoryTooLargeError
	require.ErrorAs(t, err, &tooLarge)
}

// The budget counts generated names rather than distinct ones, so repeating
// the same expansion cannot be used to do unbounded work.
func TestParseINIInventoryCountsRepeatedExpansions(t *testing.T) {
	lines := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		lines = append(lines, "node[1:9000]")
	}

	_, err := ParseINIInventory(strings.Join(lines, "\n"))

	var tooLarge InventoryTooLargeError
	require.ErrorAs(t, err, &tooLarge)
}

// A group carries two membership maps of its own regardless of how many
// hosts it holds, so an inventory built mostly of empty groups can grow
// memory without ever touching the host budget above.
func TestParseINIInventoryRefusesTooManyGroups(t *testing.T) {
	var b strings.Builder
	for i := 0; i <= maxInventoryGroups; i++ {
		fmt.Fprintf(&b, "[group%d]\n", i)
	}

	_, err := ParseINIInventory(b.String())

	var tooLarge InventoryTooLargeError
	require.ErrorAs(t, err, &tooLarge)
	assert.Equal(t, maxInventoryGroups, tooLarge.Limit)
	assert.Equal(t, "groups", tooLarge.What)
}

// Many small groups that all point at the same large :children subtree
// multiply the transitive membership count well past both the host and the
// group budget: 26 groups sharing one 9000-host leaf resolve to 234000
// membership rows from an inventory that declares only 9000 hosts and 26
// groups.
func TestParseINIInventoryRefusesTooManyMemberships(t *testing.T) {
	var b strings.Builder
	b.WriteString("[leaf]\nhost[1:9000]\n\n")

	for i := 0; i < 25; i++ {
		fmt.Fprintf(&b, "[g%d:children]\nleaf\n", i)
	}

	_, err := ParseINIInventory(b.String())

	var tooLarge InventoryTooLargeError
	require.ErrorAs(t, err, &tooLarge)
	assert.Equal(t, maxInventoryMemberships, tooLarge.Limit)
	assert.Equal(t, "group memberships", tooLarge.What)
}

func TestParseYAMLInventory(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected InventoryHosts
	}{
		{
			name: "all with hosts and nested children",
			content: `
all:
  hosts:
    bastion.example.com:
  children:
    web:
      hosts:
        web01:
        web02:
          ansible_host: 10.0.0.2
    prod:
      children:
        web:
      vars:
        env: prod
`,
			expected: InventoryHosts{
				Hosts: []string{"bastion.example.com", "web01", "web02"},
				Groups: []InventoryGroup{
					{Name: "prod", Hosts: []string{"web01", "web02"}, Children: []string{"web"}},
					{Name: "ungrouped", Hosts: []string{"bastion.example.com"}, Children: []string{}},
					{Name: "web", Hosts: []string{"web01", "web02"}, Children: []string{}},
				},
			},
		},
		{
			// Not what the YAML inventory plugin accepts, but common in
			// hand-written files.
			name: "hosts written as a sequence",
			content: `
web:
  hosts:
    - web01
    - web02
`,
			expected: InventoryHosts{
				Hosts: []string{"web01", "web02"},
				Groups: []InventoryGroup{
					{Name: "web", Hosts: []string{"web01", "web02"}, Children: []string{}},
				},
			},
		},
		{
			name: "empty groups",
			content: `
web:
db:
`,
			expected: InventoryHosts{
				Hosts: []string{},
				Groups: []InventoryGroup{
					{Name: "db", Hosts: []string{}, Children: []string{}},
					{Name: "web", Hosts: []string{}, Children: []string{}},
				},
			},
		},
		{
			name: "variables carry no hosts",
			content: `
web:
  vars:
    ansible_user: deploy
  hosts:
    web01:
`,
			expected: InventoryHosts{
				Hosts: []string{"web01"},
				Groups: []InventoryGroup{
					{Name: "web", Hosts: []string{"web01"}, Children: []string{}},
				},
			},
		},
		{
			name: "ranges expand in host keys",
			content: `
web:
  hosts:
    "web[01:03]":
`,
			expected: InventoryHosts{
				Hosts: []string{"web01", "web02", "web03"},
				Groups: []InventoryGroup{
					{Name: "web", Hosts: []string{"web01", "web02", "web03"}, Children: []string{}},
				},
			},
		},
		{
			// A host name that parses as a number decodes to a non-string
			// key, which yaml.v3 hands back in a different map type.
			name: "numeric host key",
			content: `
web:
  hosts:
    10:
`,
			expected: InventoryHosts{
				Hosts: []string{"10"},
				Groups: []InventoryGroup{
					{Name: "web", Hosts: []string{"10"}, Children: []string{}},
				},
			},
		},
		{
			name:    "empty document",
			content: "",
			expected: InventoryHosts{
				Hosts:  []string{},
				Groups: []InventoryGroup{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseYAMLInventory(tt.content)

			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseYAMLInventoryRejectsNonMappingRoot(t *testing.T) {
	_, err := ParseYAMLInventory("- web01\n- web02\n")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a mapping")
}

func TestParseYAMLInventoryRejectsMalformedDocument(t *testing.T) {
	_, err := ParseYAMLInventory("web:\n  hosts:\n   - a\n  - b\n")

	assert.Error(t, err)
}

func TestParseInventoryDispatchesOnType(t *testing.T) {
	ini, err := ParseInventory(db.InventoryStatic, "[web]\nweb01\n")
	require.NoError(t, err)
	assert.Equal(t, []string{"web01"}, ini.Hosts)

	yml, err := ParseInventory(db.InventoryStaticYaml, "web:\n  hosts:\n    web01:\n")
	require.NoError(t, err)
	assert.Equal(t, []string{"web01"}, yml.Hosts)
}

// A file, Terraform workspace or dynamic inventory holds no host list the
// server can read from the database, and must not be reported as empty.
func TestParseInventoryRefusesNonStaticTypes(t *testing.T) {
	for _, inventoryType := range []db.InventoryType{
		db.InventoryFile,
		db.InventoryTerraformWorkspace,
		db.InventoryTofuWorkspace,
		db.InventoryTerragruntWorkspace,
	} {
		t.Run(string(inventoryType), func(t *testing.T) {
			_, err := ParseInventory(inventoryType, "")

			require.Error(t, err)
			assert.Contains(t, err.Error(), "no parseable host list")
		})
	}
}

// A "hosts" sequence may hold only scalars. fmt.Sprint of a nested mapping
// yields "map[name:web02 ansible_password:hunter2]", which would file the
// variable values into a host list every project member can read.
func TestParseYAMLInventorySkipsNonScalarHostItems(t *testing.T) {
	parsed, err := ParseYAMLInventory(`web:
  hosts:
    - web01
    - name: web02
      ansible_password: hunter2
    - - nested
    -
`)

	require.NoError(t, err)
	assert.Equal(t, []string{"web01"}, parsed.Hosts)

	for _, host := range parsed.Hosts {
		assert.NotContains(t, host, "hunter2")
	}
}

// Numbers and booleans are scalars, and Ansible does accept a host written as
// one, so the skip above must not take them with it.
func TestParseYAMLInventoryKeepsScalarHostItems(t *testing.T) {
	parsed, err := ParseYAMLInventory("web:\n  hosts:\n    - 10\n    - host01\n")

	require.NoError(t, err)
	assert.Equal(t, []string{"10", "host01"}, parsed.Hosts)
}

// Range bounds are user input, and the span arithmetic used to size the result
// runs on plain int. [0:MaxInt64] overflowed the count into a negative number,
// which passed the budget check and then panicked in make(); [MaxInt64:MaxInt64]
// wrapped the loop variable, so the loop appended names forever.
func TestParseINIInventoryRefusesRangeBoundsNearMaxInt(t *testing.T) {
	_, err := ParseINIInventory("web[0:9223372036854775807]")

	var tooLarge InventoryTooLargeError
	require.ErrorAs(t, err, &tooLarge)

	parsed, err := ParseINIInventory("web[9223372036854775807:9223372036854775807]")

	require.NoError(t, err)
	assert.Equal(t, []string{"web9223372036854775807"}, parsed.Hosts)
}

// Expansion recurses once per bracket group and rebuilds the whole entry at
// every level, while a group such as [0:0] spends no host budget at all — so
// the number of groups has to be capped on its own.
func TestParseINIInventoryRefusesTooManyRangePatterns(t *testing.T) {
	_, err := ParseINIInventory("h" + strings.Repeat("[0:0]", maxHostPatternRanges+1))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "range patterns")

	parsed, err := ParseINIInventory("h" + strings.Repeat("[0:0]", maxHostPatternRanges))

	require.NoError(t, err)
	assert.Equal(t, []string{"h" + strings.Repeat("0", maxHostPatternRanges)}, parsed.Hosts)
}

func TestParseINIInventoryRefusesOverlongHostEntry(t *testing.T) {
	_, err := ParseINIInventory(strings.Repeat("h", maxHostPatternLength+1))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "longer than")
}

// Ansible's ini plugin allows a trailing comment on a section header. Reading
// the line as a host instead loses the group and sends everything below it to
// "ungrouped".
func TestParseINIInventoryReadsSectionHeaderWithTrailingComment(t *testing.T) {
	parsed, err := ParseINIInventory("[web] # production\nhost1\n")

	require.NoError(t, err)
	assert.Equal(t, []string{"host1"}, parsed.Hosts)
	require.Len(t, parsed.Groups, 1)
	assert.Equal(t, "web", parsed.Groups[0].Name)
	assert.Equal(t, []string{"host1"}, parsed.Groups[0].Hosts)
}

// Ansible's parse_address splits the port off the host name, and everything
// built from the snapshot (--limit, the target host variable) addresses hosts
// by name, so a name carrying a port would match nothing.
func TestParseINIInventoryStripsHostPort(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected []string
	}{
		{"plain host with port", "host1:2222", []string{"host1"}},
		{"host without port", "host1", []string{"host1"}},
		{"range with port", "web[01:02].example.com:2222", []string{"web01.example.com", "web02.example.com"}},
		{"range without port", "web[01:02]", []string{"web01", "web02"}},
		{"unbracketed ipv6 keeps its trailing group", "fe80::1", []string{"fe80::1"}},
		{"trailing colon is not a port", "host1:", []string{"host1:"}},
		{"non-numeric suffix is not a port", "host1:ssh", []string{"host1:ssh"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := ParseINIInventory(tt.content)

			require.NoError(t, err)
			assert.Equal(t, tt.expected, parsed.Hosts)
		})
	}
}
