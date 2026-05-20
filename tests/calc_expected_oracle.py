#!/usr/bin/env python3
"""Independent oracle for calc.toml expected outputs.

This script intentionally does not call the Rust calculation engine. It parses
the formula language and evaluates calc.toml independently, then writes a CSV
golden table that Rust integration tests can compare against.
"""

from __future__ import annotations

import argparse
import csv
import math
import os
import struct
import tomllib
from collections import deque
from dataclasses import dataclass
from typing import Any


FLOAT_EQ_EPSILON = 1e-9
I64_MIN = -(2**63)
I64_MAX = 2**63 - 1
U32_MAX = 2**32 - 1
F64_MAX = float.fromhex("0x1.fffffffffffffp+1023")
F64_MIN = -F64_MAX


class EvalFailure(Exception):
    pass


class MissingVariable(EvalFailure):
    pass


class DivisionByZero(EvalFailure):
    pass


class TypeFailure(EvalFailure):
    pass


@dataclass(frozen=True)
class EvalValue:
    kind: str
    value: int | bool | float

    @staticmethod
    def int(value: int) -> "EvalValue":
        if value < I64_MIN or value > I64_MAX:
            raise TypeFailure(f"integer {value} outside i64 range")
        return EvalValue("int", int(value))

    @staticmethod
    def bool(value: bool) -> "EvalValue":
        return EvalValue("bool", bool(value))

    @staticmethod
    def float(value: float) -> "EvalValue":
        return EvalValue("float", float(value))

    def as_bool(self) -> bool:
        if self.kind == "bool":
            return bool(self.value)
        if self.kind == "int":
            return int(self.value) != 0
        return float(self.value) != 0.0

    def as_f64(self) -> float:
        if self.kind == "bool":
            return 1.0 if self.value else 0.0
        if self.kind == "int":
            return float(self.value)
        return float(self.value)

    def is_float(self) -> bool:
        return self.kind == "float"

    def is_invalid_float(self) -> bool:
        return self.kind == "float" and not math.isfinite(float(self.value))

    def as_int_for_integer_op(self) -> int:
        if self.kind == "int":
            return int(self.value)
        if self.kind == "bool":
            return 1 if self.value else 0
        value = float(self.value)
        if not math.isfinite(value):
            raise TypeFailure(f"float {value} is not finite")
        if math.trunc(value) != value:
            raise TypeFailure(f"float {value} is not an integer")
        if value < I64_MIN or value > I64_MAX:
            raise TypeFailure(f"float {value} outside i64 range")
        return int(value)


@dataclass(frozen=True)
class Token:
    kind: str
    value: Any = None


def tokenize(text: str) -> list[Token]:
    tokens: list[Token] = []
    pos = 0
    while pos < len(text):
        ch = text[pos]
        if ch.isspace():
            pos += 1
            continue

        if ch.isdigit() or (ch == "." and pos + 1 < len(text) and text[pos + 1].isdigit()):
            start = pos
            has_dot = False
            while pos < len(text) and (text[pos].isdigit() or text[pos] == "."):
                if text[pos] == ".":
                    if has_dot:
                        break
                    has_dot = True
                pos += 1
            if pos < len(text) and (text[pos].isalpha() or text[pos] == "_"):
                while pos < len(text) and (text[pos].isalnum() or text[pos] == "_"):
                    pos += 1
                tokens.append(Token("ident", text[start:pos]))
            else:
                raw = text[start:pos]
                tokens.append(Token("float" if has_dot else "int", float(raw) if has_dot else int(raw)))
            continue

        if ch.isalpha() or ch == "_":
            start = pos
            while pos < len(text) and (text[pos].isalnum() or text[pos] == "_"):
                pos += 1
            tokens.append(Token("ident", text[start:pos]))
            continue

        two = text[pos : pos + 2]
        if two in ("&&", "||", "==", "!=", "<=", ">=", "<<", ">>"):
            tokens.append(Token(two))
            pos += 2
            continue

        single = {
            "+": "+",
            "-": "-",
            "*": "*",
            "/": "/",
            "%": "%",
            "&": "&",
            "|": "|",
            "^": "^",
            "~": "~",
            "!": "!",
            "<": "<",
            ">": ">",
            "(": "(",
            ")": ")",
            ",": ",",
        }.get(ch)
        if single is None:
            raise ValueError(f"illegal character {ch!r} in expression {text!r}")
        tokens.append(Token(single))
        pos += 1

    tokens.append(Token("eof"))
    return tokens


