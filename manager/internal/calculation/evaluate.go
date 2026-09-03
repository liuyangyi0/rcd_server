package calculation

import (
	"errors"
	"fmt"
	"math"
	"time"

	"rcd_manager/internal/domain"
)

const floatEqualityEpsilon = 1e-9

type valueKind uint8

const (
	valueInt valueKind = iota
	valueBool
	valueFloat
)

type evalValue struct {
	kind     valueKind
	integer  int64
	boolean  bool
	floating float64
}

func intValue(value int64) evalValue     { return evalValue{kind: valueInt, integer: value} }
func boolValue(value bool) evalValue     { return evalValue{kind: valueBool, boolean: value} }
func floatValue(value float64) evalValue { return evalValue{kind: valueFloat, floating: value} }
func (v evalValue) asBool() bool {
	switch v.kind {
	case valueBool:
		return v.boolean
	case valueInt:
		return v.integer != 0
	default:
		return v.floating != 0
	}
}
func (v evalValue) asFloat() float64 {
	switch v.kind {
	case valueBool:
		if v.boolean {
			return 1
		}
		return 0
	case valueInt:
		return float64(v.integer)
	default:
		return v.floating
	}
}
func (v evalValue) asInteger() (int64, error) {
	switch v.kind {
	case valueInt:
		return v.integer, nil
	case valueBool:
		if v.boolean {
			return 1, nil
		}
		return 0, nil
	default:
		if math.IsNaN(v.floating) || math.IsInf(v.floating, 0) || math.Trunc(v.floating) != v.floating ||
			v.floating < float64(math.MinInt64) || v.floating > float64(math.MaxInt64) {
			return 0, fmt.Errorf("floating value %v is not an i64 integer", v.floating)
		}
		return int64(v.floating), nil
	}
}

type operand struct {
	value               evalValue
	status              domain.StatusCode
	sourceTimestamp     time.Time
	communicationInputs []string
}

var errDivisionByZero = errors.New("division by zero")

func eval(expr *expression, variables map[string]operand) (operand, error) {
	switch expr.kind {
	case exprLiteral:
		return operand{value: expr.value, status: domain.StatusGood}, nil
	case exprVariable:
		value, exists := variables[expr.name]
		if !exists {
			return operand{}, fmt.Errorf("variable %q is missing", expr.name)
		}
		return value, nil
	case exprUnary:
		value, err := eval(expr.left, variables)
		if err != nil {
			return operand{}, err
		}
		value.value, err = evalUnary(expr.unary, value.value)
		return value, err
	case exprBinary:
		left, err := eval(expr.left, variables)
		if err != nil {
			return operand{}, err
		}
		if expr.binary == opAnd && !left.value.asBool() {
			left.value = boolValue(false)
			return left, nil
		}
		if expr.binary == opOr && left.value.asBool() {
			left.value = boolValue(true)
			return left, nil
		}
		right, err := eval(expr.right, variables)
		if err != nil {
			return operand{}, err
		}
		result, err := evalBinary(expr.binary, left.value, right.value)
		combined := operand{
			status:              worseStatus(left.status, right.status),
			sourceTimestamp:     laterTime(left.sourceTimestamp, right.sourceTimestamp),
			communicationInputs: mergeSortedUniqueStrings(left.communicationInputs, right.communicationInputs),
		}
		if err != nil {
			// Keep the metadata of inputs that were actually evaluated. The
			// runner replaces only the value/status with the configured fallback.
			return combined, err
		}
		combined.value = result
		return combined, nil
	case exprIf:
		condition, err := eval(expr.condition, variables)
		if err != nil {
			return condition, err
		}
		branch := expr.elseBranch
		if condition.value.asBool() {
			branch = expr.thenBranch
		}
		selected, err := eval(branch, variables)
		if err != nil {
			selected.status = worseStatus(condition.status, selected.status)
			selected.sourceTimestamp = laterTime(condition.sourceTimestamp, selected.sourceTimestamp)
			selected.communicationInputs = mergeSortedUniqueStrings(condition.communicationInputs, selected.communicationInputs)
			return selected, err
		}
		selected.status = worseStatus(condition.status, selected.status)
		selected.sourceTimestamp = laterTime(condition.sourceTimestamp, selected.sourceTimestamp)
		selected.communicationInputs = mergeSortedUniqueStrings(condition.communicationInputs, selected.communicationInputs)
		return selected, nil
	default:
		return operand{}, errors.New("unknown expression node")
	}
}

// mergeSortedUniqueStrings combines immutable, sorted provenance lists without
// losing the original acquisition point behind a chain of calculated values.
func mergeSortedUniqueStrings(left, right []string) []string {
	if len(left) == 0 {
		return append([]string(nil), right...)
	}
	if len(right) == 0 {
		return append([]string(nil), left...)
	}
	result := make([]string, 0, len(left)+len(right))
	for i, j := 0, 0; i < len(left) || j < len(right); {
		switch {
		case i >= len(left):
			result = append(result, right[j:]...)
			return result
		case j >= len(right):
			result = append(result, left[i:]...)
			return result
		case left[i] < right[j]:
			result = append(result, left[i])
			i++
		case right[j] < left[i]:
			result = append(result, right[j])
			j++
		default:
			result = append(result, left[i])
			i++
			j++
		}
	}
	return result
}

