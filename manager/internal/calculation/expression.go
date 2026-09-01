package calculation

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type tokenKind uint8

const (
	tokenEOF tokenKind = iota
	tokenInt
	tokenFloat
	tokenIdent
	tokenPlus
	tokenMinus
	tokenStar
	tokenSlash
	tokenPercent
	tokenAmp
	tokenPipe
	tokenCaret
	tokenTilde
	tokenShiftLeft
	tokenShiftRight
	tokenAnd
	tokenOr
	tokenBang
	tokenEq
	tokenNotEq
	tokenLt
	tokenGt
	tokenLtEq
	tokenGtEq
	tokenLParen
	tokenRParen
	tokenComma
)

type token struct {
	kind tokenKind
	text string
	i64  int64
	f64  float64
}

func tokenize(input string) ([]token, error) {
	chars := []rune(input)
	tokens := make([]token, 0, min(len(chars)/2+1, MaxTokens+1))
	appendToken := func(value token) error {
		tokens = append(tokens, value)
		if len(tokens) > MaxTokens {
			return fmt.Errorf("expression contains more than %d tokens", MaxTokens)
		}
		return nil
	}
	for pos := 0; pos < len(chars); {
		ch := chars[pos]
		if unicode.IsSpace(ch) && ch <= unicode.MaxASCII {
			pos++
			continue
		}
		if isASCIIDigit(ch) || ch == '.' && pos+1 < len(chars) && isASCIIDigit(chars[pos+1]) {
			start, hasDot := pos, false
			for pos < len(chars) && (isASCIIDigit(chars[pos]) || chars[pos] == '.') {
				if chars[pos] == '.' {
					if hasDot {
						break
					}
					hasDot = true
				}
				pos++
			}
			if pos < len(chars) && (isASCIIAlpha(chars[pos]) || chars[pos] == '_') {
				for pos < len(chars) && (isASCIIAlpha(chars[pos]) || isASCIIDigit(chars[pos]) || chars[pos] == '_') {
					pos++
				}
				if err := appendToken(token{kind: tokenIdent, text: string(chars[start:pos])}); err != nil {
					return nil, err
				}
				continue
			}
			text := string(chars[start:pos])
			if hasDot {
				value, err := strconv.ParseFloat(text, 64)
				if err != nil {
					return nil, fmt.Errorf("cannot parse float %q: %w", text, err)
				}
				if err := appendToken(token{kind: tokenFloat, text: text, f64: value}); err != nil {
					return nil, err
				}
			} else {
				value, err := strconv.ParseInt(text, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("cannot parse integer %q: %w", text, err)
				}
				if err := appendToken(token{kind: tokenInt, text: text, i64: value}); err != nil {
					return nil, err
				}
			}
			continue
		}
		if isASCIIAlpha(ch) || ch == '_' {
			start := pos
			for pos < len(chars) && (isASCIIAlpha(chars[pos]) || isASCIIDigit(chars[pos]) || chars[pos] == '_') {
				pos++
			}
			if err := appendToken(token{kind: tokenIdent, text: string(chars[start:pos])}); err != nil {
				return nil, err
			}
			continue
		}
		kind, width := tokenKind(0), 1
		switch ch {
		case '+':
			kind = tokenPlus
		case '-':
			kind = tokenMinus
		case '*':
			kind = tokenStar
		case '/':
			kind = tokenSlash
		case '%':
			kind = tokenPercent
		case '^':
			kind = tokenCaret
		case '~':
			kind = tokenTilde
		case '(':
			kind = tokenLParen
		case ')':
			kind = tokenRParen
		case ',':
			kind = tokenComma
		case '&':
			kind = tokenAmp
			if pos+1 < len(chars) && chars[pos+1] == '&' {
				kind, width = tokenAnd, 2
			}
		case '|':
			kind = tokenPipe
			if pos+1 < len(chars) && chars[pos+1] == '|' {
				kind, width = tokenOr, 2
			}
		case '!':
			kind = tokenBang
			if pos+1 < len(chars) && chars[pos+1] == '=' {
				kind, width = tokenNotEq, 2
			}
		case '=':
			if pos+1 >= len(chars) || chars[pos+1] != '=' {
				return nil, fmt.Errorf("standalone '=' at position %d; use '=='", pos)
			}
			kind, width = tokenEq, 2
		case '<':
			kind = tokenLt
			if pos+1 < len(chars) && chars[pos+1] == '=' {
				kind, width = tokenLtEq, 2
			} else if pos+1 < len(chars) && chars[pos+1] == '<' {
				kind, width = tokenShiftLeft, 2
			}
		case '>':
			kind = tokenGt
			if pos+1 < len(chars) && chars[pos+1] == '=' {
				kind, width = tokenGtEq, 2
			} else if pos+1 < len(chars) && chars[pos+1] == '>' {
				kind, width = tokenShiftRight, 2
			}
		default:
			return nil, fmt.Errorf("illegal character %q at position %d", ch, pos)
		}
		if err := appendToken(token{kind: kind, text: string(chars[pos : pos+width])}); err != nil {
			return nil, err
		}
		pos += width
	}
	// EOF is a parser sentinel, not one of the configured lexical units.
	tokens = append(tokens, token{kind: tokenEOF})
	return tokens, nil
}

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }
func isASCIIAlpha(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }

type binaryOp uint8

const (
	opAdd binaryOp = iota
	opSub
	opMul
	opDiv
	opMod
	opBitAnd
	opBitOr
	opBitXor
	opShiftLeft
	opShiftRight
	opAnd
	opOr
	opEq
	opNotEq
	opLt
	opGt
	opLtEq
	opGtEq
)

type unaryOp uint8

const (
	opNeg unaryOp = iota
	opNot
	opBitNot
)

type exprKind uint8

const (
	exprLiteral exprKind = iota
	exprVariable
	exprBinary
	exprUnary
	exprIf
)

type expression struct {
	kind                              exprKind
	value                             evalValue
	name                              string
	binary                            binaryOp
	unary                             unaryOp
	left, right                       *expression
	condition, thenBranch, elseBranch *expression
}

type expressionParser struct {
	tokens []token
	pos    int
}

func parseExpression(text string) (*expression, []string, error) {
	tokens, err := tokenize(text)
	if err != nil {
		return nil, nil, err
	}
	parser := expressionParser{tokens: tokens}
	ast, err := parser.parse(0)
	if err != nil {
		return nil, nil, err
	}
	if parser.peek().kind != tokenEOF {
		return nil, nil, fmt.Errorf("extra token %q after expression", parser.peek().text)
	}
	variables := make(map[string]struct{})
	collectVariables(ast, variables)
	names := make([]string, 0, len(variables))
	for name := range variables {
		names = append(names, name)
	}
	sort.Strings(names)
	return ast, names, nil
}

func (p *expressionParser) peek() token {
	if p.pos >= len(p.tokens) {
		return token{kind: tokenEOF}
	}
	return p.tokens[p.pos]
}
func (p *expressionParser) advance() token { value := p.peek(); p.pos++; return value }