INFIX_BP = {
    "||": (2, 3),
    "&&": (4, 5),
    "|": (6, 7),
    "^": (8, 9),
    "&": (10, 11),
    "==": (12, 13),
    "!=": (12, 13),
    "<": (14, 15),
    ">": (14, 15),
    "<=": (14, 15),
    ">=": (14, 15),
    "<<": (16, 17),
    ">>": (16, 17),
    "+": (18, 19),
    "-": (18, 19),
    "*": (20, 21),
    "/": (20, 21),
    "%": (20, 21),
}


class Parser:
    def __init__(self, tokens: list[Token]) -> None:
        self.tokens = tokens
        self.pos = 0

    def peek(self) -> Token:
        return self.tokens[self.pos] if self.pos < len(self.tokens) else Token("eof")

    def advance(self) -> Token:
        token = self.peek()
        self.pos += 1
        return token

    def parse(self) -> Any:
        expr = self.expr_bp(0)
        if self.peek().kind != "eof":
            raise ValueError(f"extra token after expression: {self.peek()}")
        return expr

    def expr_bp(self, min_bp: int) -> Any:
        token = self.advance()
        if token.kind == "int":
            lhs: Any = ("lit", EvalValue.int(token.value))
        elif token.kind == "float":
            lhs = ("lit", EvalValue.float(token.value))
        elif token.kind == "ident":
            lowered = str(token.value).lower()
            if lowered == "true":
                lhs = ("lit", EvalValue.bool(True))
            elif lowered == "false":
                lhs = ("lit", EvalValue.bool(False))
            elif lowered == "if" and self.peek().kind == "(":
                lhs = self.parse_if()
            else:
                lhs = ("var", token.value)
        elif token.kind == "(":
            lhs = self.expr_bp(0)
            if self.advance().kind != ")":
                raise ValueError("missing right parenthesis")
        elif token.kind in ("-", "!", "~"):
            lhs = ("unary", token.kind, self.expr_bp(23))
        else:
            raise ValueError(f"expected expression, got {token}")

        while True:
            op = self.peek().kind
            if op in ("eof", ")", ",") or op not in INFIX_BP:
                break
            left_bp, right_bp = INFIX_BP[op]
            if left_bp < min_bp:
                break
            self.advance()
            rhs = self.expr_bp(right_bp)
            lhs = ("binary", op, lhs, rhs)
        return lhs

    def parse_if(self) -> Any:
        if self.advance().kind != "(":
            raise ValueError("IF missing left parenthesis")
        condition = self.expr_bp(0)
        if self.advance().kind != ",":
            raise ValueError("IF missing first comma")
        then_branch = self.expr_bp(0)
        if self.advance().kind != ",":
            raise ValueError("IF missing second comma")
        else_branch = self.expr_bp(0)
        if self.advance().kind != ")":
            raise ValueError("IF missing right parenthesis or has too many args")
        return ("if", condition, then_branch, else_branch)


def parse_expression(expression: str) -> Any:
    return Parser(tokenize(expression)).parse()


def extract_variables(expr: Any) -> set[str]:
    tag = expr[0]
    if tag == "lit":
        return set()
    if tag == "var":
        return {expr[1]}
    if tag == "unary":
        return extract_variables(expr[2])
    if tag == "binary":
        return extract_variables(expr[2]) | extract_variables(expr[3])
    if tag == "if":
        return extract_variables(expr[1]) | extract_variables(expr[2]) | extract_variables(expr[3])
    raise ValueError(f"unknown AST node {tag}")


