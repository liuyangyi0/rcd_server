package calculation

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	"rcd_manager/internal/domain"
)

type rustParityResult struct {
	Kind         string  `json:"kind"`
	Number       float64 `json:"number"`
	Boolean      bool    `json:"boolean"`
	Valid        bool    `json:"valid"`
	Dependencies int     `json:"dependencies"`
}

// TestDeployedRustParity is opt-in because it consumes an artifact exported by
// the Rust integration test. It verifies every rule in the real calc.toml, not
// merely a hand-picked expression subset.
func TestDeployedRustParity(t *testing.T) {
	configPath := os.Getenv("RCD_CALC_PARITY_CONFIG")
	rustPath := os.Getenv("RCD_CALC_PARITY_RUST_RESULTS")
	if configPath == "" || rustPath == "" {
		t.Skip("set RCD_CALC_PARITY_CONFIG and RCD_CALC_PARITY_RUST_RESULTS")
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config, err := ParseConfig(string(content))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	inputs := make(map[string]Input, len(plan.BaseVariables()))
	for _, name := range plan.BaseVariables() {
		inputs[name] = Input{Type: domain.DataTypeUInt32, Value: uint32(deterministicParityInput(name)), StatusCode: domain.StatusGood}
	}
	results := NewRunner(plan).Reset(inputs, 1, 1)

	encoded, err := os.ReadFile(rustPath)
	if err != nil {
		t.Fatal(err)
	}
	rust := make(map[string]rustParityResult)
	if err = json.Unmarshal(encoded, &rust); err != nil {
		t.Fatal(err)
	}
	if len(results) != len(rust) {
		t.Fatalf("result count: Go=%d Rust=%d", len(results), len(rust))
	}
	for _, result := range results {
		want, exists := rust[result.NodeID]
		if !exists {
			t.Errorf("Rust result missing %s", result.NodeID)
			continue
		}
		if string(result.DataType) != want.Kind {
			t.Errorf("%s type: Go=%s Rust=%s", result.NodeID, result.DataType, want.Kind)
		}
		if result.DependencyCount != want.Dependencies {
			t.Errorf("%s dependencies: Go=%d Rust=%d", result.NodeID, result.DependencyCount, want.Dependencies)
		}
		if result.StatusCode.IsGood() != want.Valid {
			t.Errorf("%s validity: Go=0x%08X Rust=%v", result.NodeID, uint32(result.StatusCode), want.Valid)
		}
		if want.Kind == string(domain.DataTypeBoolean) {
			value, ok := result.Value.(bool)
			if !ok || value != want.Boolean {
				t.Errorf("%s value: Go=%#v Rust=%v", result.NodeID, result.Value, want.Boolean)
			}
			continue
		}
		got, ok := parityNumber(result.Value)
		if !ok || !numbersEquivalent(got, want.Number) {
			t.Errorf("%s value: Go=%#v Rust=%v", result.NodeID, result.Value, want.Number)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	t.Logf("Rust/Go parity passed for %d deployed calculation rules", len(results))
}

func deterministicParityInput(name string) uint32 {
	value := uint32(2_166_136_261)
	for _, item := range []byte(name) {
		value = (value ^ uint32(item)) * 16_777_619
	}
	return value%17 + 1
}

func parityNumber(value any) (float64, bool) {
	switch value := value.(type) {
	case uint32:
		return float64(value), true
	case float32:
		return float64(value), true
	case float64:
		return value, true
	default:
		return 0, false
	}
}

func numbersEquivalent(left, right float64) bool {
	if left == right {
		return true
	}
	delta := math.Abs(left - right)
	return delta <= 1e-9*math.Max(1, math.Max(math.Abs(left), math.Abs(right)))
}
