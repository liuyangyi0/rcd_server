package domain

import (
	"errors"
	"testing"
)

func variable(id string, dataType DataType) CatalogNode {
	return CatalogNode{Key: NodeKey{NamespaceURI: "urn:SerialServer", Identifier: id}, BrowseName: id, Class: NodeClassVariable, DataType: dataType, ValueRank: -1}
}

func TestBuildCatalogUnionMergesCompatibleNodes(t *testing.T) {
	catalog, err := BuildCatalogUnion([]SourceCatalog{
		{SourceID: "a", GroupID: "g", State: CatalogReady, Nodes: []CatalogNode{variable("KKS", DataTypeUInt32)}},
		{SourceID: "b", GroupID: "g", State: CatalogCached, Nodes: []CatalogNode{variable("KKS", DataTypeUInt32), variable("ONLY_B", DataTypeFloat)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(catalog.Groups["g"]); got != 2 {
		t.Fatalf("union size = %d, want 2", got)
	}
	if len(catalog.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %#v", catalog.Conflicts)
	}
}

func TestBuildCatalogUnionPreservesExactWireIdentifier(t *testing.T) {
	node := variable("KKS_WITH_LEGACY_SPACE ", DataTypeUInt32)
	catalog, err := BuildCatalogUnion([]SourceCatalog{{
		SourceID: "a", GroupID: "g", State: CatalogReady, Nodes: []CatalogNode{node},
	}})
	if err != nil {
		t.Fatal(err)
	}
	actual, exists := catalog.Groups["g"][node.Key.Identifier]
	if !exists || actual.Key.Identifier != node.Key.Identifier {
		t.Fatalf("exact wire identifier was changed: groups=%#v", catalog.Groups["g"])
	}
	if _, normalized := catalog.Groups["g"]["KKS_WITH_LEGACY_SPACE"]; normalized {
		t.Fatal("trimmed identifier must not replace the lower OPC NodeID")
	}
}

func TestBuildCatalogUnionRejectsCrossGroupDuplicate(t *testing.T) {
	_, err := BuildCatalogUnion([]SourceCatalog{
		{SourceID: "a", GroupID: "g1", Nodes: []CatalogNode{variable("KKS", DataTypeUInt32)}},
		{SourceID: "b", GroupID: "g2", Nodes: []CatalogNode{variable("KKS", DataTypeUInt32)}},
	})
	var duplicate *CrossGroupDuplicateError
	if !errors.As(err, &duplicate) {
		t.Fatalf("error = %v, want CrossGroupDuplicateError", err)
	}
}

func TestBuildCatalogUnionMarksAllTypeConflictSources(t *testing.T) {
	for name, catalogs := range map[string][]SourceCatalog{
		"different shape in middle": {
			{SourceID: "a", GroupID: "g", State: CatalogReady, Nodes: []CatalogNode{variable("KKS", DataTypeUInt32)}},
			{SourceID: "b", GroupID: "g", State: CatalogReady, Nodes: []CatalogNode{variable("KKS", DataTypeDouble)}},
			{SourceID: "c", GroupID: "g", State: CatalogReady, Nodes: []CatalogNode{variable("KKS", DataTypeUInt32)}},
		},
		"different shape last": {
			{SourceID: "a", GroupID: "g", State: CatalogReady, Nodes: []CatalogNode{variable("KKS", DataTypeUInt32)}},
			{SourceID: "b", GroupID: "g", State: CatalogReady, Nodes: []CatalogNode{variable("KKS", DataTypeUInt32)}},
			{SourceID: "c", GroupID: "g", State: CatalogReady, Nodes: []CatalogNode{variable("KKS", DataTypeDouble)}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			catalog, err := BuildCatalogUnion(catalogs)
			if err != nil {
				t.Fatal(err)
			}
			for _, sourceID := range []string{"a", "b", "c"} {
				if catalog.SourceStates[sourceID] != CatalogConflict {
					t.Fatalf("states = %#v", catalog.SourceStates)
				}
			}
			if len(catalog.Conflicts) != 1 || len(catalog.Conflicts[0].SourceIDs) != 3 {
				t.Fatalf("conflicts = %#v", catalog.Conflicts)
			}
		})
	}
}

func TestBuildCatalogUnionRejectsInvalidCommandContracts(t *testing.T) {
	method := CatalogNode{
		Key: NodeKey{Identifier: "COM1_SendCommand"}, Class: NodeClassMethod,
		InputArguments:  []Argument{{Name: "Wrong", DataType: DataTypeString, ValueRank: -1}},
		OutputArguments: []Argument{{Name: "Result", DataType: DataTypeString, ValueRank: -1}},
	}
	send := variable("COM1_Send", DataTypeUInt32)
	catalog, err := BuildCatalogUnion([]SourceCatalog{{SourceID: "a", GroupID: "g", State: CatalogReady, Nodes: []CatalogNode{method, send}}})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.SourceStates["a"] != CatalogConflict || len(catalog.Conflicts) != 2 {
		t.Fatalf("catalog = %#v", catalog)
	}
}
