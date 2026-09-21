package calculation

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

type StateTarget struct {
	SourceID string `toml:"source_id" json:"source_id"`
	PortID   string `toml:"port_id" json:"port_id"`
	DeviceID string `toml:"device_id" json:"device_id"`
}

type StateInput struct {
	Name    string        `toml:"name" json:"name"`
	Mode    string        `toml:"mode" json:"mode"`
	GroupID string        `toml:"group_id,omitempty" json:"group_id,omitempty"`
	Targets []StateTarget `toml:"targets" json:"targets"`
}

var stateName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateStateInputs(inputs []StateInput) error {
	if len(inputs) > MaxRules {
		return fmt.Errorf("%w: too many state inputs", ErrConfigTooLarge)
	}
	names := map[string]bool{}
	for _, input := range inputs {
		if len(input.Name) > 128 || !stateName.MatchString(input.Name) || strings.EqualFold(input.Name, "IF") || strings.EqualFold(input.Name, "true") || strings.EqualFold(input.Name, "false") {
			return fmt.Errorf("invalid state input name %q", input.Name)
		}
		if names[input.Name] {
			return fmt.Errorf("duplicate state input %s", input.Name)
		}
		names[input.Name] = true
		if input.Mode != "fixed" && input.Mode != "group_default" {
			return fmt.Errorf("state input %s: invalid mode", input.Name)
		}
		if input.Mode == "fixed" && (len(input.Targets) != 1 || input.GroupID != "") {
			return fmt.Errorf("state input %s: fixed mode needs exactly one target and no group_id", input.Name)
		}
		if input.Mode == "group_default" && (input.GroupID == "" || len(input.Targets) == 0) {
			return fmt.Errorf("state input %s: group_default needs group_id and targets", input.Name)
		}
		seen := map[string]bool{}
		for _, target := range input.Targets {
			if target.SourceID == "" || target.PortID == "" || target.DeviceID == "" || seen[target.SourceID] {
				return fmt.Errorf("state input %s: incomplete or duplicate source target", input.Name)
			}
			seen[target.SourceID] = true
		}
	}
	return nil
}

func cloneStateInputs(inputs []StateInput) []StateInput {
	out := append([]StateInput{}, inputs...)
	for i := range out {
		out[i].Targets = append([]StateTarget{}, out[i].Targets...)
	}
	return out
}

func (p *Plan) StateInputs() []StateInput { return cloneStateInputs(p.config.StateInputs) }
func (p *Plan) StateDependencies(output string) map[string]bool {
	found := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string)
	visit = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		if _, ok := p.stateInputs[name]; ok {
			found[name] = true
			return
		}
		if index, ok := p.byName[name]; ok {
			for _, dep := range p.rules[index].variables {
				visit(dep)
			}
		}
	}
	visit(output)
	return found
}
func (p *Plan) StateInputsForSource(source string) []StateInput {
	if source == "" {
		return p.StateInputs()
	}
	out := []StateInput{}
	for _, name := range p.statesBySource[source] {
		value, _ := p.StateInput(name)
		out = append(out, value)
	}
	return out
}
func (p *Plan) StateInput(name string) (StateInput, bool) {
	value, ok := p.stateInputs[name]
	value.Targets = append([]StateTarget(nil), value.Targets...)
	return value, ok
}

// ReplaceStateInputs changes only the state mapping values. The TOML encoder
// normalizes layout/comments; all existing rule fields retain their values.
func ReplaceStateInputs(content string, inputs []StateInput) (string, error) {
	if _, err := ParseConfig(content); err != nil {
		return "", err
	}
	if err := validateStateInputs(inputs); err != nil {
		return "", err
	}
	document := map[string]any{}
	if _, err := toml.Decode(content, &document); err != nil {
		return "", err
	}
	if len(inputs) == 0 {
		delete(document, "state_inputs")
	} else {
		document["state_inputs"] = inputs
	}
	var output bytes.Buffer
	if err := toml.NewEncoder(&output).Encode(document); err != nil {
		return "", err
	}
	if _, err := ParseConfig(output.String()); err != nil {
		return "", err
	}
	return output.String(), nil
}
