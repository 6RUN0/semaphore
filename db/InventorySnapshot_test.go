package db

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The groups arrive as a map, whose iteration order is random. This content is
// serialized into a runner's progress report, so without an order of its own
// two reports of the same inventory differ on the wire (review finding m4).
func TestNewInventorySnapshotContentOrdersGroupsByName(t *testing.T) {
	groups := map[string][]string{
		"web":  {"web01"},
		"all":  {"db01", "web01"},
		"db":   {"db01"},
		"prod": {"db01", "web01"},
	}

	// One shuffle is not proof: the same map is built repeatedly so that a
	// missing sort cannot pass by landing on the right order once.
	for i := 0; i < 20; i++ {
		content := NewInventorySnapshotContent([]string{"db01", "web01"}, groups)

		names := make([]string, 0, len(content.Groups))
		for _, group := range content.Groups {
			names = append(names, group.Name)
		}

		assert.Equal(t, []string{"all", "db", "prod", "web"}, names)
	}
}

func TestNewInventorySnapshotContentNeverHoldsNilSlices(t *testing.T) {
	content := NewInventorySnapshotContent(nil, map[string][]string{"empty": nil})

	assert.NotNil(t, content.Hosts)
	require.Len(t, content.Groups, 1)
	assert.NotNil(t, content.Groups[0].Hosts)
}

// This struct is the body a runner reports a resolved host list in, so its
// field names are a wire contract with every deployed runner. Renaming a field
// without keeping its tag would break that silently, in one direction only
// (review finding m5).
func TestInventorySnapshotContentPinsItsWireNames(t *testing.T) {
	encoded, err := json.Marshal(InventorySnapshotContent{
		Hosts:  []string{"web01"},
		Groups: []InventorySnapshotGroupContent{{Name: "web", Hosts: []string{"web01"}}},
	})

	require.NoError(t, err)
	assert.JSONEq(t,
		`{"Hosts":["web01"],"Groups":[{"Name":"web","Hosts":["web01"]}]}`,
		string(encoded))

	var decoded InventorySnapshotContent
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	assert.Equal(t, []string{"web01"}, decoded.Hosts)
	require.Len(t, decoded.Groups, 1)
	assert.Equal(t, "web", decoded.Groups[0].Name)
	assert.Equal(t, []string{"web01"}, decoded.Groups[0].Hosts)
}
