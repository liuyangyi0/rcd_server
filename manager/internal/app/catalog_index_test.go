package app

import (
	"testing"

	"rcd_manager/internal/domain"
)

func TestCatalogIndexTracksSourcePresence(t *testing.T) {
	key := domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "A"}
	index, err := buildCatalogIndex([]domain.SourceCatalog{
		{SourceID: "s1", GroupID: "g", State: domain.CatalogReady, Nodes: []domain.CatalogNode{{Key: key, Class: domain.NodeClassVariable, DataType: domain.DataTypeUInt32, ValueRank: -1}}},
		{SourceID: "s2", GroupID: "g", State: domain.CatalogReady},
	})
	if err != nil {
		t.Fatalf("buildCatalogIndex() error = %v", err)
	}
	if !index.sourceHasNode("s1", "A") || index.sourceHasNode("s2", "A") {
		t.Fatalf("unexpected source presence: %#v", index.sourceNodes)
	}
	if group, ok := index.groupForNode("A"); !ok || group != "g" {
		t.Fatalf("groupForNode() = %q, %v", group, ok)
	}
}

func TestCatalogIndexUnionRetiresOnlyWhenEverySourceRetires(t *testing.T) {
	key := domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "KKS"}
	for name, catalogs := range map[string][]domain.SourceCatalog{
		"retired source sorts first": {
			{SourceID: "a-retired", GroupID: "g", State: domain.CatalogReady, Nodes: []domain.CatalogNode{{Key: key, Class: domain.NodeClassVariable, DataType: domain.DataTypeDouble, ValueRank: -1, Retired: true}}},
			{SourceID: "b-active", GroupID: "g", State: domain.CatalogReady, Nodes: []domain.CatalogNode{{Key: key, Class: domain.NodeClassVariable, DataType: domain.DataTypeDouble, ValueRank: -1}}},
		},
		"retired source sorts last": {
			{SourceID: "a-active", GroupID: "g", State: domain.CatalogReady, Nodes: []domain.CatalogNode{{Key: key, Class: domain.NodeClassVariable, DataType: domain.DataTypeDouble, ValueRank: -1}}},
			{SourceID: "b-retired", GroupID: "g", State: domain.CatalogReady, Nodes: []domain.CatalogNode{{Key: key, Class: domain.NodeClassVariable, DataType: domain.DataTypeDouble, ValueRank: -1, Retired: true}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			index, err := buildCatalogIndex(catalogs)
			if err != nil {
				t.Fatal(err)
			}
			if node := index.union.Groups["g"]["KKS"]; node.Retired {
				t.Fatalf("union node retired while a source is active: %#v", node)
			}
			if point := index.pointByNode["KKS"]; point.Node.Retired {
				t.Fatalf("indexed point retired while a source is active: %#v", point)
			}
		})
	}
}
