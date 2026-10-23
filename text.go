package kcond

import "strings"

// compareText compares a and b as strings.Compare compares their String
// forms, without writing them; with trim, as it compares those forms with
// their leading ( and ! dropped, which orders !A next to A.
func compareText(a, b *Expr, trim bool) int {
	if a == b {
		return 0
	}
	var ca, cb textCursor
	ca.push(textFrame{e: a})
	cb.push(textFrame{e: b})
	if trim {
		ca.trim()
		cb.trim()
	}
	for {
		okA, okB := ca.fill(), cb.fill()
		switch {
		case !okA && !okB:
			return 0
		case !okA:
			return -1
		case !okB:
			return 1
		}
		n := min(len(ca.piece), len(cb.piece))
		if c := strings.Compare(ca.piece[:n], cb.piece[:n]); c != 0 {
			return c
		}
		ca.piece, cb.piece = ca.piece[n:], cb.piece[n:]
	}
}

// textCursor reads the String form of an expression a piece at a time:
// a leaf, !, a parenthesis or an operator. Its first frames are its own,
// so that reading most expressions allocates nothing.
type textCursor struct {
	frames [16]textFrame
	more   []textFrame // the frames past the first 16
	depth  int
	piece  string // the rest of the piece being read
}

// textFrame is a node being read: the arguments read so far of an AND or
// OR, and whether it is read as an operand, in parentheses.
type textFrame struct {
	e      *Expr
	next   int
	paren  bool
	opened bool // the ( of an operand, or the ! of a Not, is read
	sep    bool // the operator before argument next is read
}

func (c *textCursor) top() *textFrame {
	if c.depth <= len(c.frames) {
		return &c.frames[c.depth-1]
	}
	return &c.more[c.depth-1-len(c.frames)]
}

func (c *textCursor) push(f textFrame) {
	if c.depth++; c.depth <= len(c.frames) {
		c.frames[c.depth-1] = f
		return
	}
	if i := c.depth - 1 - len(c.frames); i < len(c.more) {
		c.more[i] = f
	} else {
		c.more = append(c.more, f)
	}
}

// fill makes piece the next text to read; false at the end.
func (c *textCursor) fill() bool {
	for c.piece == "" {
		if c.depth == 0 {
			return false
		}
		f := c.top()
		switch e := f.e; {
		case e.Op == OpConst || e.Op == OpVar:
			c.piece = e.leaf
			c.depth--
		case e.Op == OpNot && !f.opened:
			f.opened, c.piece = true, "!"
		case e.Op == OpNot:
			*f = textFrame{e: e.Args[0], paren: true}
		case f.paren && !f.opened:
			f.opened, c.piece = true, "("
		case f.next == len(e.Args):
			if c.depth--; f.paren {
				c.piece = ")"
			}
		case f.next > 0 && !f.sep:
			f.sep, c.piece = true, " && "
			if e.Op == OpOr {
				c.piece = " || "
			}
		default:
			a := e.Args[f.next]
			f.next, f.sep = f.next+1, false
			c.push(textFrame{e: a, paren: true})
		}
	}
	return true
}

// trim drops the leading ( and ! of the text.
func (c *textCursor) trim() {
	for c.fill() && (c.piece[0] == '(' || c.piece[0] == '!') {
		c.piece = c.piece[1:]
	}
}
