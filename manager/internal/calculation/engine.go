package calculation

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	"rcd_manager/internal/domain"
)

type InputDefinition struct {
	DataType  domain.DataType
	ValueRank int32
}

type Problem struct {
	RuleIndex int    `json:"rule_index,omitempty"`
	Rule      string `json:"rule,omitempty"`
	Field     string `json:"field,omitempty"`
	Message   string `json:"message"`
}

type ValidationError struct{ Problems []Problem }

func (e *ValidationError) Error() string {
	if len(e.Problems) == 0 {
		return "invalid calculation configuration"
	}
	return e.Problems[0].Message
}

type compiledRule struct {
	Rule
	ast       *expression
	variables []string
}

type Plan struct {
	config           Config
	rules            []compiledRule
	byName           map[string]int
	dependents       map[string][]int
	baseVariables    []string
	inputDefinitions map[string]InputDefinition
	stateInputs      map[string]StateInput
	statesBySource   map[string][]string
}

type RuleView struct {
	Index int `json:"index"`
	Rule
	Dependencies []string `json:"dependencies"`
}

type DiffSummary struct {
	Added   int
	Removed int
	Changed int
}

// ComparePlans reports a compact immutable-version diff without persisting the
// TOML or individual expressions in the audit trail.
func ComparePlans(before, after *Plan) DiffSummary {
	previous := make(map[string]Rule)
	if before != nil {
		for _, item := range before.rules {
			previous[item.Output] = item.Rule
		}
	}
	next := make(map[string]Rule)
	if after != nil {
		for _, item := range after.rules {
			next[item.Output] = item.Rule
		}
	}
	var result DiffSummary
	for name, rule := range next {
		old, exists := previous[name]
		if !exists {
			result.Added++
		} else if old != rule {
			result.Changed++
		}
	}
	for name := range previous {
		if _, exists := next[name]; !exists {
			result.Removed++
		}
	}
	oldStates, newStates := map[string]StateInput{}, map[string]StateInput{}
	if before != nil {
		for _, v := range before.StateInputs() {
			oldStates[v.Name] = v
		}
	}
	if after != nil {
		for _, v := range after.StateInputs() {
			newStates[v.Name] = v
		}
	}
	for name, v := range newStates {
		if old, ok := oldStates[name]; !ok {
			result.Added++
		} else if !reflect.DeepEqual(old, v) {
			result.Changed++
		}
	}
	for name := range oldStates {
		if _, ok := newStates[name]; !ok {
			result.Removed++
		}
	}
	return result
}

