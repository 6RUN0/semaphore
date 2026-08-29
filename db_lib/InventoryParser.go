package db_lib

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/semaphoreui/semaphore/db"
	"gopkg.in/yaml.v3"
)

// maxInventoryHosts caps how many host entries a single inventory may expand
// to. Host range patterns such as web[1:1000000] are written by users and
// combine multiplicatively, so an unbounded parser turns a one-line inventory
// into gigabytes of host names.
//
// Unlike FindPlaybooks, which silently truncates at its cap, exceeding this
// limit is an error: a truncated host list is indistinguishable from a
// complete one, so a user who cannot find a host would conclude it does not
// exist instead of falling back to typing it by hand.
//
// The cap is the snapshot's own: what the parser accepts here is exactly what
// StoreResolved accepts from a runner, so the honest and the reported path
// cannot drift apart.
const maxInventoryHosts = db.MaxInventorySnapshotHosts

// maxInventoryDepth caps nesting of YAML group trees. The host budget above
// does not bound a deep tree that declares no hosts, and the walk is
// recursive.
const maxInventoryDepth = 64

// maxInventoryGroups caps how many distinct groups a single inventory may
// declare. The host budget bounds hosts, but a group carries a name and two
// membership maps of its own regardless of how many hosts it holds, so an
// inventory built mostly of empty or near-empty groups (":children" sections
// referencing one another, YAML groups with no "hosts" key) can grow memory
// without ever touching maxInventoryHosts.
//
// Matches db.MaxInventorySnapshotGroups so the honest parsing path and
// StoreResolved's runner-reported path accept exactly the same shape.
const maxInventoryGroups = db.MaxInventorySnapshotGroups

// maxInventoryMemberships caps the sum, across every group, of that group's
// transitive host count (its own hosts plus every descendant group's). build()
// materialises one such set per group, and a hierarchy where many groups share
// a large descendant subtree multiplies that sum well past the number of
// hosts or groups actually declared: 200 groups each resolving to the same
// 20000-host subtree is 4,000,000 membership rows from an inventory that
// never came close to either budget above.
//
// Matches db.MaxInventorySnapshotMemberships for the same reason as
// maxInventoryGroups.
const maxInventoryMemberships = db.MaxInventorySnapshotMemberships

// maxHostPatternRanges caps how many bracketed ranges one host entry may carry.
// expandHostPattern recurses once per range and rebuilds the whole pattern at
// every level, so the cost grows quadratically with the number of brackets
// while no budget above is spent at all: "h[0:0][0:0]..." expands to exactly
// one host however many groups it holds. Measured on this parser, 50000 groups
// on one line cost 4.9 s of CPU and 200000 exhausted memory. Real inventories
// use one or two ranges.
const maxHostPatternRanges = 8

// maxHostPatternLength caps the length of a single host entry before expansion,
// for the same reason: the entry is copied at every recursion level. A stored
// host name may not exceed db.MaxInventoryHostNameLength anyway, and a range
// pattern only adds its bounds on top of that.
const maxHostPatternLength = 1024

// asciiLetters mirrors Python's string.ascii_letters, which Ansible indexes to
// resolve alphabetic range bounds. Lowercase precedes uppercase there, so
// [x:C] runs x, y, z, A, B, C.
const asciiLetters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

const (
	// allGroup is Ansible's implicit root group. It is walked like any other
	// group but never emitted: its host set is exactly InventoryHosts.Hosts.
	allGroup = "all"

	// ungroupedGroup is Ansible's implicit group holding every host that
	// belongs to no other group.
	ungroupedGroup = "ungrouped"
)

// InventoryTooLargeError reports that an inventory expands past a bound the
// parser is willing to materialise — hosts, groups, or group memberships.
// Callers are expected to fall back to free-form host entry rather than to
// show a partial list.
type InventoryTooLargeError struct {
	Limit int
	What  string
}

func (e InventoryTooLargeError) Error() string {
	return fmt.Sprintf("inventory expands to more than %d %s", e.Limit, e.What)
}

// InventoryGroup is a group declared in an inventory.
type InventoryGroup struct {
	Name string `json:"name"`

	// Hosts is the transitive host set: hosts declared in the group itself
	// plus those of every descendant group, which is what `--limit <group>`
	// actually targets.
	Hosts []string `json:"hosts"`

	// Children holds the directly nested group names, not their descendants.
	Children []string `json:"children"`
}