def checked_i64(value: int, message: str) -> EvalValue:
    if value < I64_MIN or value > I64_MAX:
        raise TypeFailure(message)
    return EvalValue.int(value)


def eval_expr(expr: Any, variables: dict[str, EvalValue]) -> EvalValue:
    tag = expr[0]
    if tag == "lit":
        return expr[1]
    if tag == "var":
        try:
            return variables[expr[1]]
        except KeyError as exc:
            raise MissingVariable(expr[1]) from exc
    if tag == "unary":
        op = expr[1]
        value = eval_expr(expr[2], variables)
        if op == "-":
            if value.kind == "float":
                return EvalValue.float(-float(value.value))
            return checked_i64(-value.as_int_for_integer_op(), "integer negation overflow")
        if op == "!":
            return EvalValue.bool(not value.as_bool())
        if op == "~":
            return EvalValue.int(~value.as_int_for_integer_op())
        raise ValueError(f"unknown unary operator {op}")
    if tag == "if":
        return eval_expr(expr[2], variables) if eval_expr(expr[1], variables).as_bool() else eval_expr(expr[3], variables)
    if tag == "binary":
        op = expr[1]
        left = eval_expr(expr[2], variables)
        if op == "&&":
            return EvalValue.bool(False) if not left.as_bool() else EvalValue.bool(eval_expr(expr[3], variables).as_bool())
        if op == "||":
            return EvalValue.bool(True) if left.as_bool() else EvalValue.bool(eval_expr(expr[3], variables).as_bool())
        right = eval_expr(expr[3], variables)
        return eval_binary(op, left, right)
    raise ValueError(f"unknown AST node {tag}")


def eval_binary(op: str, left: EvalValue, right: EvalValue) -> EvalValue:
    if op == "+":
        if left.is_float() or right.is_float():
            return EvalValue.float(left.as_f64() + right.as_f64())
        return checked_i64(left.as_int_for_integer_op() + right.as_int_for_integer_op(), "integer add overflow")
    if op == "-":
        if left.is_float() or right.is_float():
            return EvalValue.float(left.as_f64() - right.as_f64())
        return checked_i64(left.as_int_for_integer_op() - right.as_int_for_integer_op(), "integer sub overflow")
    if op == "*":
        if left.is_float() or right.is_float():
            return EvalValue.float(left.as_f64() * right.as_f64())
        return checked_i64(left.as_int_for_integer_op() * right.as_int_for_integer_op(), "integer mul overflow")
    if op == "/":
        if right.as_f64() == 0.0:
            raise DivisionByZero()
        return EvalValue.float(left.as_f64() / right.as_f64())
    if op == "%":
        if right.as_f64() == 0.0:
            raise DivisionByZero()
        if left.is_float() or right.is_float():
            return EvalValue.float(math.fmod(left.as_f64(), right.as_f64()))
        a = left.as_int_for_integer_op()
        b = right.as_int_for_integer_op()
        return EvalValue.int(a - math.trunc(a / b) * b)
    if op == "&":
        return EvalValue.int(left.as_int_for_integer_op() & right.as_int_for_integer_op())
    if op == "|":
        return EvalValue.int(left.as_int_for_integer_op() | right.as_int_for_integer_op())
    if op == "^":
        return EvalValue.int(left.as_int_for_integer_op() ^ right.as_int_for_integer_op())
    if op == "<<":
        shift = min(checked_shift(right.as_int_for_integer_op()), 63)
        return EvalValue.int(left.as_int_for_integer_op() << shift)
    if op == ">>":
        shift = min(checked_shift(right.as_int_for_integer_op()), 63)
        return EvalValue.int(left.as_int_for_integer_op() >> shift)
    if op == "==":
        return EvalValue.bool(eq_values(left, right))
    if op == "!=":
        return EvalValue.bool(not eq_values(left, right))
    if op == "<":
        return EvalValue.bool(left.as_f64() < right.as_f64())
    if op == ">":
        return EvalValue.bool(left.as_f64() > right.as_f64())
    if op == "<=":
        return EvalValue.bool(left.as_f64() <= right.as_f64())
    if op == ">=":
        return EvalValue.bool(left.as_f64() >= right.as_f64())
    raise ValueError(f"unknown binary operator {op}")


