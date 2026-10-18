package kconfig

import (
	"fmt"
	"strconv"
	"strings"
)

// Op is the kind of an Expr node.
type Op uint8

const (
	OpSym Op = iota // a symbol, or a constant: y, m, n, a number or a quoted string
	OpNot           // 2 - L
	OpAnd           // min(L, R)
	OpOr            // max(L, R)
	OpNo            // y when L is n, else n ("depends on X if Y" tests Y = n)
	OpEq            // L and R are OpSym or OpOpaque operands
	OpNeq
	OpLt
	OpLe
	OpGt
	OpGe
	OpOpaque // a macro call or text that could not be parsed: n or y
)

// modulesRef stands for the symbol declared with "modules"; a constant m
// in a condition means m && MODULES.
const modulesRef = "\x00MODULES"

// Expr is a Kconfig expression in its own tristate algebra.
type Expr struct {
	Op     Op
	Name   string // OpSym; raw text for OpOpaque
	Quoted bool   // OpSym: a quoted constant
	L, R   *Expr
}

func symExpr(name string) *Expr { return &Expr{Op: OpSym, Name: name} }

// and returns the conjunction of the non-nil arguments; nil stands for y.
func and(es ...*Expr) *Expr {
	var out *Expr
	for _, e := range es {
		switch {
		case e == nil:
		case out == nil:
			out = e
		default:
			out = &Expr{Op: OpAnd, L: out, R: e}
		}
	}
	return out
}

func or(a, b *Expr) *Expr {
	if a == nil || b == nil {
		return nil
	}
	return &Expr{Op: OpOr, L: a, R: b}
}

// rewriteM replaces the constant m in a condition by m && MODULES, as
// kconfig does for dependencies and property conditions.
func rewriteM(e *Expr) *Expr {
	if e == nil {
		return nil
	}
	switch e.Op {
	case OpSym:
		if e.Name == "m" {
			return and(e, symExpr(modulesRef))
		}
	case OpNot, OpNo:
		return &Expr{Op: e.Op, L: rewriteM(e.L)}
	case OpAnd, OpOr:
		return &Expr{Op: e.Op, L: rewriteM(e.L), R: rewriteM(e.R)}
	}
	return e
}

var cmpOps = map[Op]string{OpEq: "=", OpNeq: "!=", OpLt: "<", OpLe: "<=", OpGt: ">", OpGe: ">="}

// String renders e in Kconfig syntax; nil is y.
func (e *Expr) String() string {
	if e == nil {
		return "y"
	}
	switch e.Op {
	case OpSym:
		if e.Name == modulesRef {
			return "MODULES"
		}
		if e.Quoted {
			return strconv.Quote(e.Name)
		}
		return e.Name
	case OpOpaque:
		return e.Name
	case OpNot:
		return "!" + e.L.operand(OpNot)
	case OpNo:
		return e.L.operand(OpEq) + " = n"
	case OpAnd:
		return e.L.operand(OpAnd) + " && " + e.R.operand(OpAnd)
	case OpOr:
		return e.L.operand(OpOr) + " || " + e.R.operand(OpOr)
	}
	return e.L.String() + " " + cmpOps[e.Op] + " " + e.R.String()
}

// prec orders operators from loosest to tightest binding.
func prec(op Op) int {
	switch op {
	case OpOr:
		return 1
	case OpAnd:
		return 2
	case OpSym, OpOpaque:
		return 5
	case OpNot:
		return 4
	}
	return 3 // comparisons and OpNo
}

// operand renders e as an operand of parent, adding parentheses when e
// binds more loosely.
func (e *Expr) operand(parent Op) string {
	p, q := prec(e.Op), prec(parent)
	if p < q || (p == q && p == 3) || (parent == OpNot && p < 4) {
		return "(" + e.String() + ")"
	}
	return e.String()
}

// token is a lexical element of an expression.
type token struct {
	kind byte // 'w' word, 'q' quoted string, '$' macro call, 'o' operator
	text string
}