// InventoryHosts is the flattened view of an inventory.
//
// The implicit "all" group is not emitted, because it is exactly Hosts. The
// implicit "ungrouped" group is emitted when non-empty, so that the caller
// does not have to recompute it.
type InventoryHosts struct {
	Hosts  []string         `json:"hosts"`
	Groups []InventoryGroup `json:"groups"`
}

// ParseInventory parses the stored content of a static inventory into its host
// and group sets.
func ParseInventory(inventoryType db.InventoryType, content string) (InventoryHosts, error) {
	switch inventoryType {
	case db.InventoryStatic:
		return ParseINIInventory(content)
	case db.InventoryStaticYaml:
		return ParseYAMLInventory(content)
	default:
		return InventoryHosts{}, fmt.Errorf("inventory of type %s holds no parseable host list", inventoryType)
	}
}

// ParseINIInventory parses an Ansible INI-format inventory.
func ParseINIInventory(content string) (InventoryHosts, error) {
	builder := newInventoryBuilder()

	if err := feedINIInventory(builder, content); err != nil {
		return InventoryHosts{}, err
	}

	return builder.build()
}

// feedINIInventory adds one INI document to an existing builder. A directory
// inventory is many documents over one builder, so that groups defined across
// files merge and the host budget covers the whole source.
func feedINIInventory(builder *inventoryBuilder, content string) error {
	// Hosts written before the first section header belong to no group.
	group := ""
	kind := iniSectionHosts

	for _, raw := range strings.Split(content, "\n") {
		// TrimSpace also removes the carriage return of a CRLF inventory,
		// which would otherwise become part of the host name.
		line := strings.TrimSpace(raw)

		// Ansible treats only a leading # or ; as a comment. A trailing one
		// stays on the line, where it lands among the host variables that
		// this parser discards anyway.
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if body, ok := iniSectionHeader(line); ok {
			group, kind = parseINISectionHeader(body)

			if err := builder.declareGroup(group); err != nil {
				return err
			}

			continue
		}

		fields := strings.Fields(line)

		switch kind {
		case iniSectionHosts:
			// The rest of the line is host variables (host1 ansible_host=...).
			if err := builder.addHostPattern(group, fields[0]); err != nil {
				return err
			}
		case iniSectionChildren:
			if err := builder.addChild(group, fields[0]); err != nil {
				return err
			}
		case iniSectionVars:
			// A [group:vars] section declares variables, never hosts.
		}
	}

	return nil
}

// ParseYAMLInventory parses an Ansible YAML-format inventory.
func ParseYAMLInventory(content string) (InventoryHosts, error) {
	builder := newInventoryBuilder()

	if err := feedYAMLInventory(builder, content); err != nil {
		return InventoryHosts{}, err
	}

	return builder.build()
}

// feedYAMLInventory adds one YAML document to an existing builder.
func feedYAMLInventory(builder *inventoryBuilder, content string) error {
	var root any

	if err := yaml.Unmarshal([]byte(content), &root); err != nil {
		return err
	}

	mapping, ok := normalizeMapping(root)
	if !ok {
		// An empty document decodes to nil, which is a valid inventory holding
		// nothing.
		if root == nil {
			return nil
		}

		return fmt.Errorf("inventory root must be a mapping of group names")
	}

	return feedYAMLMapping(builder, mapping)
}

// feedYAMLMapping adds an already decoded YAML inventory. A directory walk
// decodes the document once to recognise the format and hands the result here,
// rather than unmarshalling every file of the directory twice.
func feedYAMLMapping(builder *inventoryBuilder, mapping map[string]any) error {
	for name, node := range mapping {
		if err := builder.declareGroup(name); err != nil {
			return err
		}

		if err := builder.addYAMLGroup(name, node, 0); err != nil {
			return err
		}
	}

	return nil
}

type iniSectionKind int

const (
	iniSectionHosts iniSectionKind = iota
	iniSectionChildren
	iniSectionVars
)

