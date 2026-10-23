package kconfig

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/linux-analysis/kcond"
)

func analyze(t *testing.T, files map[string]string, env Env) *Values {
	t.Helper()
	if env == nil {
		env = Env{"SRCARCH": "x86", "ARCH": "x86_64"}
	}
	tr, err := Parse(tree(files), env)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(tr.Warnings) != 0 {
		t.Fatalf("warnings: %q", tr.Warnings)
	}
	return Analyze(tr, Options{Off: []string{"COMPILE_TEST"}})
}

func expectSets(t *testing.T, v *Values, want map[string]string) {
	t.Helper()
	for name, w := range want {
		if got := v.Of(name).String(); got != w {
			t.Errorf("%s = %s, want %s", name, got, w)
		}
	}
}

const base = `
config MODULES
	bool "Enable loadable module support"
	modules

config EXPERT
	bool "Expert"

config COMPILE_TEST
	bool "Compile test"
`

func TestAnalyzeBasics(t *testing.T) {
	v := analyze(t, map[string]string{"Kconfig": base + `
config MMU
	def_bool y

config X86
	def_bool y
	select HAVE_FOO
	select HAVE_BAR if EXPERT

config HAVE_FOO
	bool

config HAVE_BAR
	bool

config IO_URING
	bool "io_uring" if EXPERT
	default y

config BPF_SYSCALL
	bool "bpf"
	default n

config DRM_MSM
	tristate "msm"
	depends on ARCH_QCOM || COMPILE_TEST

config FOO
	tristate "foo"

config MOD_ONLY
	tristate "module only"
	depends on m

config OPT_DEP
	tristate "optional dependency"
	depends on FOO || !FOO

config CC_HAS
	def_bool $(cc-option,-mfoo)

config LIMIT
	int "limit"
	default 4

config CMP
	def_bool LIMIT > 2

config D1
	bool
	default n if MMU
	default y

config D2
	bool
	default y if EXPERT
	default n

config CY1
	bool
	default CY2

config CY2
	bool
	default CY1
`}, nil)
	expectSets(t, v, map[string]string{
		"MMU": "y", "CONFIG_MMU": "y", "X86": "y", "HAVE_FOO": "y", "HAVE_BAR": "ny",
		"IO_URING": "ny", "BPF_SYSCALL": "ny", "EXPERT": "ny", "COMPILE_TEST": "n",
		"DRM_MSM": "n", "ARCH_QCOM": "n", "FOO": "nmy", "MOD_ONLY": "nm", "OPT_DEP": "nmy",
		"CC_HAS": "ny", "LIMIT": "n", "CMP": "ny", "D1": "n", "D2": "ny", "CY1": "n", "CY2": "n",
	})
}

func TestAnalyzeSelect(t *testing.T) {
	v := analyze(t, map[string]string{"Kconfig": base + `
config SEL
	tristate "sel"
	depends on ARCH_QCOM

config USER
	bool "user"
	select SEL

config A
	bool
	depends on ARCH_QCOM
	select B

config C
	bool "c"
	select A

config B
	bool

config T
	tristate "t"
	select B2

config B2
	bool

config IMP
	bool "imp"
	imply TGT

config TGT
	bool
	depends on ARCH_QCOM

config UNDEF_USER
	bool "u"
	select NEVER_DEFINED
`}, nil)
	expectSets(t, v, map[string]string{
		// select ignores the target's dependencies, but a symbol that is
		// on only through select with its own dependencies unmet does
		// not pass its selects on.
		"SEL": "ny", "A": "ny", "B": "n",
		// a bool selected by a module is y
		"B2": "ny",
		// imply respects the target's dependencies
		"TGT":           "n",
		"NEVER_DEFINED": "n",
	})
}

func TestAnalyzeModules(t *testing.T) {
	v := analyze(t, map[string]string{"Kconfig": `
config FOO
	tristate "foo"

config INV
	tristate
	default m
`}, nil)
	// Without a modules symbol every tristate is a bool.
	expectSets(t, v, map[string]string{"FOO": "ny", "INV": "y"})
}

func TestAnalyzeChoice(t *testing.T) {
	v := analyze(t, map[string]string{"Kconfig": base + `
choice
	prompt "two"

config C1
	bool "c1"

config C2
	bool "c2"

endchoice

choice
	prompt "one"

config C3
	bool "c3"

endchoice

choice
	prompt "one possible"

config C4
	bool "c4"

config C5
	bool "c5"
	depends on ARCH_QCOM

endchoice

choice
	prompt "optional"
	optional

config C6
	bool "c6"

endchoice

choice
	prompt "untyped, first member tristate (before v6.11)"

config C7
	tristate "c7"

endchoice

menu "hidden"
	visible if EXPERT

config V
	bool "v"
	default y

endmenu
`}, nil)
	expectSets(t, v, map[string]string{
		"C1": "ny", "C2": "ny", "C3": "y", "C4": "y", "C5": "n", "C6": "ny", "C7": "nmy", "V": "ny",
	})
}