func Compile(config Config, inputs map[string]InputDefinition) (*Plan, error) {
	if err := validateStateInputs(config.StateInputs); err != nil {
		return nil, err
	}
	strictInputs := inputs != nil
	merged := make(map[string]InputDefinition, len(inputs)+len(config.StateInputs))
	for name, definition := range inputs {
		merged[name] = definition
	}
	for _, input := range config.StateInputs {
		if _, ok := merged[input.Name]; ok {
			return nil, &ValidationError{Problems: []Problem{{Field: "state_inputs", Rule: input.Name, Message: "state input conflicts with a lower point or command: " + input.Name}}}
		}
		merged[input.Name] = InputDefinition{DataType: domain.DataTypeUInt32, ValueRank: -1}
	}
	inputs = merged
	problems := make([]Problem, 0)
	outputs := make(map[string]int, len(config.Rules))
	compiled := make([]compiledRule, len(config.Rules))
	for index, rule := range config.Rules {
		if previous, exists := outputs[rule.Output]; exists {
			problems = append(problems, Problem{RuleIndex: index + 1, Rule: rule.Output, Field: "kks_calc", Message: fmt.Sprintf("duplicate output %s (first at rule %d)", rule.Output, previous+1)})
		} else {
			outputs[rule.Output] = index
		}
		if _, collision := inputs[rule.Output]; collision {
			problems = append(problems, Problem{RuleIndex: index + 1, Rule: rule.Output, Field: "kks_calc", Message: fmt.Sprintf("calculation output %s conflicts with a lower point or command", rule.Output)})
		}
		ast, variables, err := parseExpression(rule.Expression)
		if err != nil {
			problems = append(problems, Problem{RuleIndex: index + 1, Rule: rule.Output, Field: "expression", Message: fmt.Sprintf("rule %s: %v", rule.Output, err)})
			continue
		}
		for _, name := range variables {
			if name == rule.Output {
				problems = append(problems, Problem{RuleIndex: index + 1, Rule: rule.Output, Field: "expression", Message: fmt.Sprintf("rule %s references itself", rule.Output)})
			}
		}
		compiled[index] = compiledRule{Rule: rule, ast: ast, variables: variables}
	}
	for index, rule := range compiled {
		if rule.ast == nil {
			continue
		}
		for _, dependency := range rule.variables {
			if _, calculated := outputs[dependency]; calculated {
				continue
			}
			definition, exists := inputs[dependency]
			if strictInputs && !exists {
				problems = append(problems, Problem{RuleIndex: index + 1, Rule: rule.Output, Field: "expression", Message: fmt.Sprintf("rule %s references missing point %s", rule.Output, dependency)})
				continue
			}
			if exists && (definition.ValueRank >= 0 || definition.DataType == domain.DataTypeString) {
				problems = append(problems, Problem{RuleIndex: index + 1, Rule: rule.Output, Field: "expression", Message: fmt.Sprintf("rule %s references non-numeric scalar point %s", rule.Output, dependency)})
			}
		}
	}
	if len(problems) != 0 {
		return nil, &ValidationError{Problems: problems}
	}

	adjacency := make([][]int, len(compiled))
	inDegree := make([]int, len(compiled))
	for index, rule := range compiled {
		for _, dependency := range rule.variables {
			if dependencyIndex, exists := outputs[dependency]; exists {
				adjacency[dependencyIndex] = append(adjacency[dependencyIndex], index)
				inDegree[index]++
			}
		}
	}
	queue := make([]int, 0, len(compiled))
	for index, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, index)
		}
	}
	order := make([]int, 0, len(compiled))
	for len(queue) != 0 {
		index := queue[0]
		queue = queue[1:]
		order = append(order, index)
		for _, next := range adjacency[index] {
			inDegree[next]--
			if inDegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if len(order) != len(compiled) {
		for index, degree := range inDegree {
			if degree != 0 {
				problems = append(problems, Problem{RuleIndex: index + 1, Rule: compiled[index].Output, Field: "expression", Message: fmt.Sprintf("rule %s participates in or depends on a cycle", compiled[index].Output)})
			}
		}
		return nil, &ValidationError{Problems: problems}
	}
	sortedRules := make([]compiledRule, len(order))
	for index, original := range order {
		sortedRules[index] = compiled[original]
	}
	plan := &Plan{config: config, rules: sortedRules, byName: make(map[string]int, len(sortedRules)), dependents: make(map[string][]int), inputDefinitions: make(map[string]InputDefinition)}
	plan.config.StateInputs = cloneStateInputs(config.StateInputs)
	plan.stateInputs = make(map[string]StateInput)
	plan.statesBySource = make(map[string][]string)
	for _, input := range plan.config.StateInputs {
		plan.stateInputs[input.Name] = input
		for _, target := range input.Targets {
			plan.statesBySource[target.SourceID] = append(plan.statesBySource[target.SourceID], input.Name)
		}
	}
	for name, definition := range inputs {
		plan.inputDefinitions[name] = definition
	}
	base := make(map[string]struct{})
	for index, rule := range sortedRules {
		plan.byName[rule.Output] = index
	}
	for index, rule := range sortedRules {
		for _, dependency := range rule.variables {
			plan.dependents[dependency] = append(plan.dependents[dependency], index)
			if _, calculated := plan.byName[dependency]; !calculated {
				base[dependency] = struct{}{}
			}
		}
	}
	for name := range base {
		plan.baseVariables = append(plan.baseVariables, name)
	}
	sort.Strings(plan.baseVariables)
	return plan, nil
}

func (p *Plan) Hash() string { return p.config.Hash }
func (p *Plan) RuleCount() int {
	if p == nil {
		return 0
	}
	return len(p.rules)
}
func (p *Plan) BaseVariables() []string {
	if p == nil {
		return nil
	}
	return append([]string(nil), p.baseVariables...)
}
func (p *Plan) HasBaseVariable(name string) bool {
	if p == nil {
		return false
	}
	index := sort.SearchStrings(p.baseVariables, name)
	return index < len(p.baseVariables) && p.baseVariables[index] == name
}
func (p *Plan) InputDefinition(name string) (InputDefinition, bool) {
	if p == nil {
		return InputDefinition{}, false
	}
	definition, ok := p.inputDefinitions[name]
	return definition, ok
}
func (p *Plan) OutputDataType(name string) (domain.DataType, bool) {
	if p == nil {
		return "", false
	}
	index, ok := p.byName[name]
	if !ok {
		return "", false
	}
	return p.rules[index].DataType, true
}
func (p *Plan) OutputNames() []string {
	if p == nil {
		return nil
	}
	result := make([]string, 0, len(p.rules))
	for _, rule := range p.rules {
		result = append(result, rule.Output)
	}
	return result
}
func (p *Plan) RuleViews() []RuleView {
	if p == nil {
		return nil
	}
	result := make([]RuleView, len(p.rules))
	for i, rule := range p.rules {
		result[i] = RuleView{Index: i + 1, Rule: rule.Rule, Dependencies: append([]string(nil), rule.variables...)}
	}
	return result
}
func (p *Plan) CatalogNodes() []domain.CatalogNode {
	if p == nil {
		return nil
	}
	nodes := make([]domain.CatalogNode, 0, len(p.rules))
	for _, rule := range p.rules {
		nodes = append(nodes, domain.CatalogNode{Kind: "manager_calculation", Key: domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: rule.Output}, BrowseName: rule.Output, ParentPath: "Objects/Devices", Class: domain.NodeClassVariable, DataType: rule.DataType, ValueRank: -1})
	}
	return nodes
}

