package kcond

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"
)

// Equal expressions are one node, however they were built.
func TestInternShares(t *testing.T) {
	a, b, c := Var("CONFIG_A", Enabled), Var("CONFIG_B", Builtin), Var(`"defined(C)"`, Enabled)
	if Var("CONFIG_A", Enabled) != a || Var(`"defined(C)"`, Module) != c {
		t.Error("Var made a second node")
	}
	if And(a, Or(b, Not(c))) != And(Or(Not(c), b), a) {
		t.Error("And of the same arguments in another order made a second node")
	}
	if e, err := Parse(`CONFIG_B=y || !"\"defined(C)\""`); err != nil || e != Or(Not(c), b) {
		t.Errorf("Parse = %v, %v; want the node Or made", e, err)
	}
}

// Goroutines building the same expressions get the same nodes.
func TestInternConcurrent(t *testing.T) {
	build := func() []*Expr {
		r := rand.New(rand.NewPCG(3, 4))
		es := make([]*Expr, 500)
		for i := range es {
			es[i] = randomExpr(r, 4)
		}
		return es
	}
	got := make([][]*Expr, 8)
	var wg sync.WaitGroup
	for i := range got {
		wg.Go(func() { got[i] = build() })
	}
	wg.Wait()
	for i := range got[0] {
		for g := range got {
			if got[g][i] != got[0][i] {
				t.Fatalf("goroutine %d built %s as another node", g, got[g][i])
			}
		}
	}
}

// The table forgets the nodes no expression holds any more.
func TestInternForgets(t *testing.T) {
	entries := func() int {
		runtime.GC()
		n := 0
		for i := range table {
			s := &table[i]
			s.mu.Lock()
			s.sweep()
			n += s.n
			s.mu.Unlock()
		}
		return n
	}
	before := entries()
	held := make([]*Expr, 5000)
	for i := range held {
		held[i] = Var(fmt.Sprintf("CONFIG_FORGET_%d", i), Builtin)
	}
	if n := entries(); n < before+len(held) {
		t.Fatalf("%d entries holding %d nodes, from %d", n, len(held), before)
	}
	runtime.KeepAlive(held)
	if n := entries(); n > before+100 {
		t.Errorf("%d entries after the nodes went, from %d", n, before)
	}
}
