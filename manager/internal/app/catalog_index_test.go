package app

import (
	"testing"

	"rcd_manager/internal/calculation"
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

func TestCentralCalculationCatalogReplacesLowerAndRetainsRemovedNodes(t *testing.T) {
	config, err := calculation.ParseConfig(`[[rules]]
kks_calc="CALC"
data_type="double"
expression="RAW + 1"`)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := calculation.Compile(config, map[string]calculation.InputDefinition{
		"RAW": {DataType: domain.DataTypeDouble, ValueRank: -1},
	})
	if err != nil {
		t.Fatal(err)
	}
	catalogs := map[string]domain.SourceCatalog{"s1": {
		SourceID: "s1", GroupID: "g", State: domain.CatalogReady,
		Nodes: []domain.CatalogNode{
			{Kind: "raw", Key: domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "RAW"}, Class: domain.NodeClassVariable, DataType: domain.DataTypeDouble, ValueRank: -1},
			{Kind: "calculation", Key: domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "CALC"}, Class: domain.NodeClassVariable, DataType: domain.DataTypeDouble, ValueRank: -1},
		},
	}}
	retired := map[string]indexedPoint{"OLD": {
		GroupID: "g",
		Node:    domain.CatalogNode{Kind: "manager_calculation", Key: domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "OLD"}, Class: domain.NodeClassVariable, DataType: domain.DataTypeUInt32, ValueRank: -1, Retired: true},
	}}
	index, states, _, err := buildCatalogIndexWithCalculations(catalogs, plan, retired, []domain.Group{{ID: "g", DefaultSourceID: "s1"}})
	if err != nil {
		t.Fatal(err)
	}
	if states["s1"] != domain.CatalogReady {
		t.Fatalf("lower calculation should be ignored, state=%s", states["s1"])
	}
	if point := index.pointByNode["CALC"]; point.Node.Kind != "manager_calculation" || len(point.SourceIDs) != 0 {
		t.Fatalf("central point not injected: %#v", point)
	}
	if point := index.pointByNode["OLD"]; point.Node.Kind != "manager_calculation" || !point.Node.Retired {
		t.Fatalf("retired point not retained: %#v", point)
	}
	if index.sourceHasNode("s1", "CALC") {
		t.Fatal("filtered lower calculation remained in source catalog")
	}
}

func TestCentralCalculationOutputQuarantinesLaterRawCollision(t *testing.T) {
	config, err := calculation.ParseConfig(`[[rules]]
kks_calc="CALC"
data_type="double"
expression="1"`)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := calculation.Compile(config, map[string]calculation.InputDefinition{})
	if err != nil {
		t.Fatal(err)
	}
	catalogs := map[string]domain.SourceCatalog{"s1": {
		SourceID: "s1", GroupID: "g", State: domain.CatalogReady,
		Nodes: []domain.CatalogNode{{Kind: "raw", Key: domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "CALC"}, Class: domain.NodeClassVariable, DataType: domain.DataTypeDouble, ValueRank: -1}},
	}}
	index, states, conflicts, err := buildCatalogIndexWithCalculations(catalogs, plan, nil, []domain.Group{{ID: "g", DefaultSourceID: "s1"}})
	if err != nil {
		t.Fatal(err)
	}
	if states["s1"] != domain.CatalogConflict || conflicts == 0 || index.pointByNode["CALC"].Node.Kind != "manager_calculation" {
		t.Fatalf("raw collision was not quarantined: state=%s conflicts=%d point=%#v", states["s1"], conflicts, index.pointByNode["CALC"])
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
