package kconfig

import (
	"strings"
	"testing"
	"testing/fstest"
)

// tree builds an in-memory kernel tree.
func tree(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(strings.TrimLeft(content, "\n"))}
	}
	return fsys
}

func parse(t *testing.T, files map[string]string) *Tree {
	t.Helper()
	tr, err := Parse(tree(files), Env{"SRCARCH": "x86", "ARCH": "x86_64"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return tr
}

func sym(t *testing.T, tr *Tree, name string) *Symbol {
	t.Helper()
	s := tr.Syms[name]
	if s == nil {
		t.Fatalf("symbol %s not defined", name)
	}
	return s
}

func strs(es []*Expr) []string {
	out := []string{}
	for _, e := range es {
		out = append(out, e.String())
	}
	return out
}

func eq(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, " ; ") != strings.Join(want, " ; ") {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func TestParseEntries(t *testing.T) {
	tr := parse(t, map[string]string{
		"Kconfig": `
# top
mainmenu "Linux/$(ARCH) Kernel Configuration"

source "arch/$(SRCARCH)/Kconfig"
source "init/Kconfig"
osource "missing/Kconfig"
`,
		"arch/x86/Kconfig": `
config X86
	def_bool y
	select HAVE_FOO
	select HAVE_BAR if !X86_32

config 64BIT
	bool "64-bit kernel" if "$(ARCH)" = "x86"
	default "$(ARCH)" != "i386"
`,
		"init/Kconfig": `
cc-option = $(success,$(CC) -Werror $(1) -E -x c /dev/null -o /dev/null)

config MODULES
	bool "Enable loadable module support"
	modules

config HAVE_FOO
	bool

menuconfig NET
	bool "Networking support"
	help
	  config NOT_A_SYMBOL
	    depends on nothing

	  More help.
config INET
	tristate "TCP/IP" if EXPERT || \
		NET_EXPERT
	depends on NET
	default m if FOO
	default y
	imply IPV6

config CC_HAS_X
	def_bool $(cc-option,-mfoo) && NET

config DRV
	tristate "driver"
	depends on (ARCH_QCOM || COMPILE_TEST) && m
	depends on BAR if BAZ
	select FIRMWARE
	---help---
	  Old help.

config W1
	bool "w1"
	--- help ---
	  This allows to communicate.
	  1. Events.
		indented more.
`,
	})
	if len(tr.Warnings) != 0 {
		t.Errorf("warnings: %q", tr.Warnings)
	}
	eq(t, "files", tr.Files, []string{"Kconfig", "arch/x86/Kconfig", "init/Kconfig"})
	if tr.Modules != "MODULES" {
		t.Errorf("Modules = %q", tr.Modules)
	}
	if _, ok := tr.Syms["NOT_A_SYMBOL"]; ok {
		t.Error("help text parsed as an entry")
	}

	x86 := sym(t, tr, "X86")
	if x86.Type != Bool || len(x86.Prompts) != 0 {
		t.Errorf("X86 = %+v", x86)
	}
	eq(t, "X86 defaults", defaults(x86), []string{"y if y"})
	eq(t, "HAVE_FOO rev", strs(sym(t, tr, "HAVE_FOO").RevDep), []string{"X86"})
	eq(t, "HAVE_BAR rev", strs(sym(t, tr, "HAVE_BAR").RevDep), []string{"X86 && !X86_32"})

	b64 := sym(t, tr, "64BIT")
	eq(t, "64BIT prompts", strs(b64.Prompts), []string{`"x86_64" = "x86"`})
	eq(t, "64BIT defaults", defaults(b64), []string{`"x86_64" != "i386" if y`})

	inet := sym(t, tr, "INET")
	if inet.Type != Tristate {
		t.Errorf("INET type = %v", inet.Type)
	}
	eq(t, "INET prompts", strs(inet.Prompts), []string{"(EXPERT || NET_EXPERT) && NET"})
	eq(t, "INET defaults", defaults(inet), []string{"m if FOO && NET", "y if NET"})
	eq(t, "INET dirdep", strs(inet.DirDep), []string{"NET"})
	eq(t, "IPV6 implied", strs(tr.Syms["IPV6"].Implied), []string{"INET && NET"})

	eq(t, "CC_HAS_X defaults", defaults(sym(t, tr, "CC_HAS_X")), []string{"$(cc-option,-mfoo) && NET if y"})

	drv := sym(t, tr, "DRV")
	// 'm' in a dependency means m && MODULES; "depends on X if Y" is
	// X || Y = n.
	eq(t, "DRV dirdep", strs(drv.DirDep), []string{"(ARCH_QCOM || COMPILE_TEST) && m && MODULES && (BAR || BAZ = n)"})
	eq(t, "FIRMWARE rev", strs(tr.Syms["FIRMWARE"].RevDep),
		[]string{"DRV && (ARCH_QCOM || COMPILE_TEST) && m && MODULES && (BAR || BAZ = n)"})
}

func defaults(s *Symbol) []string {
	out := []string{}
	for _, d := range s.Defaults {
		out = append(out, d.Value.String()+" if "+d.Cond.String())
	}
	return out
}

func TestParseBlocks(t *testing.T) {
	tr := parse(t, map[string]string{
		"Kconfig": `
if NET

menu "Drivers"
	depends on PCI
	visible if EXPERT

config A
	bool "a"
	depends on X

choice
	prompt "Mode"
	depends on A
	default B1

config B1
	bool "b1"

config B2
	bool "b2"
	depends on Y

endchoice

endmenu

config C
	bool
	default y

endif

config C
	bool "c again"
	depends on Z
	default n
`,
	})
	a := sym(t, tr, "A")
	eq(t, "A prompts", strs(a.Prompts), []string{"NET && PCI && X && EXPERT"})
	eq(t, "A dirdep", strs(a.DirDep), []string{"NET && PCI && X"})

	b1, b2 := sym(t, tr, "B1"), sym(t, tr, "B2")
	if b1.Choice == nil || b1.Choice != b2.Choice || len(b1.Choice.Members) != 2 || b1.Choice.Optional {
		t.Fatalf("choice = %+v", b1.Choice)
	}
	eq(t, "B2 prompts", strs(b2.Prompts), []string{"NET && PCI && A && Y && EXPERT"})
	if len(b2.DirDep) != 0 {
		t.Errorf("choice member has dirdep %q", strs(b2.DirDep))
	}

	// A symbol defined twice keeps each definition's conditions, with the
	// defaults in parse order.
	c := sym(t, tr, "C")
	eq(t, "C prompts", strs(c.Prompts), []string{"Z"})
	eq(t, "C defaults", defaults(c), []string{"y if NET", "n if Z"})
	eq(t, "C dirdep", strs(c.DirDep), []string{"NET", "Z"})
}

func TestParseOldSyntax(t *testing.T) {
	// v4.14: env symbols, $SRCARCH in source, boolean/def_boolean,
	// "depends" without "on", optional and tristate choices.
	tr := parse(t, map[string]string{
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

config X86_64
	def_boolean y
	depends 64BIT

config OLD
	boolean "old"
	requires NET

choice
	tristate "codec"
	optional

config C1
	tristate "c1"

endchoice

config MODULES
	bool "modules"
	option modules
`,
	})
	if s := sym(t, tr, "ARCH"); s.Env != "ARCH" || s.Type != String {
		t.Errorf("ARCH = %+v", s)
	}
	eq(t, "X86_64 dirdep", strs(sym(t, tr, "X86_64").DirDep), []string{"64BIT"})
	if s := sym(t, tr, "OLD"); s.Type != Bool {
		t.Errorf("OLD type = %v", s.Type)
	}
	eq(t, "OLD dirdep", strs(sym(t, tr, "OLD").DirDep), []string{"NET"})
	ch := sym(t, tr, "C1").Choice
	if ch == nil || !ch.Optional || !ch.Tristate {
		t.Errorf("choice = %+v", ch)
	}
	if tr.Modules != "MODULES" {
		t.Errorf("Modules = %q", tr.Modules)
	}
}

func TestParseRoot(t *testing.T) {
	// Trees before the top-level Kconfig start at the arch Kconfig;
	// rsource is relative to the including file and globs expand.
	tr := parse(t, map[string]string{
		"arch/x86/Kconfig":     "source \"drivers/*/Kconfig\"\nrsource \"Kconfig.cpu\"\n",
		"arch/x86/Kconfig.cpu": "config CPU\n\tbool\n",
		"drivers/a/Kconfig":    "config A\n\tbool \"a\"\n",
		"drivers/b/Kconfig":    "config B\n\ttristate \"b\"\n\ttransitional\n",
	})
	eq(t, "files", tr.Files, []string{"arch/x86/Kconfig", "drivers/a/Kconfig", "drivers/b/Kconfig", "arch/x86/Kconfig.cpu"})
	for _, n := range []string{"CPU", "A", "B"} {
		sym(t, tr, n)
	}
	if !tr.Syms["B"].Transitional {
		t.Error("B not transitional")
	}
}

func TestParseErrors(t *testing.T) {
	if _, err := Parse(tree(map[string]string{"README": "x"}), Env{"SRCARCH": "x86"}); err == nil {
		t.Error("Parse without a root Kconfig succeeded")
	}
	tr := parse(t, map[string]string{
		"Kconfig": `
source "missing/Kconfig"
frobnicate X
config A
	bool "a"
	depends on (B
	wibble
endmenu
`,
	})
	if len(tr.Warnings) != 5 {
		t.Errorf("warnings = %q, want 5", tr.Warnings)
	}
	// An unparsable dependency becomes opaque rather than dropping A.
	eq(t, "A dirdep", strs(sym(t, tr, "A").DirDep), []string{"$(?)"})
}
