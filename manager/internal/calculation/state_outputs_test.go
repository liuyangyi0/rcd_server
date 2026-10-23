package calculation

import (
	"reflect"
	"strings"
	"testing"

	"rcd_manager/internal/domain"
)

func TestStateInputsPublishReadOnlyCatalogWithoutRules(t *testing.T) {
	content := strings.Split(stateFixture, "[[rules]]")[0]
	config := testConfig(t, content)
	plan, err := Compile(config, map[string]InputDefinition{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.RuleCount() != 0 || !reflect.DeepEqual(plan.OutputNames(), []string{"STATION"}) {
		t.Fatalf("state publication must not fabricate a formula: %v", plan.OutputNames())
	}
	nodes := plan.CatalogNodes()
	if len(nodes) != 1 {
		t.Fatal(nodes)
	}
	node := nodes[0]
	if node.Key.NamespaceURI != "urn:SerialServer" || node.Key.Identifier != "STATION" || node.BrowseName != "STATION" ||
		node.DataType != domain.DataTypeUInt32 || node.Writable || node.ValueRank != -1 || node.ParentPath != "Objects/Devices" {
		t.Fatal(node)
	}
	if kind, ok := plan.OutputDataType("STATION"); !ok || kind != domain.DataTypeUInt32 {
		t.Fatal(kind, ok)
	}
	withRule := testConfig(t, content+"\n[[rules]]\nkks_calc=\"STATION\"\ndata_type=\"UInt32\"\nexpression=\"1\"\n")
	if _, err := Compile(withRule, nil); err == nil || !strings.Contains(err.Error(), "state input OPC output") {
		t.Fatal("same-named formula must be rejected with a precise error", err)
	}
}