func (p *expressionParser) parse(minBP int) (*expression, error) {
	tok := p.advance()
	var left *expression
	switch tok.kind {
	case tokenInt:
		left = &expression{kind: exprLiteral, value: intValue(tok.i64)}
	case tokenFloat:
		left = &expression{kind: exprLiteral, value: floatValue(tok.f64)}
	case tokenIdent:
		switch {
		case strings.EqualFold(tok.text, "true"):
			left = &expression{kind: exprLiteral, value: boolValue(true)}
		case strings.EqualFold(tok.text, "false"):
			left = &expression{kind: exprLiteral, value: boolValue(false)}
		case strings.EqualFold(tok.text, "if") && p.peek().kind == tokenLParen:
			var err error
			left, err = p.parseIf()
			if err != nil {
				return nil, err
			}
		default:
			left = &expression{kind: exprVariable, name: tok.text}
		}
	case tokenLParen:
		var err error
		left, err = p.parse(0)
		if err != nil {
			return nil, err
		}
		if p.advance().kind != tokenRParen {
			return nil, fmt.Errorf("missing right parenthesis")
		}
	case tokenMinus, tokenBang, tokenTilde:
		op := opNeg
		if tok.kind == tokenBang {
			op = opNot
		} else if tok.kind == tokenTilde {
			op = opBitNot
		}
		operand, err := p.parse(23)
		if err != nil {
			return nil, err
		}
		left = &expression{kind: exprUnary, unary: op, left: operand}
	default:
		return nil, fmt.Errorf("expected expression, got %q", tok.text)
	}
	for {
		tok = p.peek()
		if tok.kind == tokenEOF || tok.kind == tokenRParen || tok.kind == tokenComma {
			break
		}
		op, ok := tokenBinary(tok.kind)
		if !ok {
			break
		}
		lbp, rbp := bindingPower(op)
		if lbp < minBP {
			break
		}
		p.advance()
		right, err := p.parse(rbp)
		if err != nil {
			return nil, err
		}
		left = &expression{kind: exprBinary, binary: op, left: left, right: right}
	}
	return left, nil
}

func (p *expressionParser) parseIf() (*expression, error) {
	if p.advance().kind != tokenLParen {
		return nil, fmt.Errorf("IF missing '('")
	}
	condition, err := p.parse(0)
	if err != nil {
		return nil, err
	}
	if p.advance().kind != tokenComma {
		return nil, fmt.Errorf("IF first argument missing comma")
	}
	thenBranch, err := p.parse(0)
	if err != nil {
		return nil, err
	}
	if p.advance().kind != tokenComma {
		return nil, fmt.Errorf("IF second argument missing comma")
	}
	elseBranch, err := p.parse(0)
	if err != nil {
		return nil, err
	}
	if p.advance().kind != tokenRParen {
		return nil, fmt.Errorf("IF missing ')' or has too many arguments")
	}
	return &expression{kind: exprIf, condition: condition, thenBranch: thenBranch, elseBranch: elseBranch}, nil
}

func tokenBinary(kind tokenKind) (binaryOp, bool) {
	switch kind {
	case tokenPlus:
		return opAdd, true
	case tokenMinus:
		return opSub, true
	case tokenStar:
		return opMul, true
	case tokenSlash:
		return opDiv, true
	case tokenPercent:
		return opMod, true
	case tokenAmp:
		return opBitAnd, true
	case tokenPipe:
		return opBitOr, true
	case tokenCaret:
		return opBitXor, true
	case tokenShiftLeft:
		return opShiftLeft, true
	case tokenShiftRight:
		return opShiftRight, true
	case tokenAnd:
		return opAnd, true
	case tokenOr:
		return opOr, true
	case tokenEq:
		return opEq, true
	case tokenNotEq:
		return opNotEq, true
	case tokenLt:
		return opLt, true
	case tokenGt:
		return opGt, true
	case tokenLtEq:
		return opLtEq, true
	case tokenGtEq:
		return opGtEq, true
	default:
		return 0, false
	}
}

func bindingPower(op binaryOp) (int, int) {
	switch op {
	case opOr:
		return 2, 3
	case opAnd:
		return 4, 5
	case opBitOr:
		return 6, 7
	case opBitXor:
		return 8, 9
	case opBitAnd:
		return 10, 11
	case opEq, opNotEq:
		return 12, 13
	case opLt, opGt, opLtEq, opGtEq:
		return 14, 15
	case opShiftLeft, opShiftRight:
		return 16, 17
	case opAdd, opSub:
		return 18, 19
	default:
		return 20, 21
	}
}

func collectVariables(value *expression, output map[string]struct{}) {
	if value == nil {
		return
	}
	switch value.kind {
	case exprVariable:
		output[value.name] = struct{}{}
	case exprUnary:
		collectVariables(value.left, output)
	case exprBinary:
		collectVariables(value.left, output)
		collectVariables(value.right, output)
	case exprIf:
		collectVariables(value.condition, output)
		collectVariables(value.thenBranch, output)
		collectVariables(value.elseBranch, output)
	}
}
