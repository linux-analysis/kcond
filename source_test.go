package kcond

import (
	"strings"
	"testing"
)

func mustAnalyze(t *testing.T, src string) *Source {
	t.Helper()
	s, err := Analyze([]byte(src), kernelC)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return s
}

// checkGuards compares the C guard condition of every line.
func checkGuards(t *testing.T, src string, want []string) {
	t.Helper()
	s := mustAnalyze(t, src)
	if s.GuardErr != nil {
		t.Fatalf("GuardErr: %v", s.GuardErr)
	}
	if len(s.Guard) != len(want) {
		t.Fatalf("got %d lines, want %d", len(s.Guard), len(want))
	}
	for i := range want {
		if got := s.Guard[i].String(); got != want[i] {
			t.Errorf("line %d: guard %q, want %q", i+1, got, want[i])
		}
	}
}

func TestGuardsIfElseChain(t *testing.T) {
	checkGuards(t, `int f(void)
{
	if (IS_ENABLED(CONFIG_A)) {
		a();
	} else if (IS_BUILTIN(CONFIG_B)) {
		b();
	} else {
		c();
	}
	d();
}
`, []string{
		"true",
		"true",
		"true",
		"CONFIG_A",
		"true",
		"!CONFIG_A && CONFIG_B=y",
		"!CONFIG_A",
		"!CONFIG_A && !CONFIG_B=y",
		"!CONFIG_A && !CONFIG_B=y",
		"true",
		"true",
	})
}

func TestGuardsSingleStatements(t *testing.T) {
	checkGuards(t, `void f(void)
{
	if (!IS_ENABLED(CONFIG_A))
		return;
	if (IS_MODULE(CONFIG_B))
		x();
	else
		y();
	if (IS_REACHABLE(CONFIG_C)) z(); else w();
	if (config_enabled(CONFIG_D))
		v();
	if (config_enabled(CONFIG_E_MODULE))
		u();
}
`, []string{
		"true",
		"true",
		"true",
		"!CONFIG_A",
		"true",
		"CONFIG_B=m",
		"true",
		"!CONFIG_B=m",
		"true",
		"true",
		"CONFIG_D=y",
		"true",
		"CONFIG_E=m",
		"true",
	})
}

func TestGuardsNestedAndMixed(t *testing.T) {
	checkGuards(t, `void f(struct dev *dev)
{
	if (IS_ENABLED(CONFIG_A)) {
		if (IS_ENABLED(CONFIG_B) && dev->flags & F)
			x();
	}
	if (dev->ready) {
		y();
	}
	if ((IS_ENABLED(CONFIG_C) || IS_ENABLED(CONFIG_D)) && !IS_ENABLED(CONFIG_E))
		z();
}
`, []string{
		"true",
		"true",
		"true",
		"CONFIG_A",
		`"dev -> flags & F" && CONFIG_A && CONFIG_B`,
		"CONFIG_A",
		"true",
		"true",
		"true",
		"true",
		"(CONFIG_C || CONFIG_D) && !CONFIG_E",
		"true",
	})
}

func TestGuardsElseAfterBlockStatement(t *testing.T) {
	// The else after an iterator macro's block or a do-while belongs to
	// the if, not to the statement before it.
	checkGuards(t, `void f(void)
{
	if (IS_ENABLED(CONFIG_A))
		list_for_each_entry(p, h, l) {
			a();
		}
	else
		b();
	if (IS_ENABLED(CONFIG_B))
		do {
			c();
		} while (0);
	else
		d();
}
`, []string{
		"true",
		"true",
		"true",
		"CONFIG_A",
		"CONFIG_A",
		"CONFIG_A",
		"true",
		"!CONFIG_A",
		"true",
		"CONFIG_B",
		"CONFIG_B",
		"CONFIG_B",
		"true",
		"!CONFIG_B",
		"true",
	})
}

func TestGuardsStatementsAroundIf(t *testing.T) {
	// Guarded ifs below case labels and loops, and inside a statement
	// expression.
	checkGuards(t, `int f(int op)
{
	switch (op) {
	case 1:
		if (IS_ENABLED(CONFIG_A))
			a();
		break;
	default:
		for (;;)
			if (IS_ENABLED(CONFIG_B))
				b();
	}
	do {
		if (IS_ENABLED(CONFIG_C)) c();
	} while (0);
	return ({ if (IS_ENABLED(CONFIG_D)) d(); 0; });
}
`, []string{
		"true",
		"true",
		"true",
		"true",
		"true",
		"CONFIG_A",
		"true",
		"true",
		"true",
		"true",
		"CONFIG_B",
		"true",
		"true",
		"true",
		"true",
		"true",
		"true",
	})
}

func TestGuardsIgnoreStringsCommentsAndDirectives(t *testing.T) {
	checkGuards(t, `void f(void)
{
	pr_info("{ not a brace '");
	/* } neither */ // nor this {
	if (IS_ENABLED(CONFIG_A)) {
#define M(x) if (x) { /* macro body, not code here */
		a('}');
	}
}
`, []string{
		"true",
		"true",
		"true",
		"true",
		"true",
		"true",
		"CONFIG_A",
		"CONFIG_A",
		"true",
	})
}

func TestGuardsSkipDeadBlocks(t *testing.T) {
	// Text inside #if 0 is not C; an apostrophe or brace there must not
	// derail the parser.
	s := mustAnalyze(t, `void f(void)
{
#if 0
	don't { parse this
#endif
	if (IS_ENABLED(CONFIG_A))
		a();
}
`)
	if s.GuardErr != nil {
		t.Fatalf("GuardErr: %v", s.GuardErr)
	}
	if got := s.Guard[6].String(); got != "CONFIG_A" {
		t.Errorf("line 7: guard %q, want CONFIG_A", got)
	}
	if got := s.Cpp[3].String(); got != "false" {
		t.Errorf("line 4: cpp %q, want false", got)
	}
}

func TestGuardsFallback(t *testing.T) {
	// A brace opened in both branches of an #ifdef cannot be followed
	// without choosing a branch; guards fall back to true.
	s := mustAnalyze(t, `#ifdef CONFIG_A
void f(int a) {
#else
void f(void) {
#endif
	if (IS_ENABLED(CONFIG_B))
		x();
}
`)
	if s.GuardErr == nil {
		t.Fatal("GuardErr is nil, want an error")
	}
	for i, g := range s.Guard {
		if g.String() != "true" {
			t.Errorf("line %d: guard %q after a fallback, want true", i+1, g)
		}
	}
}

func TestCode(t *testing.T) {
	src := `/*
 * SPDX comment block
 */

#include <linux/kernel.h>
// line comment
int x; /* trailing */
	/* only a comment */

#if 0
dead code
#endif
`
	want := []bool{false, false, false, false, true, false, true, false, false, true, true, true}
	s := mustAnalyze(t, src)
	if len(s.Code) != len(want) {
		t.Fatalf("got %d lines, want %d", len(s.Code), len(want))
	}
	for i := range want {
		if s.Code[i] != want[i] {
			t.Errorf("line %d (%q): code %v, want %v", i+1, strings.Split(src, "\n")[i], s.Code[i], want[i])
		}
	}
}

func TestSourceCond(t *testing.T) {
	s := mustAnalyze(t, `#ifdef CONFIG_A
void f(void)
{
	if (IS_ENABLED(CONFIG_B))
		x();
}
#endif
`)
	if got := s.Cond(4).String(); got != "CONFIG_A=y && CONFIG_B" {
		t.Errorf("Cond(4) = %q, want CONFIG_A=y && CONFIG_B", got)
	}
}
