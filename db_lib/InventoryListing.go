package db_lib

import (
	"encoding/json"
	"fmt"
)

// maxInventoryListingBytes bounds the output of a dynamic inventory before it
// is parsed. The listing comes from a program in somebody's repository, and a
// cloud inventory of any size carries a variable block per host.
const maxInventoryListingBytes = 32 << 20

// metaListingKey is the entry ansible-inventory uses for data that is not a
// group. It holds hostvars — every variable of every host, group variables
// included, which is where passwords and tokens live.
const metaListingKey = "_meta"

// inventoryListingGroup is a group as `ansible-inventory --list` reports one.
// Only the two membership fields are decoded; "vars" is deliberately absent, so
// that variables have nowhere to land even by accident.
type inventoryListingGroup struct {
	Hosts    []string `json:"hosts"`
	Children []string `json:"children"`
}

// ParseInventoryListing reduces the output of `ansible-inventory --list` to the
// host and group names it describes.
//
// The reduction exists to be run where the listing is produced. The raw output
// carries every host variable — ansible_password among them — while a snapshot
// is readable by any member of the project, so the variables must be dropped
// before the listing travels rather than after it arrives.
func ParseInventoryListing(output []byte) (InventoryHosts, error) {
	if len(output) > maxInventoryListingBytes {
		return InventoryHosts{}, fmt.Errorf(
			"inventory listing is larger than %d bytes",
			maxInventoryListingBytes,
		)
	}

	var listing map[string]json.RawMessage

	if err := json.Unmarshal(output, &listing); err != nil {
		return InventoryHosts{}, fmt.Errorf("inventory listing is not valid JSON: %w", err)
	}

	builder := newInventoryBuilder()

	for name, raw := range listing {
		if name == metaListingKey {
			continue
		}

		var group inventoryListingGroup

		if err := json.Unmarshal(raw, &group); err != nil {
			return InventoryHosts{}, fmt.Errorf("group %q in the inventory listing is malformed: %w", name, err)
		}

		if err := builder.declareGroup(name); err != nil {
			return InventoryHosts{}, err
		}

		for _, host := range group.Hosts {
			if err := builder.addHost(name, host); err != nil {
				return InventoryHosts{}, err
			}
		}

		for _, child := range group.Children {
			if err := builder.addChild(name, child); err != nil {
				return InventoryHosts{}, err
			}
		}
	}

	return builder.build()
}