func TestAnalyzeEnv(t *testing.T) {
	files := map[string]string{
		"Kconfig": `
config SRCARCH
	string
	option env="SRCARCH"

source "arch/$SRCARCH/Kconfig"
`,
		"arch/x86/Kconfig": `
config ARCH
	string
	option env="ARCH"

config 64BIT
	bool "64-bit kernel" if ARCH = "x86"
	default ARCH != "i386"

config X86_32
	def_bool y
	depends on !64BIT

config X86_64
	def_bool y
	depends on 64BIT
`,
	}
	v := analyze(t, files, nil)
	expectSets(t, v, map[string]string{"64BIT": "y", "X86_64": "y", "X86_32": "n"})
	v = analyze(t, files, Env{"SRCARCH": "x86", "ARCH": "x86"})
	expectSets(t, v, map[string]string{"64BIT": "ny", "X86_64": "ny", "X86_32": "ny"})

	// v4.18+ writes the same with macros.
	v = analyze(t, map[string]string{"Kconfig": `
config 64BIT
	bool "64-bit kernel" if "$(ARCH)" = "x86"
	default "$(ARCH)" != "i386"
`}, nil)
	expectSets(t, v, map[string]string{"64BIT": "y"})
}

func TestAnalyzeTransitional(t *testing.T) {
	v := analyze(t, map[string]string{"Kconfig": `
config NEW
	bool "new"
	default OLD

config OLD
	bool
	transitional
`}, nil)
	// A transitional symbol is never written to a .config.
	expectSets(t, v, map[string]string{"NEW": "ny", "OLD": "n"})
}

func TestPossible(t *testing.T) {
	v := analyze(t, map[string]string{"Kconfig": base + `
config MMU
	def_bool y

config NET
	bool "net"

config BONDING
	tristate "bonding"
	depends on NET

config IO_URING
	bool "io_uring" if EXPERT
	default y

config MOD_ONLY
	tristate "module only"
	depends on m

config FOO
	tristate "foo"
`}, nil)
	for _, tc := range []struct {
		cond             string
		always, possible bool
	}{
		{"CONFIG_MMU", true, true},
		{"CONFIG_MMU && CONFIG_IO_URING", false, true},
		{"!CONFIG_MMU", false, false},
		{"CONFIG_ARCH_QCOM", false, false},
		{"!CONFIG_ARCH_QCOM", true, true},
		{"CONFIG_MOD_ONLY=y", false, false},
		{"CONFIG_MOD_ONLY=m", false, true},
		{"CONFIG_FOO || !CONFIG_FOO", true, true},
		{"CONFIG_FOO=y && CONFIG_FOO=m", false, false},
		{`CONFIG_MMU && "$(shell,true)"`, false, true},
		{"true", true, true},
		{"false", false, false},
		// BONDING requires NET.
		{"CONFIG_BONDING && !CONFIG_NET", false, false},
		{"!CONFIG_BONDING || CONFIG_NET", true, true},
		{"CONFIG_BONDING=m && CONFIG_NET", false, true},
	} {
		e, err := kcond.Parse(tc.cond)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.cond, err)
		}
		th := Theory{Values: v}
		if got := th.Always(e) == kcond.True; got != tc.always {
			t.Errorf("Always(%s) = %v, want %v", tc.cond, got, tc.always)
		}
		if got := th.Possible(e) == kcond.True; got != tc.possible {
			t.Errorf("Possible(%s) = %v, want %v", tc.cond, got, tc.possible)
		}
	}

	// Many symbols: a guess settles an OR at once.
	var terms []*kcond.Expr
	for i := range 2000 {
		terms = append(terms, kcond.And(kcond.Var(fmt.Sprintf("CONFIG_A%d", i), kcond.Enabled), kcond.Var("CONFIG_MMU", kcond.Enabled)))
	}
	big := kcond.Or(append(terms, kcond.Var("CONFIG_IO_URING", kcond.Enabled))...)
	th := Theory{Values: v}
	if th.Possible(big) != kcond.True || th.Always(big) != kcond.False {
		t.Errorf("big: possible %v always %v", th.Possible(big), th.Always(big))
	}

	// A term the Kconfig settles settles the OR before any branch.
	terms = terms[:0]
	for i := range 2000 {
		terms = append(terms, kcond.And(kcond.Var(fmt.Sprintf("defined(A%d)", i), kcond.Enabled), kcond.Var(fmt.Sprintf("defined(B%d)", i), kcond.Enabled)))
	}
	if got := th.Always(kcond.Or(append(terms, kcond.Var("CONFIG_MMU", kcond.Enabled))...)); got != kcond.True {
		t.Errorf("Always(... || CONFIG_MMU) = %v, want true", got)
	}
}