// iniSectionHeader reports the body of an INI section header when the line is
// one. Ansible's ini plugin matches ^\[([^:\]\s]+)(?::(\w+))?\]\s*(?:\#.*)?$,
// so a trailing comment belongs to the header line rather than to the section
// it opens. Requiring the line to end with "]" instead would read "[web] # prod"
// as a host literally named "[web]", drop the group, and send every host below
// it to "ungrouped".
func iniSectionHeader(line string) (string, bool) {
	if !strings.HasPrefix(line, "[") {
		return "", false
	}

	closing := strings.Index(line, "]")
	if closing < 0 {
		return "", false
	}

	rest := strings.TrimSpace(line[closing+1:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return "", false
	}

	return line[1:closing], true
}

// parseINISectionHeader splits a section header body into a group name and the
// kind of entries the section holds. A suffix other than :vars or :children
// stays part of the group name: Ansible rejects such a header, and inventing a
// group from a typo would hide the mistake instead of showing it.
func parseINISectionHeader(body string) (string, iniSectionKind) {
	body = strings.TrimSpace(body)

	switch {
	case strings.HasSuffix(body, ":vars"):
		return strings.TrimSuffix(body, ":vars"), iniSectionVars
	case strings.HasSuffix(body, ":children"):
		return strings.TrimSuffix(body, ":children"), iniSectionChildren
	default:
		return body, iniSectionHosts
	}
}

// inventoryBuilder accumulates hosts and group membership while a document is
// walked, and resolves nesting once at the end.
type inventoryBuilder struct {
	hosts         map[string]bool
	groupHosts    map[string]map[string]bool
	groupChildren map[string]map[string]bool

	// budget is how many more host entries may be materialised. It counts
	// generated names rather than distinct ones, so that a pattern repeating
	// the same host cannot be used to do unbounded work.
	budget int

	// groupBudget is how many more distinct groups may be declared. Unlike
	// budget, it is spent once per name, the first time that name is seen: a
	// group referenced repeatedly (":children" sections, a YAML host nested
	// under many parents) costs nothing on the repeat visits.
	groupBudget int
}

func newInventoryBuilder() *inventoryBuilder {
	return &inventoryBuilder{
		hosts:         map[string]bool{},
		groupHosts:    map[string]map[string]bool{},
		groupChildren: map[string]map[string]bool{},
		budget:        maxInventoryHosts,
		groupBudget:   maxInventoryGroups,
	}
}

// declareGroup records a group that exists but may hold nothing yet, which is
// how a group referenced only from a :children section or carrying only
// variables still reaches the output.
func (b *inventoryBuilder) declareGroup(name string) error {
	if name == "" || name == allGroup {
		return nil
	}

	if _, ok := b.groupHosts[name]; !ok {
		if b.groupBudget <= 0 {
			return InventoryTooLargeError{Limit: maxInventoryGroups, What: "groups"}
		}

		b.groupBudget--
		b.groupHosts[name] = map[string]bool{}
	}

	if _, ok := b.groupChildren[name]; !ok {
		b.groupChildren[name] = map[string]bool{}
	}

	return nil
}

// addHostPattern expands pattern and records the resulting hosts. An empty
// group name, or "all", records them without group membership, which is how
// hosts written before the first INI header and directly under YAML "all"
// arrive.
func (b *inventoryBuilder) addHostPattern(group string, pattern string) error {
	names, err := expandHostPattern(stripHostPort(pattern), b.budget)
	if err != nil {
		return err
	}

	b.budget -= len(names)

	for _, name := range names {
		b.hosts[name] = true

		if group == "" || group == allGroup {
			continue
		}

		if err := b.declareGroup(group); err != nil {
			return err
		}

		b.groupHosts[group][name] = true
	}

	return nil
}

// addHost records a name that is already resolved and takes it literally.
// Ansible has expanded any ranges by the time it reports a host, so running the
// name through pattern expansion again could only corrupt one that happens to
// contain brackets.
func (b *inventoryBuilder) addHost(group string, name string) error {
	if b.budget <= 0 {
		return InventoryTooLargeError{Limit: maxInventoryHosts, What: "hosts"}
	}

	b.budget--
	b.hosts[name] = true

	if group == "" || group == allGroup {
		return nil
	}

	if err := b.declareGroup(group); err != nil {
		return err
	}

	b.groupHosts[group][name] = true

	return nil
}

func (b *inventoryBuilder) addChild(parent string, child string) error {
	if err := b.declareGroup(child); err != nil {
		return err
	}

	if parent == "" || parent == allGroup {
		return nil
	}

	if err := b.declareGroup(parent); err != nil {
		return err
	}

	b.groupChildren[parent][child] = true

	return nil
}

// addYAMLGroup records the hosts and children of one group node.
func (b *inventoryBuilder) addYAMLGroup(name string, node any, depth int) error {
	if depth > maxInventoryDepth {
		return fmt.Errorf("inventory groups are nested deeper than %d levels", maxInventoryDepth)
	}

	// An empty group ("web:") decodes to nil, which is valid.
	body, ok := normalizeMapping(node)
	if !ok {
		return nil
	}

	for key, value := range body {
		switch key {
		case "hosts":
			for _, pattern := range hostPatternsOf(value) {
				if err := b.addHostPattern(name, pattern); err != nil {
					return err
				}
			}
		case "children":
			children, ok := normalizeMapping(value)
			if !ok {
				continue
			}

			for child, childNode := range children {
				if err := b.addChild(name, child); err != nil {
					return err
				}

				if err := b.addYAMLGroup(child, childNode, depth+1); err != nil {
					return err
				}
			}
		default:
			// "vars" and any other key carry no hosts.
		}
	}

	return nil
}

// resolveHosts collects the transitive host set of a group.
//
// A recursive definition ([a:children] b together with [b:children] a) is
// broken rather than reported: Ansible rejects it, but a picker showing a
// partial host list is more useful than one showing nothing because of a
// single bad line.
func (b *inventoryBuilder) resolveHosts(group string, into map[string]bool, visited map[string]bool) {
	if visited[group] {
		return
	}

	visited[group] = true

	for host := range b.groupHosts[group] {
		into[host] = true
	}

	for child := range b.groupChildren[group] {
		b.resolveHosts(child, into, visited)
	}
}

// build flattens every group's transitive host set and reports the result, or
// an error if that flattening runs past the membership budget first.
//
// The budget is charged as each group's set is materialised, group by group,
// rather than after all of them are built: a hierarchy crafted to blow the
// budget would otherwise force every one of its (possibly enormous) sets into
// memory before the check ever ran.
func (b *inventoryBuilder) build() (InventoryHosts, error) {
	resolved := make(map[string]map[string]bool, len(b.groupHosts))
	grouped := map[string]bool{}
	memberships := 0

	for name := range b.groupHosts {
		hosts := map[string]bool{}
		b.resolveHosts(name, hosts, map[string]bool{})

		memberships += len(hosts)
		if memberships > maxInventoryMemberships {
			return InventoryHosts{}, InventoryTooLargeError{Limit: maxInventoryMemberships, What: "group memberships"}
		}

		resolved[name] = hosts

		for host := range hosts {
			grouped[host] = true
		}
	}

	for host := range b.hosts {
		if grouped[host] {
			continue
		}

		if resolved[ungroupedGroup] == nil {
			resolved[ungroupedGroup] = map[string]bool{}
		}

		resolved[ungroupedGroup][host] = true
		memberships++
	}

	if memberships > maxInventoryMemberships {
		return InventoryHosts{}, InventoryTooLargeError{Limit: maxInventoryMemberships, What: "group memberships"}
	}

	result := InventoryHosts{
		Hosts:  sortedKeys(b.hosts),
		Groups: make([]InventoryGroup, 0, len(resolved)),
	}

	for name, hosts := range resolved {
		result.Groups = append(result.Groups, InventoryGroup{
			Name:     name,
			Hosts:    sortedKeys(hosts),
			Children: sortedKeys(b.groupChildren[name]),
		})
	}

	sort.Slice(result.Groups, func(i, j int) bool {
		return result.Groups[i].Name < result.Groups[j].Name
	})

	return result, nil
}

// expandHostPattern expands the range patterns of a host entry into concrete
// host names, mirroring Ansible's expand_hostname_range. Only the first range
// is expanded per call; any further one is reached through the recursion, so
// web[1:2]-[a:b] yields all four combinations.
func expandHostPattern(pattern string, budget int) ([]string, error) {
	if budget <= 0 {
		return nil, InventoryTooLargeError{Limit: maxInventoryHosts, What: "hosts"}
	}

	// Both caps are re-checked on every recursion level, which is cheap: each
	// level removes exactly one bracket group, so the count only falls.
	if len(pattern) > maxHostPatternLength {
		return nil, fmt.Errorf("host entry is longer than %d characters", maxHostPatternLength)
	}

	if strings.Count(pattern, "[") > maxHostPatternRanges {
		return nil, fmt.Errorf("host entry carries more than %d range patterns", maxHostPatternRanges)
	}

	if !isHostRange(pattern) {
		return []string{pattern}, nil
	}

	open := strings.Index(pattern, "[")
	closing := strings.Index(pattern, "]")

	head := pattern[:open]
	tail := pattern[closing+1:]

	sequence, err := rangeSequence(pattern[open+1:closing], budget)
	if err != nil {
		return nil, err
	}

	result := make([]string, 0, len(sequence))

	for _, token := range sequence {
		expanded, err := expandHostPattern(head+token+tail, budget-len(result))
		if err != nil {
			return nil, err
		}

		result = append(result, expanded...)
	}

	return result, nil
}

// isHostRange mirrors Ansible's detect_range: the first "[", ":" and "]" of
// the pattern must appear in that order.
//
// A bracketed IPv6 address such as [fe80::1] therefore reads as a range and
// fails the whole parse on its malformed bounds. That is Ansible's own
// verdict for such a line — its detect_range fires on the same three
// characters — and matching it is worth more than accepting an address the
// play itself would then trip over.
func isHostRange(pattern string) bool {
	open := strings.Index(pattern, "[")
	colon := strings.Index(pattern, ":")
	closing := strings.Index(pattern, "]")

	return open >= 0 && colon >= 0 && closing >= 0 && open < colon && colon < closing
}

// rangeSequence expands the body of a host range ("01:50", "a:f", "1:10:2")
// into its tokens.
func rangeSequence(body string, budget int) ([]string, error) {
	bounds := strings.Split(body, ":")
	if len(bounds) != 2 && len(bounds) != 3 {
		return nil, fmt.Errorf("host range must be begin:end or begin:end:step, got %q", body)
	}

	begin, end := bounds[0], bounds[1]
	step := 1

	if len(bounds) == 3 {
		parsed, err := strconv.Atoi(bounds[2])
		if err != nil || parsed < 1 {
			return nil, fmt.Errorf("host range step must be a positive number, got %q", bounds[2])
		}

		step = parsed
	}

	if begin == "" {
		begin = "0"
	}

	if end == "" {
		return nil, fmt.Errorf("host range must specify an end value, got %q", body)
	}

	if isAlphaBound(begin) && isAlphaBound(end) {
		return alphaSequence(begin, end, step)
	}

	return numericSequence(begin, end, step, budget)
}

// isAlphaBound reports whether a range bound is a single ASCII letter.
//
// Ansible resolves alphabetic bounds with string.ascii_letters.index(), which
// is a substring search, so a multi-character bound such as [ab:cd] silently
// expands to a, b, c. This parser declines to expand those: a host picker that
// invents names nobody wrote is worse than one that reports a broken range.
func isAlphaBound(bound string) bool {
	return len(bound) == 1 && strings.Contains(asciiLetters, bound)
}

func alphaSequence(begin string, end string, step int) ([]string, error) {
	first := strings.Index(asciiLetters, begin)
	last := strings.Index(asciiLetters, end)

	if first > last {
		return nil, fmt.Errorf("host range must have begin <= end, got %q", begin+":"+end)
	}

	result := make([]string, 0, (last-first)/step+1)

	for i := first; i <= last; i += step {
		result = append(result, string(asciiLetters[i]))
	}

	return result, nil
}

func numericSequence(begin string, end string, step int, budget int) ([]string, error) {
	first, err := strconv.Atoi(begin)
	if err != nil {
		return nil, fmt.Errorf("host range bound %q is neither a number nor a single letter", begin)
	}

	last, err := strconv.Atoi(end)
	if err != nil {
		return nil, fmt.Errorf("host range bound %q is neither a number nor a single letter", end)
	}

	// Negative bounds would make the span arithmetic below overflow, and no
	// host name carries a negative index.
	if first < 0 || last < 0 {
		return nil, fmt.Errorf("host range bounds must not be negative, got %q", begin+":"+end)
	}

	if last < first {
		// Python's range() yields nothing here, so the pattern matches no host.
		return nil, nil
	}

	// Counted before the tokens are built: the whole point of the budget is to
	// refuse [1:1000000] without first allocating a million strings.
	//
	// The span is compared against the budget before the +1 that turns it into
	// a count, because the bounds are user input: for [0:9223372036854775807]
	// the +1 overflows to a negative count, which passes "count > budget" and
	// then panics in make().
	span := (last - first) / step
	if span >= budget {
		return nil, InventoryTooLargeError{Limit: maxInventoryHosts, What: "hosts"}
	}

	count := span + 1

	// A leading zero in the begin bound is Ansible's signal to zero-pad, and
	// both bounds must then be written to the same width.
	width := 0

	if len(begin) > 1 && begin[0] == '0' {
		if len(begin) != len(end) {
			return nil, fmt.Errorf("host range must specify equal-length begin and end formats, got %q", begin+":"+end)
		}

		width = len(begin)
	}

	result := make([]string, 0, count)

	// Indexed rather than accumulated: "value += step" wraps around for a range
	// ending near math.MaxInt64, and a wrapped value stays <= last forever, so
	// the loop would never end while appending a name on every turn.
	for i := 0; i < count; i++ {
		value := first + i*step

		if width > 0 {
			result = append(result, fmt.Sprintf("%0*d", width, value))
		} else {
			result = append(result, strconv.Itoa(value))
		}
	}

	return result, nil
}

// stripHostPort removes the ":port" suffix of a host entry, mirroring Ansible's
// parse_address. A play addresses the host by its name alone, so a snapshot
// that keeps the port would produce a --limit matching nothing.
//
// An unbracketed IPv6 address carries colons of its own and takes no port in
// Ansible either, which is why the split only fires when the name part holds no
// colon outside brackets.
func stripHostPort(entry string) string {
	colon := strings.LastIndex(entry, ":")
	if colon < 0 {
		return entry
	}

	port := entry[colon+1:]
	if port == "" || strings.ContainsFunc(port, func(r rune) bool { return r < '0' || r > '9' }) {
		return entry
	}

	name := entry[:colon]
	if hasColonOutsideBrackets(name) {
		return entry
	}

	return name
}

func hasColonOutsideBrackets(name string) bool {
	depth := 0

	for _, char := range name {
		switch char {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case ':':
			if depth == 0 {
				return true
			}
		}
	}

	return false
}

// normalizeMapping accepts both shapes yaml.v3 produces for a mapping decoded
// into any: map[string]any when every key is a string, and map[any]any when
// some key is not, which happens for a host named 10 or 2001.
func normalizeMapping(node any) (map[string]any, bool) {
	switch typed := node.(type) {
	case map[string]any:
		return typed, true
	case map[any]any:
		result := make(map[string]any, len(typed))

		for key, value := range typed {
			result[fmt.Sprint(key)] = value
		}

		return result, true
	default:
		return nil, false
	}
}

// hostPatternsOf reads the patterns of a YAML "hosts" node. The Ansible YAML
// inventory plugin requires a mapping, but a plain sequence is common enough
// in hand-written inventories to be worth accepting.
func hostPatternsOf(node any) []string {
	if mapping, ok := normalizeMapping(node); ok {
		patterns := make([]string, 0, len(mapping))

		for key := range mapping {
			patterns = append(patterns, key)
		}

		return patterns
	}

	sequence, ok := node.([]any)
	if !ok {
		return nil
	}

	patterns := make([]string, 0, len(sequence))

	for _, item := range sequence {
		// Only a scalar can name a host. fmt.Sprint of a nested mapping yields
		// "map[name:x ansible_password:y]", which would file variable values
		// into a host list that every project member can read.
		switch item.(type) {
		case nil, map[string]any, map[any]any, []any:
			continue
		}

		patterns = append(patterns, fmt.Sprint(item))
	}

	return patterns
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))

	for key := range set {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}
