package calculation

import (
	"rcd_manager/internal/domain"
	"reflect"
	"strings"
	"testing"
)

const stateFixture = `[[state_inputs]]
name = "STATION"
mode = "fixed"
[[state_inputs.targets]]
source_id = "a"
port_id = "/dev/ttyWCH0"
device_id = "11"
[[rules]]
kks_calc = "ALARM"
data_type = "UInt32"
expression = "IF(STATION == 1, 0, 1)"
`

func TestStateInputCompileAndQuality(t *testing.T) {
	config, err := ParseConfig(stateFixture)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(config, map[string]InputDefinition{})
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(plan)
	for state := uint32(0); state <= 5; state++ {
		runner.Reset(map[string]Input{"STATION": {Type: domain.DataTypeUInt32, Value: state, StatusCode: domain.StatusGood}}, 1, 1)
		got := runner.Results()[0]
		want := uint32(1)
		if state == 1 {
			want = 0
		}
		if got.Value != want || got.StatusCode != domain.StatusGood {
			t.Fatalf("state %d: %#v", state, got)
		}
	}
	if _, err = Compile(config, map[string]InputDefinition{"STATION": {DataType: domain.DataTypeUInt32, ValueRank: -1}}); err == nil {
		t.Fatal("collision allowed")
	}
	bad := config
	bad.Rules = append([]Rule(nil), config.Rules...)
	bad.Rules[0].Output = "STATION"
	if _, err = Compile(bad, nil); err == nil {
		t.Fatal("output alias collision allowed")
	}
	copy := plan.StateInputs()
	copy[0].Targets[0].PortID = "changed"
	if plan.StateInputs()[0].Targets[0].PortID == "changed" {
		t.Fatal("mutable plan")
	}
}
func TestStateInputConfigValidationAndRoundTrip(t *testing.T) {
	for _, bad := range []string{
		strings.Replace(stateFixture, `name = "STATION"`, `name = "IF"`, 1),
		strings.Replace(stateFixture, `mode = "fixed"`, `mode = "other"`, 1),
		strings.Replace(stateFixture, `device_id = "11"`, `device_id = ""`, 1),
		strings.Replace(stateFixture, `port_id =`, `port_typo =`, 1),
	} {
		if _, err := ParseConfig(bad); err == nil {
			t.Fatal("invalid config accepted", bad)
		}
	}
	config, _ := ParseConfig(stateFixture)
	replaced, err := ReplaceStateInputs(stateFixture, config.StateInputs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseConfig(replaced)
	if err != nil || !reflect.DeepEqual(config.Rules, got.Rules) || !reflect.DeepEqual(config.StateInputs, got.StateInputs) {
		t.Fatalf("round trip: %s %v", replaced, err)
	}
	without, err := ReplaceStateInputs(stateFixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = ParseConfig(without)
	if len(got.StateInputs) != 0 || !reflect.DeepEqual(got.Rules, config.Rules) {
		t.Fatal("removal changed rules")
	}
	plan, _ := Compile(config, nil)
	config.StateInputs[0].Targets[0].PortID = "COM2"
	next, _ := Compile(config, nil)
	if ComparePlans(plan, next).Changed != 1 {
		t.Fatal("mapping changes missing from version diff")
	}
}