// TestPossibleWant checks that the search tries first the value a test
// needs where it sits: Always asks whether !e is possible, and the terms
// of e are turned off there before their symbols are turned on.
func TestPossibleWant(t *testing.T) {
	kc := base + `
config MMU
	def_bool y

config ZZ
	tristate "zz"

config D
	tristate "d"

config U
	tristate "u"
	depends on D
`
	var terms []*kcond.Expr
	for i := range 20 {
		kc += fmt.Sprintf("\nconfig A%d\n\tbool \"a\"\n\tdepends on ZZ\n\nconfig B%d\n\tbool \"b\"\n", i, i)
		terms = append(terms, kcond.And(kcond.Var(fmt.Sprintf("CONFIG_A%d", i), kcond.Enabled), kcond.Var(fmt.Sprintf("CONFIG_B%d", i), kcond.Enabled)))
	}
	th := Theory{Values: analyze(t, map[string]string{"Kconfig": kc}, nil)}
	// Off with U and D m and ZZ n. The guess fails, U turning D y, and an
	// A turned on makes ZZ m or y, which shows only once ZZ is tried,
	// past all the other As.
	e := kcond.Or(append(terms, kcond.Var("CONFIG_D", kcond.Builtin), kcond.And(kcond.Var("CONFIG_ZZ", kcond.Module), kcond.Var("CONFIG_MMU", kcond.Enabled)), kcond.Var("CONFIG_ZZ", kcond.Builtin), kcond.Not(kcond.Var("CONFIG_U", kcond.Enabled)))...)
	if got := th.Always(e); got != kcond.False {
		t.Errorf("Always = %v, want false", got)
	}
}

// TestPossibleModel checks Possible against every configuration of its
// model on random conditions small enough for the search to finish:
// each symbol a value Of allows it, each enabled one with the symbols it
// requires enabled, and the opaque atom n or y.
func TestPossibleModel(t *testing.T) {
	v := analyze(t, map[string]string{"Kconfig": base + `
config MMU
	def_bool y

config NET
	bool "net"

config BONDING
	tristate "bonding"
	depends on NET

config IO_URING
	bool "io_uring" if EXPERT
	default y

config MOD_ONLY
	tristate "module only"
	depends on m

config FOO
	tristate "foo"
	select NET
`}, nil)
	names := []string{"CONFIG_MODULES", "CONFIG_EXPERT", "CONFIG_MMU", "CONFIG_NET", "CONFIG_BONDING",
		"CONFIG_IO_URING", "CONFIG_MOD_ONLY", "CONFIG_FOO", "CONFIG_ARCH_QCOM", `"defined(X)"`}
	th := Theory{Values: v}
	var models []map[string]Set
	var enumerate func(i int, cfg map[string]Set)
	enumerate = func(i int, cfg map[string]Set) {
		if i == len(names) {
			for name, x := range cfg {
				for _, r := range th.requires(name) {
					if x != N && cfg[r] == N {
						return
					}
				}
			}
			models = append(models, maps.Clone(cfg))
			return
		}
		for _, x := range []Set{N, M, Y} {
			if th.domain(names[i])&x != 0 {
				cfg[names[i]] = x
				enumerate(i+1, cfg)
			}
		}
	}
	enumerate(0, map[string]Set{})

	rng := rand.New(rand.NewPCG(1, 2))
	var gen func(depth int) *kcond.Expr
	gen = func(depth int) *kcond.Expr {
		if depth == 0 || rng.IntN(3) == 0 {
			return kcond.Var(names[rng.IntN(len(names))], []kcond.State{kcond.Enabled, kcond.Builtin, kcond.Module}[rng.IntN(3)])
		}
		switch rng.IntN(3) {
		case 0:
			return kcond.Not(gen(depth - 1))
		case 1:
			return kcond.And(gen(depth-1), gen(depth-1), gen(depth-1))
		}
		return kcond.Or(gen(depth-1), gen(depth-1), gen(depth-1))
	}
	for range 3000 {
		e := gen(4)
		want := kcond.False
		for _, cfg := range models {
			if kcond.Rewrite(e, func(v *kcond.Expr) *kcond.Expr {
				switch x := cfg[v.Name]; v.State {
				case kcond.Builtin:
					return kcond.Const(x == Y)
				case kcond.Module:
					return kcond.Const(x == M)
				default:
					return kcond.Const(x != N)
				}
			}).IsConst(true) {
				want = kcond.True
				break
			}
		}
		if got := th.Possible(e); got != want {
			t.Fatalf("Possible(%s) = %v, want %v", e, got, want)
		}
	}
}