type Input struct {
	Type            domain.DataType
	Value           any
	StatusCode      domain.StatusCode
	SourceTimestamp time.Time
}

func InputFromSample(sample domain.Sample, fallback domain.DataType) Input {
	dataType := sample.Type
	if dataType == "" {
		dataType = fallback
	}
	value := sample.Value
	if value == nil {
		value = typedZero(dataType)
	}
	return Input{Type: dataType, Value: value, StatusCode: sample.StatusCode, SourceTimestamp: sample.SourceTimestamp}
}

type Result struct {
	NodeID               string            `json:"node_id"`
	DataType             domain.DataType   `json:"data_type"`
	Expression           string            `json:"expression"`
	Value                any               `json:"value"`
	StatusCode           domain.StatusCode `json:"status_code"`
	CommunicationInputs  []string          `json:"communication_inputs,omitempty"`
	SourceTimestamp      time.Time         `json:"source_timestamp,omitempty,omitzero"`
	EvaluatedAt          time.Time         `json:"evaluated_at"`
	Error                string            `json:"error,omitempty"`
	DependencyCount      int               `json:"dependency_count"`
	ConfigRevision       uint64            `json:"config_revision"`
	DefaultRouteRevision uint64            `json:"default_route_revision"`
}

func (r Result) Sample() domain.Sample {
	return domain.Sample{Type: r.DataType, Value: r.Value, StatusCode: r.StatusCode, SourceTimestamp: r.SourceTimestamp}
}

type Runner struct {
	plan      *Plan
	variables map[string]operand
	results   []Result
}

func NewRunner(plan *Plan) *Runner {
	return &Runner{plan: plan, variables: make(map[string]operand), results: make([]Result, plan.RuleCount())}
}

func (r *Runner) Reset(inputs map[string]Input, configRevision, routeRevision uint64) []Result {
	r.variables = make(map[string]operand, len(inputs)+r.plan.RuleCount())
	for name, input := range inputs {
		r.variables[name] = inputOperand(name, input)
	}
	affected := make([]bool, r.plan.RuleCount())
	for index := range affected {
		affected[index] = true
	}
	return r.evaluate(affected, configRevision, routeRevision)
}
func (r *Runner) Update(inputs map[string]Input, configRevision, routeRevision uint64) []Result {
	if len(inputs) == 0 {
		return nil
	}
	queue := make([]string, 0, len(inputs))
	for name, input := range inputs {
		r.variables[name] = inputOperand(name, input)
		queue = append(queue, name)
	}
	affected := make([]bool, r.plan.RuleCount())
	visitedNames := make(map[string]struct{}, len(queue))
	for len(queue) != 0 {
		name := queue[0]
		queue = queue[1:]
		if _, seen := visitedNames[name]; seen {
			continue
		}
		visitedNames[name] = struct{}{}
		for _, index := range r.plan.dependents[name] {
			if !affected[index] {
				affected[index] = true
				queue = append(queue, r.plan.rules[index].Output)
			}
		}
	}
	return r.evaluate(affected, configRevision, routeRevision)
}
func (r *Runner) Results() []Result {
	result := make([]Result, len(r.results))
	copy(result, r.results)
	return result
}

