package kcond

import (
	"strings"
	"unicode/utf8"
)

// analyzeRust computes the regions and code lines of a Rust source from
// its #[cfg(...)] attributes: the condition of an attribute applies to its
// lines and to the item, field, variant, arm or statement that follows,
// and nested attributes AND. CONFIG_X holds when the option is y or m, as
// rustc_cfg sets it, CONFIG_X = "y" and "m" when it is built in or a
// module; not(), all() and any() are read; test, doctest and doc never
// hold in the kernel build, MODULE holds in a module as for C, and any
// other predicate is an opaque atom.
// cfg_attr only changes other attributes, and cfg!() is an expression,
// so neither is a region. Rust has no C guards.
func analyzeRust(src []byte) *Source {
	lines := strings.Split(string(src), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	toks := lexRust(string(src), len(lines))
	s := &Source{Cpp: make([]*Expr, len(lines)), Guard: make([]*Expr, len(lines)), Code: make([]bool, len(lines))}
	for i := range lines {
		s.Guard[i] = Const(true)
		s.Code[i] = len(toks[i]) > 0
	}
	cfgRegions(toks, s.Cpp)
	return s
}

// rtoken is a Rust token: an identifier, keyword or literal (ident), or
// punctuation. A string literal keeps its quotes.
type rtoken struct {
	text  string
	ident bool
}

// rustPunct are the multi-character operators that matter to the region
// scan: -> and => are not angle brackets, and >> closes two of them.
var rustPunct = []string{"->", "=>", "<<", ">>", "<=", ">=", "::", "==", "!=", ".."}

// lexRust splits src into tokens by line, without comments; a literal that
// spans lines belongs to the line it starts on. Lines beyond n are dropped.
func lexRust(src string, n int) [][]rtoken {
	toks := make([][]rtoken, n)
	line := 0
	s := src
	// skip consumes s[:end], counting the lines it spans; take keeps it as
	// a token of the line it starts on.
	skip := func(end int) {
		line += strings.Count(s[:end], "\n")
		s = s[end:]
	}
	take := func(end int, ident bool) {
		if line < n {
			toks[line] = append(toks[line], rtoken{s[:end], ident})
		}
		skip(end)
	}
	for s != "" {
		c := s[0]
		switch {
		case c == '\n':
			line++
			s = s[1:]
		case c == ' ' || c == '\t' || c == '\r':
			s = s[1:]
		case strings.HasPrefix(s, "//"):
			end := strings.IndexByte(s, '\n')
			if end < 0 {
				end = len(s)
			}
			skip(end)
		case strings.HasPrefix(s, "/*"):
			skip(blockComment(s))
		case c == '"' || (c == 'b' || c == 'c') && strings.HasPrefix(s[1:], "\""):
			take(rustString(s), true)
		case rawString(s) > 0:
			take(rawString(s), true)
		case c == '\'':
			// A character literal, or a lifetime such as 'static.
			if end := rustChar(s); end > 0 {
				take(end, true)
			} else {
				take(identEnd(s), true)
			}
		case isIdentByte(c):
			take(identEnd(s), true)
		default:
			end := 1
			for _, p := range rustPunct {
				if strings.HasPrefix(s, p) {
					end = len(p)
					break
				}
			}
			take(end, false)
		}
	}
	return toks
}

// identEnd is the length of the identifier (or lifetime) s starts with.
func identEnd(s string) int {
	return 1 + len(firstIdent(s[1:]))
}

// blockComment is the length of the block comment s starts with; they
// nest in Rust.
func blockComment(s string) int {
	depth, i := 0, 0
	for i < len(s) {
		switch {
		case strings.HasPrefix(s[i:], "/*"):
			depth, i = depth+1, i+2
		case strings.HasPrefix(s[i:], "*/"):
			if depth, i = depth-1, i+2; depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(s)
}

// rustString is the length of the string literal s starts with, after an
// optional b or c prefix.
func rustString(s string) int {
	for i := strings.IndexByte(s, '"') + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(s)
}

// rawString is the length of the raw string literal s starts with: r, br
// or cr, hashes, a quote, and the closing quote followed by as many
// hashes; 0 when s starts with none.
func rawString(s string) int {
	i := 0
	if i < len(s) && (s[i] == 'b' || s[i] == 'c') {
		i++
	}
	if i >= len(s) || s[i] != 'r' {
		return 0
	}
	hashes := 0
	for i++; i < len(s) && s[i] == '#'; i++ {
		hashes++
	}
	if i >= len(s) || s[i] != '"' {
		return 0
	}
	closer := "\"" + strings.Repeat("#", hashes)
	if end := strings.Index(s[i+1:], closer); end >= 0 {
		return i + 1 + end + len(closer)
	}
	return len(s)
}

// rustChar is the length of the character literal s starts with, 0 when
// the quote starts a lifetime.
func rustChar(s string) int {
	if len(s) < 3 {
		return 0
	}
	if s[1] == '\\' {
		// The escaped character may itself be a quote, as in '\''.
		if end := strings.IndexByte(s[3:], '\''); end >= 0 {
			return end + 4
		}
		return 0
	}
	_, size := utf8.DecodeRuneInString(s[1:])
	if len(s) > 1+size && s[1+size] == '\'' {
		return 2 + size
	}
	return 0
}

// cfgScope is an item a #[cfg] attribute conditions, until it ends.
type cfgScope struct {
	cond    *Expr
	depth   int  // the bracket depth the item is at
	started bool // its first token was seen; before it, attributes AND in
	braced  bool // a { opened at depth: the matching } ends the item
	angle   int  // unclosed < since the item started, where , is no end
	where   bool // in a where clause, where , is no end either
	inner   bool // a #![cfg], which lasts to the end of the enclosing block
}

// cfgRegions fills conds, one per line, from the #[cfg] attributes of
// toks: a line is compiled where any of its tokens is, an attribute with
// its item. An item ends at the first ; or , at its depth, , outside
// generics and where clauses, at the } closing a { opened at its depth, or
// before the bracket that closes the construct around it. shortcut: an
// if/else statement ends at its first }, which leaves the else branches
// out; upgrade if a cfg'd else branch is ever misjudged.
func cfgRegions(toks [][]rtoken, conds []*Expr) {
	var stack []*cfgScope
	active := func() *Expr {
		if len(stack) == 0 {
			return Const(true)
		}
		return stack[len(stack)-1].cond
	}
	record := func(li int, c *Expr) {
		if conds[li] == nil {
			conds[li] = c
		} else {
			conds[li] = Or(conds[li], c)
		}
	}
	depth := 0
	var attr []rtoken // the attribute being read, after its #
	attrStart, attrDepth, inAttr := 0, 0, false
	for li, line := range toks {
		if len(line) == 0 {
			conds[li] = active()
		}
		for ti, t := range line {
			if inAttr {
				switch t.text {
				case "[", "(", "{":
					attrDepth++
				case "]", ")", "}":
					if attrDepth--; attrDepth == 0 {
						inAttr = false
						if e, inner, ok := cfgAttr(attr); ok {
							if top := len(stack) - 1; top >= 0 && !stack[top].started {
								stack[top].cond = And(stack[top].cond, e)
							} else {
								stack = append(stack, &cfgScope{cond: And(active(), e), depth: depth, started: inner, inner: inner})
							}
						}
						for j := attrStart; j <= li; j++ {
							record(j, active())
						}
						continue
					}
				}
				attr = append(attr, t)
				continue
			}
			if t.text == "#" && ti+1 < len(line) && (line[ti+1].text == "[" || line[ti+1].text == "!") {
				inAttr, attrStart, attrDepth, attr = true, li, 0, attr[:0]
				continue
			}
			var top *cfgScope
			if len(stack) > 0 {
				top = stack[len(stack)-1]
				top.started = true
			}
			if t.text != ")" && t.text != "]" && t.text != "}" {
				record(li, active())
			}
			switch t.text {
			case "(", "[", "{":
				if top != nil && !top.inner && t.text == "{" && depth == top.depth {
					top.braced, top.where = true, false
				}
				depth++
			case ")", "]", "}":
				depth--
				for len(stack) > 0 && depth < stack[len(stack)-1].depth {
					stack = stack[:len(stack)-1]
				}
				record(li, active())
				if len(stack) > 0 {
					if top = stack[len(stack)-1]; !top.inner && t.text == "}" && top.braced && depth == top.depth {
						stack = stack[:len(stack)-1]
					}
				}
			case ";", ",":
				if top != nil && !top.inner && depth == top.depth && (t.text == ";" || top.angle == 0 && !top.where) {
					stack = stack[:len(stack)-1]
				}
			case "<", "<<":
				if top != nil && depth == top.depth {
					top.angle += len(t.text)
				}
			case ">", ">>":
				if top != nil && depth == top.depth {
					top.angle = max(0, top.angle-len(t.text))
				}
			case "=>":
				// A match arm's pattern and guard end here, so a < in
				// the guard opened no generics.
				if top != nil && depth == top.depth {
					top.angle = 0
				}
			case "where":
				if top != nil && depth == top.depth {
					top.where = true
				}
			}
		}
	}
	// An attribute left open at the end of the file recorded none of its
	// lines.
	for i, c := range conds {
		if c == nil {
			conds[i] = Const(true)
		}
	}
}

// cfgAttr reads the tokens of an attribute after its #, without the
// closing bracket: a cfg(...) gives its condition, and inner tells
// whether it is a #![cfg].
func cfgAttr(attr []rtoken) (e *Expr, inner, ok bool) {
	if len(attr) > 0 && attr[0].text == "!" {
		inner, attr = true, attr[1:]
	}
	if len(attr) < 4 || attr[0].text != "[" || attr[1].text != "cfg" || attr[2].text != "(" || attr[len(attr)-1].text != ")" {
		return nil, false, false
	}
	return cfgPred(attr[3 : len(attr)-1]), inner, true
}

// RustCfg reads the predicate of a Rust cfg attribute, as the region of
// the lines under #[cfg(pred)] (see analyzeRust).
func RustCfg(pred string) *Expr {
	var toks []rtoken
	for _, line := range lexRust(pred, strings.Count(pred, "\n")+1) {
		toks = append(toks, line...)
	}
	return cfgPred(toks)
}

// cfgPred reads the tokens of a cfg predicate, an opaque atom when they
// do not parse.
func cfgPred(toks []rtoken) *Expr {
	p := &cfgParser{toks: toks}
	e := p.pred()
	if p.pos < len(p.toks) || p.bad {
		e = cfgAtom(p.toks)
	}
	return e
}

// cfgParser reads a cfg predicate.
type cfgParser struct {
	toks []rtoken
	pos  int
	bad  bool
}

func (p *cfgParser) next() string {
	if p.pos >= len(p.toks) {
		p.bad = true
		return ""
	}
	p.pos++
	return p.toks[p.pos-1].text
}

func (p *cfgParser) peek() string {
	if p.pos >= len(p.toks) {
		return ""
	}
	return p.toks[p.pos].text
}

// pred reads one predicate: not(p), all(p, ...), any(p, ...), or a name,
// with the string it is compared to when there is one.
func (p *cfgParser) pred() *Expr {
	start := p.pos
	name := p.next()
	if (name == "not" || name == "all" || name == "any") && p.peek() == "(" {
		p.next()
		var args []*Expr
		for p.peek() != ")" && !p.bad {
			args = append(args, p.pred())
			if p.peek() == "," {
				p.next()
			}
		}
		p.next()
		switch name {
		case "not":
			if len(args) != 1 {
				p.bad = true
				return Const(false)
			}
			return Not(args[0])
		case "all":
			return And(args...)
		}
		return Or(args...)
	}
	value, compared := "", false
	if p.peek() == "=" {
		p.next()
		if v := p.next(); strings.HasPrefix(v, "\"") {
			value, compared = strings.Trim(v, "\""), true
		} else {
			p.bad = true
		}
	}
	toks := p.toks[start:p.pos]
	switch {
	case !IsSymbol(name):
		switch {
		case compared:
		case name == "test" || name == "doctest" || name == "doc":
			return Const(false)
		case name == "MODULE":
			// The kernel passes --cfg MODULE to a module, as -DMODULE in C.
			return Var("defined(MODULE)", Enabled)
		}
		return cfgAtom(toks)
	case !compared:
		return Var(name, Enabled)
	case value == "y":
		return Var(name, Builtin)
	case value == "m":
		return Var(name, Module)
	case value == "n":
		return Const(false)
	}
	return cfgAtom(toks)
}

// cfgAtom is the opaque atom of a predicate the analysis cannot read,
// cfg(...) with its tokens and without quotes.
func cfgAtom(toks []rtoken) *Expr {
	var b strings.Builder
	b.WriteString("cfg(")
	for i, t := range toks {
		if i > 0 && t.ident && toks[i-1].ident {
			b.WriteByte(' ')
		}
		b.WriteString(strings.Trim(t.text, "\""))
	}
	b.WriteString(")")
	return Var(b.String(), Enabled)
}