func TestRequires(t *testing.T) {
	v := analyze(t, map[string]string{"Kconfig": base + `
config NET
	bool "Networking"

if NET
config INET
	bool "TCP/IP networking"
endif

config MPTCP
	bool "Multipath TCP"
	depends on INET
	select CRYPTO

config CRYPTO
	tristate

config PCI
	bool "PCI"

config ETH
	tristate "Ethernet"
	depends on NET && (PCI || EXPERT) && !EXPERT && MODULES = y

config TWO
	bool "two definitions"
	depends on NET && PCI

config TWO
	bool
	depends on NET

choice
	prompt "Member"
	depends on NET

config MEMBER
	bool "member"

endchoice

config FLOPPY
	tristate "Floppy"
	depends on NET && HAVE_FDC && m

config LOOP_A
	bool "a"
	depends on LOOP_B

config LOOP_B
	bool "b"
	depends on LOOP_A

config SEL
	bool "selects"
	depends on NET
	select INET
	select PCI if MODULES
	select CRYPTO if NET

config TWICE
	bool "twice"

config TWICE
	bool
	depends on NET
	select INET
`}, nil)
	for name, want := range map[string]string{
		"MPTCP":        "CONFIG_INET CONFIG_NET",
		"CONFIG_MPTCP": "CONFIG_INET CONFIG_NET",
		"INET":         "CONFIG_NET",
		// Only plain symbols ANDed count.
		"ETH": "CONFIG_NET",
		// Every definition must need the symbol.
		"TWO": "CONFIG_NET",
		// An undefined symbol counts, the constant m does not.
		"FLOPPY":    "CONFIG_HAVE_FDC CONFIG_NET",
		"MEMBER":    "",
		"NET":       "",
		"UNDEFINED": "",
		"LOOP_A":    "CONFIG_LOOP_B",
	} {
		if got := strings.Join(v.Requires(name), " "); got != want {
			t.Errorf("Requires(%s) = %q, want %q", name, got, want)
		}
	}
	// A result is the caller's to change.
	if r := v.Requires("MPTCP"); len(r) > 0 {
		r[0] = "CONFIG_CHANGED"
	}
	if got := strings.Join(v.Requires("MPTCP"), " "); got != "CONFIG_INET CONFIG_NET" {
		t.Errorf("Requires(MPTCP) after a change of an earlier result = %q", got)
	}
	// DependsOn stops at the direct dependencies.
	for name, want := range map[string]string{"MPTCP": "CONFIG_INET", "INET": "CONFIG_NET", "NET": ""} {
		if got := strings.Join(v.DependsOn(name), " "); got != want {
			t.Errorf("DependsOn(%s) = %q, want %q", name, got, want)
		}
	}
	// A select under the symbol's own dependencies counts, one under
	// another condition does not, nor one under the dependencies of one
	// of its definitions.
	for name, want := range map[string]string{"MPTCP": "CONFIG_CRYPTO", "CONFIG_MPTCP": "CONFIG_CRYPTO", "NET": "", "SEL": "CONFIG_CRYPTO CONFIG_INET", "TWICE": ""} {
		if got := strings.Join(v.Selects(name), " "); got != want {
			t.Errorf("Selects(%s) = %q, want %q", name, got, want)
		}
	}
}

// Modular tells whether a symbol can be m, and whether the tree says.
func TestModular(t *testing.T) {
	v := analyze(t, map[string]string{"Kconfig": base + `
config BOOL
	bool "bool"

config TRI
	tristate "tri"

config TRI_ON_BOOL
	tristate "tristate that a bool caps"
	depends on BOOL

config NUM
	int "number"
`}, nil)
	for name, want := range map[string][2]bool{
		"BOOL": {false, true}, "CONFIG_TRI": {true, true}, "TRI_ON_BOOL": {true, true},
		"NUM": {false, false}, "UNDEFINED": {false, false},
	} {
		if m, ok := v.Modular(name); m != want[0] || ok != want[1] {
			t.Errorf("Modular(%s) = %v, %v; want %v, %v", name, m, ok, want[0], want[1])
		}
	}
}

// Numbers compare as numbers, decimal or hex, and anything else as text.
func TestCompareValues(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"10", "9", 1},
		{"0x10", "15", 1},
		{"-1", "0", -1},
		{"+5", "5", 0},
		{"y", "n", 1},
		{"10", "y", -1},
		{"", "0", -1},
	} {
		if got := compareValues(tc.a, tc.b); got != tc.want {
			t.Errorf("compareValues(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
