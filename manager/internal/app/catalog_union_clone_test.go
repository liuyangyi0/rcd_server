package app

import (
	"testing"

	"rcd_manager/internal/domain"
)

func TestCloneCatalogUnionPreservesIndependentSourceTopologies(t *testing.T) {
	original := domain.CatalogUnion{
		Groups:       map[string]map[string]domain.CatalogNode{"g": {}},
		SourceStates: map[string]domain.CatalogState{"source-a": domain.CatalogReady},
		SourceTopologies: map[string]*domain.SourceTopology{"source-a": {
			SchemaVersion: 1,
			Ports: []domain.TopologyPort{{
				PortID: "COM1", PortName: "COM1",
				Devices: []domain.TopologyDevice{{DeviceID: "1", DeviceAddress: 1, Name: "Device 1", NodeIDs: []string{"KKS"}}},
			}},
		}},
		SourceNodes: map[string]map[string]domain.CatalogNode{"source-a": {
			"COM1_SendCommand": {
				Key: domain.NodeKey{Identifier: "COM1_SendCommand"}, Class: domain.NodeClassMethod,
				InputArguments: []domain.Argument{{Name: "HexData", DataType: domain.DataTypeString, ValueRank: -1}},
			},
		}},
		Conflicts: []domain.CatalogConflictDetail{{SourceIDs: []string{"source-a", "source-b"}}},
	}
	clone := cloneCatalogUnion(original)
	if clone.SourceTopologies["source-a"] == nil || clone.SourceTopologies["source-a"].Ports[0].Devices[0].Name != "Device 1" {
		t.Fatalf("topology missing from catalog clone: %#v", clone.SourceTopologies)
	}
	clone.SourceTopologies["source-a"].Ports[0].Devices[0].Name = "Changed"
	clone.SourceTopologies["source-a"].Ports[0].Devices[0].NodeIDs[0] = "OTHER"
	clone.SourceNodes["source-a"]["COM1_SendCommand"].InputArguments[0].Name = "Changed"
	clone.Conflicts[0].SourceIDs[0] = "changed"
	if original.SourceTopologies["source-a"].Ports[0].Devices[0].Name != "Device 1" || original.SourceTopologies["source-a"].Ports[0].Devices[0].NodeIDs[0] != "KKS" {
		t.Fatal("catalog topology clone aliases the immutable runtime snapshot")
	}
	if original.SourceNodes["source-a"]["COM1_SendCommand"].InputArguments[0].Name != "HexData" || original.Conflicts[0].SourceIDs[0] != "source-a" {
		t.Fatal("catalog union clone aliases method arguments or conflict source IDs")
	}
}