def checked_shift(value: int) -> int:
    if value < 0:
        raise TypeFailure(f"negative shift {value}")
    return value


def eq_values(left: EvalValue, right: EvalValue) -> bool:
    if left.kind == "bool" and right.kind == "bool":
        return bool(left.value) == bool(right.value)
    if left.kind == "int" and right.kind == "int":
        return int(left.value) == int(right.value)
    if left.kind == "float" and right.kind == "float":
        return abs(float(left.value) - float(right.value)) <= FLOAT_EQ_EPSILON
    return abs(left.as_f64() - right.as_f64()) <= FLOAT_EQ_EPSILON


def normalize_data_type(raw: str) -> str | None:
    lowered = raw.lower()
    if lowered in ("u32", "uint32", "uint"):
        return "uint"
    if lowered in ("bool", "boolean"):
        return "bool"
    if lowered in ("f32", "float"):
        return "float"
    if lowered in ("f64", "double"):
        return "double"
    return None


@dataclass
class CompiledRule:
    kks_calc: str
    data_type: str
    expression: str
    ast: Any
    variables: set[str]
    min_limit: float
    max_limit: float
    default_value: float


def compile_rules(raw_rules: list[dict[str, Any]]) -> list[CompiledRule]:
    compiled: list[CompiledRule] = []
    seen_outputs: set[str] = set()
    for raw in raw_rules:
        try:
            kks_calc = str(raw["kks_calc"])
            data_type = normalize_data_type(str(raw["data_type"]))
            if data_type is None:
                continue
            min_limit = float(raw.get("min_limit", F64_MIN))
            max_limit = float(raw.get("max_limit", F64_MAX))
            default_value = float(raw.get("default_value", 0.0))
            if not (math.isfinite(min_limit) and math.isfinite(max_limit) and math.isfinite(default_value)):
                continue
            if min_limit > max_limit:
                continue
            ast = parse_expression(str(raw["expression"]))
            variables = extract_variables(ast)
            if kks_calc in variables or kks_calc in seen_outputs:
                continue
            compiled.append(
                CompiledRule(
                    kks_calc=kks_calc,
                    data_type=data_type,
                    expression=str(raw["expression"]),
                    ast=ast,
                    variables=variables,
                    min_limit=min_limit,
                    max_limit=max_limit,
                    default_value=default_value,
                )
            )
            seen_outputs.add(kks_calc)
        except Exception:
            continue
    return topological_sort(compiled)


def topological_sort(rules: list[CompiledRule]) -> list[CompiledRule]:
    name_to_idx = {rule.kks_calc: idx for idx, rule in enumerate(rules)}
    adj = [[] for _ in rules]
    in_degree = [0 for _ in rules]
    for idx, rule in enumerate(rules):
        for variable in rule.variables:
            dep_idx = name_to_idx.get(variable)
            if dep_idx is not None:
                adj[dep_idx].append(idx)
                in_degree[idx] += 1
    queue = deque(idx for idx, degree in enumerate(in_degree) if degree == 0)
    order: list[int] = []
    while queue:
        idx = queue.popleft()
        order.append(idx)
        for next_idx in adj[idx]:
            in_degree[next_idx] -= 1
            if in_degree[next_idx] == 0:
                queue.append(next_idx)
    return [rules[idx] for idx in order]


def required_base_variables(rules: list[CompiledRule]) -> list[str]:
    outputs = {rule.kks_calc for rule in rules}
    base = sorted({variable for rule in rules for variable in rule.variables if variable not in outputs})
    return base


def run_cycle(rules: list[CompiledRule], base_variables: dict[str, EvalValue]) -> list[tuple[str, str, EvalValue]]:
    variables = dict(base_variables)
    results: list[tuple[str, str, EvalValue]] = []
    for rule in rules:
        try:
            raw_value = eval_expr(rule.ast, variables)
        except EvalFailure:
            raw_value = EvalValue.float(rule.default_value)
        safe_value = EvalValue.float(rule.default_value) if raw_value.is_invalid_float() else raw_value
        clamped = min(max(safe_value.as_f64(), rule.min_limit), rule.max_limit)
        output_value = convert_output(rule.data_type, safe_value, clamped)
        variables[rule.kks_calc] = output_to_eval(output_value)
        results.append((rule.kks_calc, rule.data_type, output_value))
    return results


