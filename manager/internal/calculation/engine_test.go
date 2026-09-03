package calculation

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func testConfig(t *testing.T, rules string) Config {
	t.Helper()
	config, err := ParseConfig(rules)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	return config
}

func scalarInputs(names ...string) map[string]InputDefinition {
	result := make(map[string]InputDefinition, len(names))
	for _, name := range names {
		result[name] = InputDefinition{DataType: domain.DataTypeDouble, ValueRank: -1}
	}
	return result
}

func TestParseConfigStrictSchemaAndLimits(t *testing.T) {
	valid := testConfig(t, `[[rules]]
kks_calc = "OUT"
data_type = "u32"
expression = "A + 1"
min_limit = 0
max_limit = 100
default_value = 7
`)
	if valid.Hash == "" || len(valid.Rules) != 1 || valid.Rules[0].DataType != domain.DataTypeUInt32 {
		t.Fatalf("unexpected parsed config: %#v", valid)
	}
	legacyName := testConfig(t, `[[rules]]
kks_calc = "LEGACY_TRAILING_SPACE "
data_type = "uint"
expression = "1"
`)
	if got := legacyName.Rules[0].Output; got != "LEGACY_TRAILING_SPACE " {
		t.Fatalf("legacy NodeId was normalized to %q", got)
	}
	for name, content := range map[string]string{
		"unknown root":    "unknown = 1",
		"unknown rule":    `[[rules]]\nkks_calc="X"\ndata_type="uint"\nexpression="1"\nextra=true`,
		"non finite":      `[[rules]]\nkks_calc="X"\ndata_type="float"\nexpression="1"\nmax_limit=nan`,
		"reversed limits": `[[rules]]\nkks_calc="X"\ndata_type="float"\nexpression="1"\nmin_limit=2\nmax_limit=1`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig(strings.ReplaceAll(content, `\n`, "\n")); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	if _, err := ParseConfig(strings.Repeat("#", MaxConfigBytes+1)); !errors.Is(err, ErrConfigTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
}

func TestCompileRejectsCatalogAndDependencyConflicts(t *testing.T) {
	tests := map[string]struct {
		config string
		inputs map[string]InputDefinition
	}{
		"duplicate output": {`[[rules]]
kks_calc="X"
data_type="uint"
expression="A"
[[rules]]
kks_calc="X"
data_type="uint"
expression="A"`, scalarInputs("A")},
		"self reference": {`[[rules]]
kks_calc="X"
data_type="uint"
expression="X + 1"`, nil},
		"cycle": {`[[rules]]
kks_calc="X"
data_type="uint"
expression="Y + 1"
[[rules]]
kks_calc="Y"
data_type="uint"
expression="X + 1"`, nil},
		"missing input": {`[[rules]]
kks_calc="X"
data_type="uint"
expression="MISSING + 1"`, scalarInputs("A")},
		"array input": {`[[rules]]
kks_calc="X"
data_type="uint"
expression="A + 1"`, map[string]InputDefinition{"A": {DataType: domain.DataTypeDouble, ValueRank: 1}}},
		"raw collision": {`[[rules]]
kks_calc="A"
data_type="uint"
expression="1"`, scalarInputs("A")},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Compile(testConfig(t, test.config), test.inputs)
			var invalid *ValidationError
			if !errors.As(err, &invalid) || len(invalid.Problems) == 0 {
				t.Fatalf("expected ValidationError, got %v", err)
			}
		})
	}
}

func TestRunnerMatchesExpressionSemanticsAndActualPathQuality(t *testing.T) {
	config := testConfig(t, `[[rules]]
kks_calc="SELECTED"
data_type="double"
expression="IF(READY == 1, ANALOG + 5, 4)"
[[rules]]
kks_calc="BITS"
data_type="uint"
expression="(MASK << 2) | 3"
[[rules]]
kks_calc="FLAG"
data_type="bool"
expression="SELECTED >= 4 && !DISABLED"`)
	plan, err := Compile(config, scalarInputs("READY", "ANALOG", "MASK", "DISABLED"))
	if err != nil {
		t.Fatal(err)
	}
	oldTime := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	newTime := oldTime.Add(time.Minute)
	runner := NewRunner(plan)
	runner.Reset(map[string]Input{
		"READY":    {Type: domain.DataTypeUInt32, Value: uint32(0), StatusCode: domain.StatusGood, SourceTimestamp: oldTime},
		"ANALOG":   {Type: domain.DataTypeDouble, Value: 99.0, StatusCode: domain.StatusBadNoCommunication, SourceTimestamp: newTime},
		"MASK":     {Type: domain.DataTypeUInt32, Value: uint32(5), StatusCode: domain.StatusGood, SourceTimestamp: oldTime},
		"DISABLED": {Type: domain.DataTypeBoolean, Value: false, StatusCode: domain.StatusGood, SourceTimestamp: oldTime},
	}, 7, 11)
	results := map[string]Result{}
	for _, item := range runner.Results() {
		results[item.NodeID] = item
	}
	if results["SELECTED"].Value != float64(4) || results["SELECTED"].StatusCode != domain.StatusGood || !results["SELECTED"].SourceTimestamp.Equal(oldTime) {
		t.Fatalf("unselected bad IF branch polluted result: %#v", results["SELECTED"])
	}
	if len(results["SELECTED"].CommunicationInputs) != 0 {
		t.Fatalf("unselected bad IF branch polluted provenance: %#v", results["SELECTED"].CommunicationInputs)
	}
	if results["BITS"].Value != uint32(23) || results["FLAG"].Value != true {
		t.Fatalf("operator result mismatch: BITS=%#v FLAG=%#v", results["BITS"], results["FLAG"])
	}
	changed := runner.Update(map[string]Input{"READY": {Type: domain.DataTypeUInt32, Value: uint32(1), StatusCode: domain.StatusGood, SourceTimestamp: newTime}}, 7, 11)
	if len(changed) != 2 {
		t.Fatalf("READY should update SELECTED and FLAG only, changed=%v", resultNames(changed))
	}
	results = map[string]Result{}
	for _, item := range runner.Results() {
		results[item.NodeID] = item
	}
	if results["SELECTED"].StatusCode != domain.StatusBadNoCommunication || results["FLAG"].StatusCode != domain.StatusBadNoCommunication {
		t.Fatalf("selected bad path quality was not propagated: %#v %#v", results["SELECTED"], results["FLAG"])
	}
	if fmt.Sprint(results["SELECTED"].CommunicationInputs) != "[ANALOG]" || fmt.Sprint(results["FLAG"].CommunicationInputs) != "[ANALOG]" {
		t.Fatalf("communication provenance was not propagated through calculated values: %#v %#v", results["SELECTED"], results["FLAG"])
	}
	if results["SELECTED"].Value != float64(5) || results["FLAG"].Value != true {
		t.Fatalf("communication-failed input was not evaluated as zero: %#v %#v", results["SELECTED"], results["FLAG"])
	}
}

