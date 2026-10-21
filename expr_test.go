package kcond

import (
	"slices"
	"strings"
	"testing"
)

func mustParse(t *testing.T, s string) *Expr {
	t.Helper()
	e, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return e
}

func TestParseString(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"CONFIG_A", "CONFIG_A"},
		{"CONFIG_A && (CONFIG_B || !CONFIG_C)", "CONFIG_A && (CONFIG_B || !CONFIG_C)"},
		{"CONFIG_A=y || CONFIG_B=m", "CONFIG_A=y || CONFIG_B=m"},
		{"!(CONFIG_A && CONFIG_B)", "!(CONFIG_A && CONFIG_B)"},
		{"true", "true"},
		{"false", "false"},
		{"KMAX_ATOM_0 && CONFIG_A", "CONFIG_A && KMAX_ATOM_0"},
		// Normalization: flattening, sorting, deduplication.
		{"(CONFIG_B && CONFIG_A) && CONFIG_C", "CONFIG_A && CONFIG_B && CONFIG_C"},
		{"CONFIG_A && CONFIG_A", "CONFIG_A"},
		// Constant folding and double negation.
		{"true && CONFIG_A", "CONFIG_A"},
		{"false && CONFIG_A", "false"},
		{"true || CONFIG_A", "true"},
		{"false || false", "false"},
		{"!!CONFIG_A", "CONFIG_A"},
		{"!true", "false"},
		// Absorption and complements.
		{"CONFIG_A || (CONFIG_A && CONFIG_B)", "CONFIG_A"},
		{"CONFIG_A && (CONFIG_A || CONFIG_B)", "CONFIG_A"},
		{"(CONFIG_A && CONFIG_B) || (CONFIG_A && CONFIG_B && CONFIG_C)", "CONFIG_A && CONFIG_B"},
		{"CONFIG_A || !CONFIG_A", "true"},
		{"CONFIG_A || CONFIG_B || !(CONFIG_A || CONFIG_B)", "true"},
		{"CONFIG_A && CONFIG_B && !(CONFIG_A && CONFIG_B)", "false"},
		{"CONFIG_A || !(CONFIG_A || CONFIG_B)", "CONFIG_A || !(CONFIG_A || CONFIG_B)"},
		{"CONFIG_A && !CONFIG_A", "false"},
		// Tristate states of one symbol.
		{"CONFIG_A=y || CONFIG_A=m", "CONFIG_A"},
		{"CONFIG_A || CONFIG_A=y", "CONFIG_A"},
		{"CONFIG_A && CONFIG_A=m", "CONFIG_A=m"},
		{"CONFIG_A=y && CONFIG_A=m", "false"},
		{"CONFIG_A=y && !CONFIG_A", "false"},
		{"!CONFIG_A=y && !CONFIG_A=m", "!CONFIG_A"},
		{"CONFIG_A=y && !CONFIG_A=m", "CONFIG_A=y"},
		{"CONFIG_A=m && !CONFIG_A=y", "CONFIG_A=m"},
		{"CONFIG_A && !CONFIG_A=m", "CONFIG_A=y"},
		{"CONFIG_A && !CONFIG_A=y", "CONFIG_A=m"},
		{"CONFIG_A=m || (!CONFIG_A=m && CONFIG_A=y)", "CONFIG_A"},
		{"(CONFIG_B && CONFIG_A=y) || (CONFIG_B && CONFIG_A=m)", "CONFIG_A && CONFIG_B"},
		// An operand shared by every term of an OR is hoisted.
		{"(CONFIG_A && CONFIG_B) || (CONFIG_A && CONFIG_C)", "CONFIG_A && (CONFIG_B || CONFIG_C)"},
		{"(CONFIG_A && CONFIG_T) || (CONFIG_B && CONFIG_T) || (CONFIG_C && CONFIG_T && CONFIG_D)",
			"(CONFIG_A || CONFIG_B || (CONFIG_C && CONFIG_D)) && CONFIG_T"},
	} {
		if got := mustParse(t, tc.in).String(); got != tc.want {
			t.Errorf("Parse(%q).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseRoundTrip(t *testing.T) {
	for _, s := range []string{
		"CONFIG_A && (CONFIG_B=y || !CONFIG_C=m)",
		"!(CONFIG_A || CONFIG_B) && CONFIG_C",
		`"dev -> flags & F" && !"defined(MODULE)" && CONFIG_A`,
		`"say \"hi\"" || KMAX_ATOM_1`,
	} {
		e := mustParse(t, s)
		if again := mustParse(t, e.String()); again.String() != e.String() {
			t.Errorf("round trip of %q: %q then %q", s, e, again)
		}
	}
	// Atoms named like the constants, as from #if true, stay atoms.
	for _, name := range []string{"true", "false"} {
		e := Or(Var("CONFIG_A", Enabled), Var(name, Enabled))
		if again := mustParse(t, e.String()); again.String() != e.String() {
			t.Errorf("round trip of atom %s: %q then %q", name, e, again)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"CONFIG_A &&",
		"(CONFIG_A",
		"CONFIG_A)",
		"CONFIG_A CONFIG_B",
		"&& CONFIG_A",
		"CONFIG_A=x",
		"CONFIG_A=",
		"CONFIG_A | CONFIG_B",
		"CONFIG_A # comment",
		`"unterminated`,
	} {
		if e, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %q, want error", in, e)
		}
	}
}

func TestEval(t *testing.T) {
	cfg := Config{"CONFIG_Y": 'y', "CONFIG_M": 'm'}
	for _, tc := range []struct {
		in   string
		want Tri
	}{
		{"CONFIG_Y", True},
		{"CONFIG_M", True},
		{"CONFIG_N", False},
		{"CONFIG_Y=y", True},
		{"CONFIG_M=y", False},
		{"CONFIG_M=m", True},
		{"CONFIG_Y=m", False},
		{"CONFIG_Y && !CONFIG_N", True},
		{"CONFIG_N || CONFIG_M", True},
		// Atoms other than CONFIG_ symbols are unknown (Kleene logic).
		{"KMAX_ATOM_0", Unknown},
		{"!KMAX_ATOM_0", Unknown},
		{"KMAX_ATOM_0 && CONFIG_N", False},
		{"KMAX_ATOM_0 || CONFIG_Y", True},
		{"KMAX_ATOM_0 && CONFIG_Y", Unknown},
		{"true", True},
		{"false", False},
	} {
		if got := mustParse(t, tc.in).Eval(cfg); got != tc.want {
			t.Errorf("Eval(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestImplies(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"CONFIG_A && CONFIG_B", "CONFIG_A", true},
		{"CONFIG_A", "CONFIG_A && CONFIG_B", false},
		{"CONFIG_A=y", "CONFIG_A", true},
		{"CONFIG_A=m", "CONFIG_A", true},
		{"CONFIG_A", "CONFIG_A=y", false},
		{"CONFIG_A=y", "!CONFIG_A=m", true},
		{"false", "CONFIG_A", true},
		{"CONFIG_A", "true", true},
		{"KMAX_ATOM_0 && CONFIG_A", "CONFIG_A", true},
		{"CONFIG_A", "KMAX_ATOM_0", false},
	} {
		got, ok := Implies(mustParse(t, tc.a), mustParse(t, tc.b))
		if !ok || got != tc.want {
			t.Errorf("Implies(%q, %q) = %v, %v; want %v, true", tc.a, tc.b, got, ok, tc.want)
		}
	}
}

func TestEqual(t *testing.T) {
	a := mustParse(t, "CONFIG_A=y || CONFIG_A=m")
	b := mustParse(t, "CONFIG_A")
	if eq, ok := Equal(a, b); !ok || !eq {
		t.Errorf("Equal(%q, %q) = %v, %v; want true, true", a, b, eq, ok)
	}
	c := mustParse(t, "CONFIG_A && (CONFIG_B || CONFIG_C)")
	d := mustParse(t, "(CONFIG_A && CONFIG_B) || (CONFIG_A && CONFIG_C)")
	if eq, ok := Equal(c, d); !ok || !eq {
		t.Errorf("Equal(%q, %q) = %v, %v; want true, true", c, d, eq, ok)
	}
}

func TestImpliesTooManySymbols(t *testing.T) {
	var syms []string
	for i := range maxTableSymbols + 1 {
		syms = append(syms, "CONFIG_S"+strings.Repeat("X", i))
	}
	a := mustParse(t, strings.Join(syms, " || "))
	if _, ok := Implies(a, mustParse(t, "CONFIG_S")); ok {
		t.Errorf("Implies over %d symbols reported ok", len(syms))
	}
}

func TestSymbols(t *testing.T) {
	e := mustParse(t, "CONFIG_B && (CONFIG_A=y || !CONFIG_B=m) && KMAX_ATOM_1")
	if got, want := e.Symbols(), []string{"CONFIG_A", "CONFIG_B"}; !slices.Equal(got, want) {
		t.Errorf("Symbols() = %q, want %q", got, want)
	}
	if got, want := e.Opaque(), []string{"KMAX_ATOM_1"}; !slices.Equal(got, want) {
		t.Errorf("Opaque() = %q, want %q", got, want)
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig(strings.NewReader(`#
# Automatically generated file; DO NOT EDIT.
CONFIG_A=y
CONFIG_B=m
# CONFIG_C is not set
CONFIG_D="string"
CONFIG_E=0x10
`))
	if err != nil {
		t.Fatal(err)
	}
	for sym, want := range map[string]byte{"CONFIG_A": 'y', "CONFIG_B": 'm', "CONFIG_D": 'y', "CONFIG_E": 'y'} {
		if cfg[sym] != want {
			t.Errorf("cfg[%s] = %q, want %q", sym, cfg[sym], want)
		}
	}
	if _, ok := cfg["CONFIG_C"]; ok {
		t.Error("CONFIG_C is not set but present")
	}
}

func TestRelax(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"CONFIG_A && KMAX_ATOM_1", "CONFIG_A"},
		{"(!KMAX_ATOM_1 && CONFIG_B) || KMAX_ATOM_1", "true"},
		{"!KMAX_ATOM_1 && CONFIG_B", "CONFIG_B"},
		{"CONFIG_A || CONFIG_B", "CONFIG_A || CONFIG_B"},
		{"(KMAX_ATOM_1 && CONFIG_A) || (!KMAX_ATOM_1 && CONFIG_B)", "CONFIG_A || CONFIG_B"},
	} {
		if got := Relax(mustParse(t, tc.in)).String(); got != tc.want {
			t.Errorf("Relax(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