func evalUnary(operator unaryOp, value evalValue) (evalValue, error) {
	switch operator {
	case opNot:
		return boolValue(!value.asBool()), nil
	case opBitNot:
		integer, err := value.asInteger()
		if err != nil {
			return evalValue{}, err
		}
		return intValue(^integer), nil
	default:
		if value.kind == valueFloat {
			return floatValue(-value.floating), nil
		}
		integer, err := value.asInteger()
		if err != nil {
			return evalValue{}, err
		}
		if integer == math.MinInt64 {
			return evalValue{}, errors.New("integer negation overflow")
		}
		return intValue(-integer), nil
	}
}

func evalBinary(operator binaryOp, left, right evalValue) (evalValue, error) {
	switch operator {
	case opAnd:
		return boolValue(right.asBool()), nil
	case opOr:
		return boolValue(right.asBool()), nil
	case opEq:
		return boolValue(equalValues(left, right)), nil
	case opNotEq:
		return boolValue(!equalValues(left, right)), nil
	case opLt:
		return boolValue(left.asFloat() < right.asFloat()), nil
	case opGt:
		return boolValue(left.asFloat() > right.asFloat()), nil
	case opLtEq:
		return boolValue(left.asFloat() <= right.asFloat()), nil
	case opGtEq:
		return boolValue(left.asFloat() >= right.asFloat()), nil
	case opDiv:
		if right.asFloat() == 0 {
			return evalValue{}, errDivisionByZero
		}
		return floatValue(left.asFloat() / right.asFloat()), nil
	case opMod:
		if right.asFloat() == 0 {
			return evalValue{}, errDivisionByZero
		}
		if left.kind == valueFloat || right.kind == valueFloat {
			return floatValue(math.Mod(left.asFloat(), right.asFloat())), nil
		}
		li, err := left.asInteger()
		if err != nil {
			return evalValue{}, err
		}
		ri, err := right.asInteger()
		if err != nil {
			return evalValue{}, err
		}
		if li == math.MinInt64 && ri == -1 {
			return evalValue{}, errors.New("integer remainder overflow")
		}
		return intValue(li % ri), nil
	case opAdd, opSub, opMul:
		if left.kind == valueFloat || right.kind == valueFloat {
			switch operator {
			case opAdd:
				return floatValue(left.asFloat() + right.asFloat()), nil
			case opSub:
				return floatValue(left.asFloat() - right.asFloat()), nil
			default:
				return floatValue(left.asFloat() * right.asFloat()), nil
			}
		}
		li, err := left.asInteger()
		if err != nil {
			return evalValue{}, err
		}
		ri, err := right.asInteger()
		if err != nil {
			return evalValue{}, err
		}
		switch operator {
		case opAdd:
			value, ok := checkedAdd(li, ri)
			if !ok {
				return evalValue{}, errors.New("integer addition overflow")
			}
			return intValue(value), nil
		case opSub:
			value, ok := checkedSub(li, ri)
			if !ok {
				return evalValue{}, errors.New("integer subtraction overflow")
			}
			return intValue(value), nil
		default:
			value, ok := checkedMul(li, ri)
			if !ok {
				return evalValue{}, errors.New("integer multiplication overflow")
			}
			return intValue(value), nil
		}
	default:
		li, err := left.asInteger()
		if err != nil {
			return evalValue{}, err
		}
		ri, err := right.asInteger()
		if err != nil {
			return evalValue{}, err
		}
		switch operator {
		case opBitAnd:
			return intValue(li & ri), nil
		case opBitOr:
			return intValue(li | ri), nil
		case opBitXor:
			return intValue(li ^ ri), nil
		case opShiftLeft, opShiftRight:
			if ri < 0 {
				return evalValue{}, fmt.Errorf("shift %d cannot be negative", ri)
			}
			if ri > 63 {
				ri = 63
			}
			if operator == opShiftLeft {
				return intValue(li << uint(ri)), nil
			}
			return intValue(li >> uint(ri)), nil
		}
	}
	return evalValue{}, errors.New("unsupported binary operator")
}

func checkedAdd(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}
func checkedSub(a, b int64) (int64, bool) {
	if b < 0 && a > math.MaxInt64+b || b > 0 && a < math.MinInt64+b {
		return 0, false
	}
	return a - b, true
}
func checkedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a == -1 && b == math.MinInt64 || b == -1 && a == math.MinInt64 {
		return 0, false
	}
	value := a * b
	return value, value/b == a
}

func equalValues(left, right evalValue) bool {
	if left.kind == valueBool && right.kind == valueBool {
		return left.boolean == right.boolean
	}
	if left.kind == valueInt && right.kind == valueInt {
		return left.integer == right.integer
	}
	return math.Abs(left.asFloat()-right.asFloat()) <= floatEqualityEpsilon
}

func laterTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

func worseStatus(left, right domain.StatusCode) domain.StatusCode {
	if statusPriority(right) > statusPriority(left) {
		return right
	}
	return left
}
func statusPriority(status domain.StatusCode) int {
	if status == domain.StatusGood {
		return 0
	}
	if status.IsUncertain() {
		return 1
	}
	switch status.BaseCode() {
	case domain.StatusBadWaitingInitialData:
		return 2
	case domain.StatusBadNoCommunication:
		return 3
	case domain.StatusBadConfigurationError:
		return 5
	case domain.StatusBadOutOfService:
		return 6
	default:
		if status.IsBad() {
			return 4
		}
		return 1
	}
}