func TestRunnerIncrementalClosureAndRouteRevisionRefresh(t *testing.T) {
	config := testConfig(t, `[[rules]]
kks_calc="X"
data_type="double"
expression="A + 1"
[[rules]]
kks_calc="Y"
data_type="double"
expression="X + 1"
[[rules]]
kks_calc="Z"
data_type="double"
expression="B + 1"`)
	plan, err := Compile(config, scalarInputs("A", "B"))
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(plan)
	inputs := map[string]Input{
		"A": {Type: domain.DataTypeDouble, Value: 1.0, StatusCode: domain.StatusGood},
		"B": {Type: domain.DataTypeDouble, Value: 2.0, StatusCode: domain.StatusGood},
	}
	if changed := runner.Reset(inputs, 1, 1); len(changed) != 3 {
		t.Fatalf("initial changes=%d", len(changed))
	}
	changed := runner.Update(map[string]Input{"A": {Type: domain.DataTypeDouble, Value: 2.0, StatusCode: domain.StatusGood}}, 1, 1)
	if fmt.Sprint(resultNames(changed)) != "[X Y]" {
		t.Fatalf("incremental closure=%v", resultNames(changed))
	}
	// A default-source switch must refresh subscriptions even when the selected
	// source currently exposes byte-for-byte identical values and timestamps.
	inputs["A"] = Input{Type: domain.DataTypeDouble, Value: 2.0, StatusCode: domain.StatusGood}
	changed = runner.Reset(inputs, 1, 2)
	if len(changed) != 3 {
		t.Fatalf("route revision did not refresh all results: %v", resultNames(changed))
	}
}

func TestRunnerRuntimeErrorUsesConfiguredFallbackAndReportsBadConfiguration(t *testing.T) {
	config := testConfig(t, `[[rules]]
kks_calc="OUT"
data_type="uint"
expression="A / 0"
default_value=9`)
	plan, err := Compile(config, scalarInputs("A"))
	if err != nil {
		t.Fatal(err)
	}
	sourceTime := time.Date(2026, 9, 1, 6, 30, 0, 0, time.UTC)
	result := NewRunner(plan).Reset(map[string]Input{"A": {Type: domain.DataTypeDouble, Value: 2.0, StatusCode: domain.StatusGood, SourceTimestamp: sourceTime}}, 1, 1)[0]
	if result.Value != uint32(9) || result.StatusCode != domain.StatusBadConfigurationError || result.Error == "" {
		t.Fatalf("unexpected fallback: %#v", result)
	}
	if !result.SourceTimestamp.Equal(sourceTime) {
		t.Fatalf("runtime fallback lost actual-path source timestamp: %#v", result)
	}
}

func TestComparePlansCountsAddedRemovedAndChangedRules(t *testing.T) {
	before, err := Compile(testConfig(t, `[[rules]]
kks_calc="KEEP"
data_type="uint"
expression="1"
[[rules]]
kks_calc="CHANGE"
data_type="uint"
expression="2"
[[rules]]
kks_calc="REMOVE"
data_type="uint"
expression="3"`), nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := Compile(testConfig(t, `[[rules]]
kks_calc="KEEP"
data_type="uint"
expression="1"
[[rules]]
kks_calc="CHANGE"
data_type="uint"
expression="20"
[[rules]]
kks_calc="ADD"
data_type="uint"
expression="4"`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ComparePlans(before, after), (DiffSummary{Added: 1, Removed: 1, Changed: 1}); got != want {
		t.Fatalf("diff=%#v want=%#v", got, want)
	}
}

func resultNames(results []Result) []string {
	names := make([]string, len(results))
	for index, result := range results {
		names[index] = result.NodeID
	}
	return names
}