func (r *Runner) evaluate(affected []bool, configRevision, routeRevision uint64) []Result {
	now := time.Now().UTC()
	changed := make([]Result, 0)
	for index, rule := range r.plan.rules {
		if !affected[index] {
			continue
		}
		value, err := eval(rule.ast, r.variables)
		errorText := ""
		if err != nil || value.value.kind == valueFloat && (math.IsNaN(value.value.floating) || math.IsInf(value.value.floating, 0)) {
			if err != nil {
				errorText = err.Error()
			} else {
				errorText = "expression produced NaN or Infinity"
			}
			// Runtime failures use the configured fallback. The Bad quality remains
			// attached so consumers cannot mistake that fallback for live process
			// data.
			value.value = floatValue(rule.DefaultValue)
			value.status = domain.StatusBadConfigurationError
		}
		typed := convertOutput(value.value, rule)
		result := Result{NodeID: rule.Output, DataType: rule.DataType, Expression: rule.Expression, Value: typed, StatusCode: value.status,
			CommunicationInputs: append([]string(nil), value.communicationInputs...), SourceTimestamp: value.sourceTimestamp, EvaluatedAt: now,
			Error: errorText, DependencyCount: len(rule.variables), ConfigRevision: configRevision, DefaultRouteRevision: routeRevision}
		previous := r.results[index]
		r.results[index] = result
		r.variables[rule.Output] = operand{value: domainValue(typed), status: result.StatusCode, sourceTimestamp: result.SourceTimestamp,
			communicationInputs: result.CommunicationInputs}
		if previous.NodeID == "" || previous.StatusCode != result.StatusCode || previous.Error != result.Error ||
			previous.ConfigRevision != result.ConfigRevision || previous.DefaultRouteRevision != result.DefaultRouteRevision ||
			!previous.SourceTimestamp.Equal(result.SourceTimestamp) || !reflect.DeepEqual(previous.Value, result.Value) ||
			!reflect.DeepEqual(previous.CommunicationInputs, result.CommunicationInputs) {
			changed = append(changed, result)
		}
	}
	return changed
}

func inputOperand(name string, input Input) operand {
	// A communications failure means the live acquisition value is no longer
	// available. Evaluate the expression with the declared type's zero value,
	// while propagating BadNoCommunication through the actual execution path.
	// Information bits do not change the underlying OPC UA status code.
	if input.StatusCode.BaseCode() == domain.StatusBadNoCommunication {
		input.Value = typedZero(input.Type)
		return operand{value: domainValue(input.Value), status: input.StatusCode, sourceTimestamp: input.SourceTimestamp,
			communicationInputs: []string{name}}
	}
	return operand{value: domainValue(input.Value), status: input.StatusCode, sourceTimestamp: input.SourceTimestamp}
}
func domainValue(value any) evalValue {
	switch value := value.(type) {
	case bool:
		return boolValue(value)
	case uint32:
		return intValue(int64(value))
	case uint:
		return intValue(int64(value))
	case uint64:
		if value > math.MaxInt64 {
			return floatValue(float64(value))
		}
		return intValue(int64(value))
	case int:
		return intValue(int64(value))
	case int32:
		return intValue(int64(value))
	case int64:
		return intValue(value)
	case float32:
		return floatValue(float64(value))
	case float64:
		return floatValue(value)
	default:
		return intValue(0)
	}
}
func convertOutput(value evalValue, rule compiledRule) any {
	number := value.asFloat()
	if number < rule.MinLimit {
		number = rule.MinLimit
	}
	if number > rule.MaxLimit {
		number = rule.MaxLimit
	}
	switch rule.DataType {
	case domain.DataTypeUInt32:
		if number < 0 {
			return uint32(0)
		}
		if number > math.MaxUint32 {
			return uint32(math.MaxUint32)
		}
		return uint32(number)
	case domain.DataTypeBoolean:
		return number != 0
	case domain.DataTypeFloat:
		if number > math.MaxFloat32 {
			number = math.MaxFloat32
		}
		if number < -math.MaxFloat32 {
			number = -math.MaxFloat32
		}
		return float32(number)
	default:
		return number
	}
}
func typedZero(dataType domain.DataType) any {
	switch dataType {
	case domain.DataTypeBoolean:
		return false
	case domain.DataTypeUInt32:
		return uint32(0)
	case domain.DataTypeFloat:
		return float32(0)
	case domain.DataTypeDouble:
		return float64(0)
	default:
		return nil
	}
}

func MatchRule(view RuleView, query, dataType string) bool {
	if dataType != "" && !strings.EqualFold(string(view.DataType), dataType) {
		return false
	}
	query = strings.ToLower(strings.TrimSpace(query))
	return query == "" || strings.Contains(strings.ToLower(view.Output+" "+view.Expression), query)
}