def convert_output(data_type: str, safe_value: EvalValue, clamped: float) -> EvalValue:
    if data_type == "uint":
        if math.isnan(clamped) or clamped <= 0.0:
            return EvalValue.int(0)
        if clamped >= U32_MAX:
            return EvalValue.int(U32_MAX)
        return EvalValue.int(int(clamped))
    if data_type == "bool":
        if safe_value.kind == "bool" and clamped == safe_value.as_f64():
            return EvalValue.bool(bool(safe_value.value))
        return EvalValue.bool(clamped != 0.0)
    if data_type == "float":
        return EvalValue.float(to_f32(clamped))
    if data_type == "double":
        return EvalValue.float(clamped)
    raise ValueError(f"unknown data type {data_type}")


def output_to_eval(value: EvalValue) -> EvalValue:
    return value


def to_f32(value: float) -> float:
    return struct.unpack("!f", struct.pack("!f", float(value)))[0]


def make_scenarios(base_variables: list[str]) -> list[tuple[str, dict[str, EvalValue]]]:
    def fill(value: int) -> dict[str, EvalValue]:
        return {name: EvalValue.int(value) for name in base_variables}

    all_one = fill(1)
    all_zero = fill(0)
    ready_true = fill(1)
    ready_true["9CYE91GH201_READY"] = EvalValue.int(1)
    ready_true["9CYE91GH201_SL3"] = EvalValue.int(13)
    ready_false = fill(1)
    ready_false["9CYE91GH201_READY"] = EvalValue.int(0)
    ready_false["9CYE91GH201_SL3"] = EvalValue.int(13)
    patterned = {
        name: EvalValue.int((sum(name.encode("utf-8")) % 2) if name.endswith("_READY") else (sum(name.encode("utf-8")) % 17))
        for name in base_variables
    }
    return [
        ("all_one", all_one),
        ("all_zero", all_zero),
        ("ready_gate_true", ready_true),
        ("ready_gate_false", ready_false),
        ("patterned", patterned),
    ]


def value_type(value: EvalValue, data_type: str) -> str:
    if data_type == "uint":
        return "uint"
    if data_type == "bool":
        return "bool"
    if data_type == "float":
        return "float"
    if data_type == "double":
        return "double"
    return value.kind


def value_text(value: EvalValue, data_type: str) -> str:
    if data_type == "bool":
        return "true" if value.as_bool() else "false"
    if data_type == "uint":
        return str(value.as_int_for_integer_op())
    return format(value.as_f64(), ".17g")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--calc", required=True, help="Path to calc.toml")
    parser.add_argument("--output", required=True, help="Path to expected output CSV")
    args = parser.parse_args()

    with open(args.calc, "rb") as handle:
        data = tomllib.load(handle)
    raw_rules = data.get("rules", [])
    rules = compile_rules(raw_rules)
    base_variables = required_base_variables(rules)
    scenarios = make_scenarios(base_variables)

    os.makedirs(os.path.dirname(os.path.abspath(args.output)), exist_ok=True)
    row_count = 0
    with open(args.output, "w", newline="", encoding="utf-8") as handle:
        writer = csv.writer(handle)
        writer.writerow(["scenario", "kks_calc", "data_type", "value_type", "expected"])
        for scenario_name, inputs in scenarios:
            for kks_calc, data_type, value in run_cycle(rules, inputs):
                writer.writerow([scenario_name, kks_calc, data_type, value_type(value, data_type), value_text(value, data_type)])
                row_count += 1

    print(
        f"generated {row_count} rows: rules={len(rules)}, "
        f"base_variables={len(base_variables)}, scenarios={len(scenarios)}, output={args.output}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
