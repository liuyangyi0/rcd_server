package calculation

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"rcd_manager/internal/domain"
)

var ErrConfigTooLarge = errors.New("calculation configuration exceeds a limit")

const (
	MaxConfigBytes = 4 * 1024 * 1024
	MaxRules       = 25_000
	// The deployed calc.toml contains generated minimum-selection expressions
	// up to roughly 42 KiB / 6,800 tokens. Keep a bounded parser budget while
	// accepting that existing configuration during a manager migration.
	MaxExpressionBytes = 64 * 1024
	MaxTokens          = 8 * 1024
)

type Rule struct {
	Output       string          `json:"kks_calc"`
	DataType     domain.DataType `json:"data_type"`
	Expression   string          `json:"expression"`
	MaxLimit     float64         `json:"max_limit"`
	MinLimit     float64         `json:"min_limit"`
	DefaultValue float64         `json:"default_value"`
}

type rawConfig struct {
	Rules       []rawRule    `toml:"rules"`
	StateInputs []StateInput `toml:"state_inputs"`
}

type rawRule struct {
	Output       string   `toml:"kks_calc"`
	DataType     string   `toml:"data_type"`
	Expression   string   `toml:"expression"`
	MaxLimit     *float64 `toml:"max_limit"`
	MinLimit     *float64 `toml:"min_limit"`
	DefaultValue *float64 `toml:"default_value"`
}

type Config struct {
	Content     string       `json:"content"`
	Hash        string       `json:"hash"`
	Rules       []Rule       `json:"rules"`
	StateInputs []StateInput `json:"state_inputs"`
}

type Draft struct {
	Revision  uint64    `json:"revision"`
	Content   string    `json:"content"`
	Hash      string    `json:"hash"`
	Valid     bool      `json:"valid"`
	Problems  []Problem `json:"problems,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Version struct {
	ID           int64     `json:"id"`
	Revision     uint64    `json:"revision"`
	Hash         string    `json:"hash"`
	Content      string    `json:"content,omitempty"`
	RuleCount    int       `json:"rule_count"`
	AddedCount   int       `json:"added_count"`
	RemovedCount int       `json:"removed_count"`
	ChangedCount int       `json:"changed_count"`
	State        string    `json:"state"`
	Actor        string    `json:"actor"`
	CreatedAt    time.Time `json:"created_at"`
	ActivatedAt  time.Time `json:"activated_at,omitempty,omitzero"`
}

func ParseConfig(content string) (Config, error) {
	if len(content) > MaxConfigBytes {
		return Config{}, fmt.Errorf("%w: calc.toml exceeds %d bytes", ErrConfigTooLarge, MaxConfigBytes)
	}
	var raw rawConfig
	metadata, err := toml.Decode(content, &raw)
	if err != nil {
		return Config{}, fmt.Errorf("parse calc.toml: %w", err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) != 0 {
		return Config{}, fmt.Errorf("calc.toml contains unknown field %s", undecoded[0])
	}
	if len(raw.Rules) > MaxRules {
		return Config{}, fmt.Errorf("%w: calc.toml contains %d rules; maximum is %d", ErrConfigTooLarge, len(raw.Rules), MaxRules)
	}
	rules := make([]Rule, 0, len(raw.Rules))
	for index, item := range raw.Rules {
		// Existing Rust deployments preserve kks_calc byte-for-byte when they
		// create ns=2 string NodeIds. In particular, some generated legacy names
		// end in a space. Use a trimmed copy only for the empty-name check.
		name := item.Output
		if strings.TrimSpace(name) == "" || strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return Config{}, fmt.Errorf("rules[%d].kks_calc is empty or contains control characters", index+1)
		}
		dataType, ok := normalizeDataType(item.DataType)
		if !ok {
			return Config{}, fmt.Errorf("rule %s uses unsupported data_type %q", name, item.DataType)
		}
		if item.Expression == "" {
			return Config{}, fmt.Errorf("rule %s has an empty expression", name)
		}
		if len(item.Expression) > MaxExpressionBytes {
			return Config{}, fmt.Errorf("rule %s expression exceeds %d bytes", name, MaxExpressionBytes)
		}
		maxLimit, minLimit, defaultValue := math.MaxFloat64, -math.MaxFloat64, 0.0
		if item.MaxLimit != nil {
			maxLimit = *item.MaxLimit
		}
		if item.MinLimit != nil {
			minLimit = *item.MinLimit
		}
		if item.DefaultValue != nil {
			defaultValue = *item.DefaultValue
		}
		if math.IsNaN(maxLimit) || math.IsInf(maxLimit, 0) || math.IsNaN(minLimit) || math.IsInf(minLimit, 0) ||
			math.IsNaN(defaultValue) || math.IsInf(defaultValue, 0) || minLimit > maxLimit {
			return Config{}, fmt.Errorf("rule %s limits/default must be finite and min_limit <= max_limit", name)
		}
		rules = append(rules, Rule{Output: name, DataType: dataType, Expression: item.Expression,
			MaxLimit: maxLimit, MinLimit: minLimit, DefaultValue: defaultValue})
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	if err := validateStateInputs(raw.StateInputs); err != nil {
		return Config{}, err
	}
	return Config{Content: content, Hash: hash, Rules: rules, StateInputs: raw.StateInputs}, nil
}

func normalizeDataType(value string) (domain.DataType, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "uint", "u32", "uint32":
		return domain.DataTypeUInt32, true
	case "bool", "boolean":
		return domain.DataTypeBoolean, true
	case "float", "f32":
		return domain.DataTypeFloat, true
	case "double", "f64":
		return domain.DataTypeDouble, true
	default:
		return "", false
	}
}
