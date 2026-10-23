package calculation

import (
	"reflect"
	"strings"
	"testing"

	"rcd_manager/internal/domain"
)

func TestStateInputNamesMatchExpressionIdentifiers(t *testing.T) {
	for _, name := range []string{"STATION", "_STATE", "_", "9CYE91GH201_COM1", "11_STATE", "9_11", "123_", strings.Repeat("9", 127) + "A"} {
		t.Run(name, func(t *testing.T) {
			content := strings.ReplaceAll(stateFixture, "STATION", name)
			config, err := ParseConfig(content)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Compile(config, map[string]InputDefinition{}); err != nil {
				t.Fatalf("valid alias cannot be referenced: %v", err)
			}
			_, variables, err := parseExpression(name + " + 11")
			if err != nil || !reflect.DeepEqual(variables, []string{name}) {
				t.Fatalf("identifier or numeric literal changed: variables=%v err=%v", variables, err)
			}
		})
	}
	for _, name := range []string{"", "0", "11", "00011", strings.Repeat("9", 128), "IF", "if", "If", "true", "TRUE", "false", "False", "9CYE-STATE", "9CYE.STATE", "9CYE/STATE", "9CYE STATE", "9CYE\nSTATE", "9CYE状态", strings.Repeat("9", 128) + "A"} {
		t.Run("reject_"+name, func(t *testing.T) {
			input := StateInput{Name: name, Mode: "fixed", Targets: []StateTarget{{SourceID: "a", PortID: "COM1", DeviceID: "11"}}}
			if err := validateStateInputs([]StateInput{input}); err == nil {
				t.Fatalf("invalid alias accepted: %q", name)
			}
		})
	}
}

func TestDigitLeadingStateInputRoundTripEvaluationAndCollisions(t *testing.T) {
	const name = "9CYE91GH201_COM1"
	content := strings.ReplaceAll(stateFixture, "STATION", name) + `
[[rules]]
kks_calc = "COPY"
data_type = "UInt32"
expression = "9CYE91GH201_COM1"
[[rules]]
kks_calc = "ARITHMETIC"
data_type = "UInt32"
expression = "9CYE91GH201_COM1 + 11"
[[rules]]
kks_calc = "CONSTANT"
data_type = "UInt32"
expression = "11"
`
	config := testConfig(t, content)
	roundTrip, err := ReplaceStateInputs(content, config.StateInputs)
	if err != nil {
		t.Fatal(err)
	}
	restored := testConfig(t, roundTrip)
	if !reflect.DeepEqual(config.Rules, restored.Rules) || !reflect.DeepEqual(config.StateInputs, restored.StateInputs) {
		t.Fatal("round trip changed rules or state input name")
	}
	plan, err := Compile(restored, map[string]InputDefinition{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.StateDependencies("ALARM"), map[string]bool{name: true}) {
		t.Fatal("digit-leading state dependency was lost")
	}
	runner := NewRunner(plan)
	for state := uint32(0); state <= 5; state++ {
		runner.Reset(map[string]Input{name: {Type: domain.DataTypeUInt32, Value: state, StatusCode: domain.StatusGood}}, 1, 1)
		alarm := uint32(1)
		if state == 1 {
			alarm = 0
		}
		want := map[string]uint32{"ALARM": alarm, "COPY": state, "ARITHMETIC": state + 11, "CONSTANT": 11}
		for _, got := range runner.Results() {
			if got.Value != want[got.NodeID] || got.StatusCode != domain.StatusGood || got.Error != "" {
				t.Fatalf("state %d, result=%#v want=%d/Good", state, got, want[got.NodeID])
			}
		}
	}
	runner.Update(map[string]Input{name: {Type: domain.DataTypeUInt32, Value: uint32(2), StatusCode: domain.StatusGood}}, 1, 1)
	for _, got := range runner.Results() {
		if got.NodeID == "COPY" && got.Value != uint32(2) {
			t.Fatalf("incremental update did not resolve numeric-leading input: %#v", got)
		}
	}
	if _, err := Compile(config, map[string]InputDefinition{name: {DataType: domain.DataTypeUInt32, ValueRank: -1}}); err == nil {
		t.Fatal("raw point alias collision accepted")
	}
	outputCollision := config
	outputCollision.Rules = append([]Rule(nil), config.Rules...)
	outputCollision.Rules[0].Output = name
	if _, err := Compile(outputCollision, nil); err == nil {
		t.Fatal("output alias collision accepted")
	}
	duplicate := config
	duplicate.StateInputs = append(append([]StateInput(nil), config.StateInputs...), config.StateInputs[0])
	if err := validateStateInputs(duplicate.StateInputs); err == nil {
		t.Fatal("duplicate digit-leading alias accepted")
	}
}
