package kcond

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// randomExpr builds an expression of depth up to depth from symbols in
// every state, opaque atoms that print quoted, negations and nested ANDs
// and ORs.
func randomExpr(r *rand.Rand, depth int) *Expr {
	atoms := []string{"CONFIG_A", "CONFIG_AB", "CONFIG_B", "CONFIG_B_C", "defined(X)", "a b", "true", "x\"y", "Z"}
	if depth == 0 || r.IntN(3) == 0 {
		return Var(atoms[r.IntN(len(atoms))], State(r.IntN(3)))
	}
	switch r.IntN(3) {
	case 0:
		return Not(randomExpr(r, depth-1))
	case 1:
		return And(randomExpr(r, depth-1), randomExpr(r, depth-1), randomExpr(r, depth-1))
	}
	return Or(randomExpr(r, depth-1), randomExpr(r, depth-1))
}

func sign(c int) int { return min(max(c, -1), 1) }

// compareText orders expressions as their String forms do, whole and with
// the leading ( and ! dropped.
func TestCompareText(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	es := make([]*Expr, 400)
	for i := range es {
		es[i] = randomExpr(r, 4)
	}
	for _, a := range es {
		for _, b := range es[:100] {
			for _, trim := range []bool{false, true} {
				sa, sb := a.String(), b.String()
				if trim {
					sa, sb = strings.TrimLeft(sa, "(!"), strings.TrimLeft(sb, "(!")
				}
				if got, want := sign(compareText(a, b, trim)), sign(strings.Compare(sa, sb)); got != want {
					t.Fatalf("compareText(%s, %s, %v) = %d, want %d", a, b, trim, got, want)
				}
			}
		}
	}
}
