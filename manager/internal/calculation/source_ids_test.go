package calculation

import (
	"reflect"
	"testing"
)

func TestRemapRoutingIDsOnlyTouchesRouting(t *testing.T) {
	content := `# real-lower in comments is not an identifier
[[state_inputs]]
name="SOURCE_STATE"
mode="group_default"
group_id="real-hardware"
[[state_inputs.targets]]
source_id="real-lower"
port_id="real-lower"
device_id="11"
[[rules]]
kks_calc="real-lower"
data_type="UInt32"
expression="SOURCE_STATE"
`
	before, _ := ParseConfig(content)
	updated, changed, err := RemapRoutingIDs(content, map[string]string{"real-lower": "rcd-46"}, map[string]string{"real-hardware": "serial-acquisition"})
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	after, err := ParseConfig(updated)
	if err != nil || !reflect.DeepEqual(before.Rules, after.Rules) || after.StateInputs[0].Targets[0].PortID != "real-lower" {
		t.Fatal("non-routing fields changed", after, err)
	}
	if after.StateInputs[0].Targets[0].SourceID != "rcd-46" || after.StateInputs[0].GroupID != "serial-acquisition" {
		t.Fatal(after)
	}
	if got, changed, err := RemapRoutingIDs(updated, map[string]string{"real-lower": "rcd-46"}, nil); err != nil || changed || got != updated {
		t.Fatal("not idempotent", err)
	}
}