// lex splits an expression into tokens. Macro calls whose expansion is
// unknown stay single '$' tokens.
func lex(s string) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '"' || c == '\'':
			j, text, err := quoted(s, i)
			if err != nil {
				return nil, err
			}
			kind := byte('q')
			if strings.Contains(text, "$(") {
				kind = '$' // an unknown expansion inside quotes
			}
			out = append(out, token{kind, text})
			i = j
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			j := macroEnd(s, i)
			if j < 0 {
				return nil, fmt.Errorf("unterminated macro in %q", s)
			}
			out = append(out, token{'$', s[i:j]})
			i = j
		case isWordByte(c) || c == '-':
			j := i + 1
			for j < len(s) && (isWordByte(s[j]) || s[j] == '-') {
				j++
			}
			out = append(out, token{'w', s[i:j]})
			i = j
		default:
			op := string(c)
			if i+1 < len(s) {
				switch two := s[i : i+2]; two {
				case "&&", "||", "!=", "<=", ">=":
					op = two
				}
			}
			switch op {
			case "&&", "||", "!=", "<=", ">=", "!", "=", "<", ">", "(", ")":
			default:
				return nil, fmt.Errorf("unexpected %q in %q", op, s)
			}
			out = append(out, token{'o', op})
			i += len(op)
		}
	}
	return out, nil
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// quoted reads the quoted string starting at s[i] and returns the index
// after it and its unescaped text.
func quoted(s string, i int) (int, string, error) {
	q := s[i]
	var b strings.Builder
	for j := i + 1; j < len(s); j++ {
		if k := macroEnd(s, j); k > 0 {
			// Quotes inside a macro call do not end the string.
			b.WriteString(s[j:k])
			j = k - 1
			continue
		}
		switch s[j] {
		case '\\':
			if j+1 < len(s) {
				j++
				b.WriteByte(s[j])
			}
		case q:
			return j + 1, b.String(), nil
		default:
			b.WriteByte(s[j])
		}
	}
	return 0, "", fmt.Errorf("unterminated string in %q", s)
}

// macroEnd returns the index after the macro call starting at s[i], or -1
// when none starts there or it is unterminated.
func macroEnd(s string, i int) int {
	if !strings.HasPrefix(s[i:], "$(") {
		return -1
	}
	depth := 0
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return j + 1
			}
		}
	}
	return -1
}

// parseExpr parses a Kconfig expression.
func parseExpr(s string) (*Expr, error) {
	toks, err := lex(s)
	if err != nil {
		return nil, err
	}
	p := &exprParser{toks: toks, src: s}
	e, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("unexpected %q in %q", p.toks[p.pos].text, s)
	}
	return e, nil
}

type exprParser struct {
	toks []token
	pos  int
	src  string
}

func (p *exprParser) accept(op string) bool {
	if p.pos < len(p.toks) && p.toks[p.pos].kind == 'o' && p.toks[p.pos].text == op {
		p.pos++
		return true
	}
	return false
}

func (p *exprParser) or() (*Expr, error) {
	l, err := p.and()
	for err == nil && p.accept("||") {
		var r *Expr
		if r, err = p.and(); err == nil {
			l = &Expr{Op: OpOr, L: l, R: r}
		}
	}
	return l, err
}

func (p *exprParser) and() (*Expr, error) {
	l, err := p.unary()
	for err == nil && p.accept("&&") {
		var r *Expr
		if r, err = p.unary(); err == nil {
			l = &Expr{Op: OpAnd, L: l, R: r}
		}
	}
	return l, err
}

func (p *exprParser) unary() (*Expr, error) {
	if p.accept("!") {
		e, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &Expr{Op: OpNot, L: e}, nil
	}
	if p.accept("(") {
		e, err := p.or()
		if err != nil {
			return nil, err
		}
		if !p.accept(")") {
			return nil, fmt.Errorf("missing ) in %q", p.src)
		}
		return e, nil
	}
	l, err := p.operand()
	if err != nil {
		return nil, err
	}
	for op, text := range cmpOps {
		if p.accept(text) {
			r, err := p.operand()
			if err != nil {
				return nil, err
			}
			return &Expr{Op: op, L: l, R: r}, nil
		}
	}
	return l, nil
}

func (p *exprParser) operand() (*Expr, error) {
	if p.pos >= len(p.toks) {
		return nil, fmt.Errorf("unexpected end of %q", p.src)
	}
	t := p.toks[p.pos]
	p.pos++
	switch t.kind {
	case 'w':
		return symExpr(t.text), nil
	case 'q':
		return &Expr{Op: OpSym, Name: t.text, Quoted: true}, nil
	case '$':
		return &Expr{Op: OpOpaque, Name: t.text}, nil
	}
	return nil, fmt.Errorf("unexpected %q in %q", t.text, p.src)
}
