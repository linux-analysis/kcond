package kcond

import (
	"fmt"
	"strings"
	"testing"
)

// kernelC is the code of a C source compiled only into the kernel.
var kernelC = Code{Kernel: true}

func TestParseIf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"defined(CONFIG_A)", "CONFIG_A=y"},
		{"defined CONFIG_A", "CONFIG_A=y"},
		{"CONFIG_A", "CONFIG_A=y"},
		{"IS_ENABLED(CONFIG_A)", "CONFIG_A"},
		{"IS_BUILTIN(CONFIG_A)", "CONFIG_A=y"},
		{"IS_MODULE(CONFIG_A)", "CONFIG_A=m"},
		{"IS_REACHABLE(CONFIG_A)", "CONFIG_A"},
		{"config_enabled(CONFIG_A)", "CONFIG_A=y"},
		{"config_enabled(CONFIG_A_MODULE)", "CONFIG_A=m"},
		{"!config_enabled(CONFIG_A) && IS_ENABLED(CONFIG_B)", "!CONFIG_A=y && CONFIG_B"},
		{"defined(CONFIG_A_MODULE)", "CONFIG_A=m"},
		{"CONFIG_A_MODULE", "CONFIG_A=m"},
		{"!defined(CONFIG_A) || IS_MODULE(CONFIG_B)", "!CONFIG_A=y || CONFIG_B=m"},
		{"(defined(CONFIG_A) && defined(CONFIG_B))", "CONFIG_A=y && CONFIG_B=y"},
		{"0", "false"},
		{"1", "true"},
		{"0x10", "true"},
		{"1UL", "true"},
		// Kernel conventions.
		{"defined(__KERNEL__)", "true"},
		{"!defined(__ASSEMBLY__)", "true"},
		// Arch macros and the word size stand for their Kconfig symbols.
		{"defined(__i386__) && !defined(__arch_um__)", "!CONFIG_UML && CONFIG_X86_32"},
		{"defined(__x86_64__)", "CONFIG_X86_64"},
		{"BITS_PER_LONG == 32", "!CONFIG_64BIT"},
		{"BITS_PER_LONG > 32", "CONFIG_64BIT"},
		{"64 == BITS_PER_LONG", "CONFIG_64BIT"},
		{"(BITS_PER_LONG >= 32)", "true"},
		{"BITS_PER_LONG & 32", `"BITS_PER_LONG & 32"`},
		// Page size comparisons take one form, for a build to decide.
		{"PAGE_SIZE < 8192", `"PAGE_SIZE < 8192"`},
		{"PAGE_SIZE > 8192UL", `"PAGE_SIZE > 8192"`},
		{"(PAGE_SHIFT > 14)", `"PAGE_SIZE > 16384"`},
		{"PAGE_SHIFT == 12", `"PAGE_SIZE == 4096"`},
		{"SZ_64K <= PAGE_SIZE", `"PAGE_SIZE >= 65536"`},
		{"XEN_PAGE_SIZE == PAGE_SIZE", `"PAGE_SIZE == 4096"`},
		{"PAGE_SIZE == PAGE_SHIFT", `"PAGE_SIZE == PAGE_SHIFT"`},
		// Anything else is an opaque atom named by its normalized text.
		{"defined(MODULE)", `"defined(MODULE)"`},
		{"CONFIG_NR_CPUS > 1", `"CONFIG_NR_CPUS > 1"`},
		{"BITS_PER_LONG==64 && defined(CONFIG_A)", "CONFIG_64BIT && CONFIG_A=y"},
		{"LINUX_VERSION_CODE >= KERNEL_VERSION(5, 0, 0)", `"LINUX_VERSION_CODE >= KERNEL_VERSION ( 5 , 0 , 0 )"`},
		{"defined(CONFIG_A) ? 1 : 0", `"defined ( CONFIG_A ) ? 1 : 0"`},
		{"__GNUC__", "__GNUC__"},
	} {
		e, err := ParseIf(tc.in, kernelC)
		if err != nil {
			t.Errorf("ParseIf(%q): %v", tc.in, err)
			continue
		}
		if got := e.String(); got != tc.want {
			t.Errorf("ParseIf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Outside kernel code, an x86-64 configuration may compile a line as 16- or
// 32-bit code, so the arch macros keep an atom there; the other arches and
// i386 still decide.
func TestParseIfMixed(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"defined(__i386__)", `("defined(__i386__)" && CONFIG_X86_64) || CONFIG_X86_32`},
		{"defined(__x86_64__)", `!"defined(__i386__)" && CONFIG_X86_64`},
		{"!defined(__arch_um__)", "!CONFIG_UML"},
		{"BITS_PER_LONG == 32", `"BITS_PER_LONG == 32" || !CONFIG_64BIT`},
		{"BITS_PER_LONG > 32", `!"BITS_PER_LONG == 32" && CONFIG_64BIT`},
		{"BITS_PER_LONG >= 32", "true"},
		{"defined(CONFIG_A)", "CONFIG_A=y"},
	} {
		e, err := ParseIf(tc.in, Code{})
		if err != nil {
			t.Errorf("ParseIf(%q): %v", tc.in, err)
			continue
		}
		if got := e.String(); got != tc.want {
			t.Errorf("ParseIf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	got, err := Lines([]byte("#ifdef __x86_64__\nx\n#endif\n"), Code{})
	if err != nil || got[1].String() != `!"defined(__i386__)" && CONFIG_X86_64` {
		t.Errorf("Lines = %v, %v; want the mixed __x86_64__ on line 2", got, err)
	}
}

func TestParseIfErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"(CONFIG_A",
		"CONFIG_A)",
		"defined(",
		"defined(CONFIG_A",
		"CONFIG_A &&",
		"CONFIG_A CONFIG_B",
		"IS_ENABLED(",
		"@",
	} {
		if e, err := ParseIf(in, kernelC); err == nil {
			t.Errorf("ParseIf(%q) = %q, want error", in, e)
		}
	}
}

// checkLines runs Lines over src and compares every line's condition.
func checkLines(t *testing.T, src string, want []string) {
	t.Helper()
	got, err := Lines([]byte(src), kernelC)
	if err != nil {
		t.Fatalf("Lines: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Lines returned %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("line %d: %q, want %q", i+1, got[i], want[i])
		}
	}
}

// Each directive line gets the branch it opens, #endif the branch it
// closes, so a hunk that adds or wraps a block reads as the block.
func TestLinesElifChain(t *testing.T) {
	// The else branch must be !A && !B, not !(!A && B).
	checkLines(t, `#if defined(CONFIG_A)
a
#elif IS_ENABLED(CONFIG_B)
b
#elif IS_MODULE(CONFIG_C)
c
#else
d
#endif
e
`, []string{
		"CONFIG_A=y",
		"CONFIG_A=y",
		"!CONFIG_A=y && CONFIG_B",
		"!CONFIG_A=y && CONFIG_B",
		"!CONFIG_A=y && !CONFIG_B && CONFIG_C=m",
		"!CONFIG_A=y && !CONFIG_B && CONFIG_C=m",
		"!CONFIG_A=y && !CONFIG_B && !CONFIG_C=m",
		"!CONFIG_A=y && !CONFIG_B && !CONFIG_C=m",
		"!CONFIG_A=y && !CONFIG_B && !CONFIG_C=m",
		"true",
	})
}

func TestLinesNested(t *testing.T) {
	checkLines(t, `#ifdef CONFIG_A
#ifndef CONFIG_B_MODULE
x
#endif
y
#endif`, []string{
		"CONFIG_A=y",
		"CONFIG_A=y && !CONFIG_B=m",
		"CONFIG_A=y && !CONFIG_B=m",
		"CONFIG_A=y && !CONFIG_B=m",
		"CONFIG_A=y",
		"CONFIG_A=y",
	})
}

func TestLinesElifdef(t *testing.T) {
	checkLines(t, `#ifdef CONFIG_A
#elifdef CONFIG_B
x
#elifndef CONFIG_C
y
#endif
`, []string{
		"CONFIG_A=y",
		"!CONFIG_A=y && CONFIG_B=y",
		"!CONFIG_A=y && CONFIG_B=y",
		"!CONFIG_A=y && !CONFIG_B=y && !CONFIG_C=y",
		"!CONFIG_A=y && !CONFIG_B=y && !CONFIG_C=y",
		"!CONFIG_A=y && !CONFIG_B=y && !CONFIG_C=y",
	})
}

func TestLinesContinuation(t *testing.T) {
	checkLines(t, `#if defined(CONFIG_A) && \
    defined(CONFIG_B)
x
#endif
`, []string{
		"CONFIG_A=y && CONFIG_B=y",
		"CONFIG_A=y && CONFIG_B=y",
		"CONFIG_A=y && CONFIG_B=y",
		"CONFIG_A=y && CONFIG_B=y",
	})
}

func TestLinesComments(t *testing.T) {
	checkLines(t, `#ifdef CONFIG_A /* trailing */
a
#endif // CONFIG_A
/*
#ifdef CONFIG_B
*/
const char *s = "/* not a comment";
#  if IS_ENABLED(CONFIG_C) /* spans
   lines */
c
  # endif
`, []string{
		"CONFIG_A=y",
		"CONFIG_A=y",
		"CONFIG_A=y",
		"true",
		"true",
		"true",
		"true",
		"CONFIG_C",
		"CONFIG_C",
		"CONFIG_C",
		"CONFIG_C",
	})
}

func TestLinesConstants(t *testing.T) {
	checkLines(t, `#if 0
dead
#else
live
#endif
`, []string{"false", "false", "true", "true", "true"})
	checkLines(t, `#if 1
live
#else
dead
#endif
#if (0) /* never */
dead
#elif 0L
dead
#elif defined(CONFIG_A)
a
#endif
#if 0 && defined(CONFIG_B)
dead
#endif
`, []string{"true", "true", "false", "false", "false", "false", "false", "false", "false", "CONFIG_A=y",
		"CONFIG_A=y", "CONFIG_A=y", "false", "false", "false"})
}

func TestLinesIncludeGuard(t *testing.T) {
	checkLines(t, `/* SPDX */
#ifndef _LINUX_FOO_H
#define _LINUX_FOO_H
#ifdef CONFIG_FOO
int foo(void);
#endif
#endif /* _LINUX_FOO_H */
`, []string{"true", "true", "true", "CONFIG_FOO=y", "CONFIG_FOO=y", "CONFIG_FOO=y", "true"})
}

// A macro the file itself defines or undefines before a test is known
// there: atyfb's #undef DEBUG makes its #ifdef DEBUG blocks dead, a
// #define under a condition holds under it and only under it before any
// #include, and an #include may define the macro again. CONFIG_ symbols
// are left to Kconfig.
func TestLinesFileMacros(t *testing.T) {
	checkLines(t, `#undef DEBUG
/*#define DEBUG*/
#ifdef DEBUG
a
#endif
#ifdef CONFIG_A
#define D
#endif
#if defined(D) && !defined(E)
b
#endif
#undef F
#include "f.h"
#ifdef F
c
#endif
#undef CONFIG_64BIT
#ifdef CONFIG_64BIT
d
#endif
`, []string{
		"true",
		"true",
		"false",
		"false",
		"false",
		"CONFIG_A=y",
		"CONFIG_A=y",
		"CONFIG_A=y",
		`!"defined(E)" && CONFIG_A=y`,
		`!"defined(E)" && CONFIG_A=y`,
		`!"defined(E)" && CONFIG_A=y`,
		"true",
		"true",
		`"defined(F)"`,
		`"defined(F)"`,
		`"defined(F)"`,
		"true",
		"CONFIG_64BIT=y",
		"CONFIG_64BIT=y",
		"CONFIG_64BIT=y",
	})
}

// A file that tests a macro it defines further down, as qspinlock.c does
// before including itself for the paravirt slowpath, is compiled once
// without it: the test before any #include is undefined, the one after
// the includes may see a header's definition.
func TestLinesSelfInclude(t *testing.T) {
	checkLines(t, `#ifndef _GEN_PV
#include "lock.h"
int lock;
#endif
#if !defined(_GEN_PV) && defined(CONFIG_PV)
#define _GEN_PV
#include "lock.c"
#endif
`, []string{"true", "true", "true", "true", `!"defined(_GEN_PV)" && CONFIG_PV=y`, `!"defined(_GEN_PV)" && CONFIG_PV=y`, `!"defined(_GEN_PV)" && CONFIG_PV=y`, `!"defined(_GEN_PV)" && CONFIG_PV=y`})
}

// A header may be included after its includer defined the macro, as a
// trace header includes define_trace.h under CREATE_TRACE_POINTS, so a
// test before its first #include is still the atom; its include guard
// adds no condition, its #ifndef line included.
func TestLinesHeaderMacros(t *testing.T) {
	src := `#ifndef _FOO_H
#define _FOO_H
#ifdef CREATE_TRACE_POINTS
#undef CREATE_TRACE_POINTS
x
#define CREATE_TRACE_POINTS
#endif
#endif
`
	got, err := Lines([]byte(src), Code{Assembly: Unknown})
	if err != nil {
		t.Fatal(err)
	}
	ctp := `"defined(CREATE_TRACE_POINTS)"`
	for i, w := range []string{"true", "true", ctp, ctp, ctp, ctp, ctp, "true"} {
		if got[i].String() != w {
			t.Errorf("line %d: %s, want %s", i+1, got[i], w)
		}
	}
	// A guard after another test, and that of a trace header, which may
	// be read again, hold as well.
	src = `#ifndef __ASSEMBLY__
#error
#endif
#if !defined(_T_H) || defined(TRACE_HEADER_MULTI_READ)
#define _T_H
x
#endif
`
	if got, err = Lines([]byte(src), Code{Assembly: Unknown}); err != nil {
		t.Fatal(err)
	}
	if got[5].String() != "true" {
		t.Errorf("guarded line: %s, want true", got[5])
	}
}

// A file that redefines a macro under tests of itself, as arm's
// glue-proc.h does CPU_NAME once per CPU, would double its definition with
// each block; past maxDefinition it reads as its atom again.
func TestLinesRedefinedMacro(t *testing.T) {
	var src strings.Builder
	for i := range 16 {
		fmt.Fprintf(&src, "#ifdef CONFIG_CPU_%d\n# ifdef CPU_NAME\n#  undef MULTI_CPU\n#  define MULTI_CPU\n# else\n#  define CPU_NAME cpu_%d\n# endif\n#endif\n", i, i)
	}
	src.WriteString("#ifdef MULTI_CPU\nx\n#endif\n")
	conds, err := Lines([]byte(src.String()), kernelC)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range conds {
		if n := len(c.String()); n > 4*maxDefinition {
			t.Fatalf("line %d: condition of %d bytes", i+1, n)
		}
	}
}

func TestLinesMalformedCondition(t *testing.T) {
	// A condition ParseIf rejects becomes an opaque atom; the region
	// structure is still tracked.
	checkLines(t, `#if CONFIG_A &&
x
#endif
`, []string{`"CONFIG_A &&"`, `"CONFIG_A &&"`, `"CONFIG_A &&"`})
}

func TestLinesErrors(t *testing.T) {
	for _, src := range []string{
		"#endif\n",
		"#else\n",
		"#elif CONFIG_A\n",
		"#ifdef CONFIG_A\nx\n",
		"#ifdef CONFIG_A\n#else\n#else\n#endif\n",
		"#ifdef CONFIG_A\n#else\n#elif CONFIG_B\n#endif\n",
		"#ifdef\n#endif\n",
	} {
		if _, err := Lines([]byte(src), kernelC); err == nil {
			t.Errorf("Lines(%q) succeeded, want error", src)
		}
	}
}

func TestLinesNoTrailingNewline(t *testing.T) {
	got, err := Lines([]byte(strings.Repeat("x\n", 2)+"y"), kernelC)
	if err != nil || len(got) != 3 {
		t.Fatalf("Lines = %d lines, %v; want 3, nil", len(got), err)
	}
}

func TestPageSizeHolds(t *testing.T) {
	for _, tc := range []struct {
		atom      string
		size      uint64
		holds, ok bool
	}{
		{"PAGE_SIZE < 8192", 4096, true, true},
		{"PAGE_SIZE < 8192", 16384, false, true},
		{"PAGE_SIZE >= 65536", 65536, true, true},
		{"PAGE_SIZE == 4096", 4096, true, true},
		{"BITS_PER_LONG & 32", 4096, false, false},
	} {
		holds, ok := PageSizeHolds(tc.atom, tc.size)
		if holds != tc.holds || ok != tc.ok {
			t.Errorf("PageSizeHolds(%q, %d) = %v, %v; want %v, %v", tc.atom, tc.size, holds, ok, tc.holds, tc.ok)
		}
	}
}

// __ASSEMBLY__ is defined in assembly files and not in C ones; a header,
// which both include, has its lines compiled where either compiles them.
func TestLinesAssembly(t *testing.T) {
	src := `#ifndef __ASSEMBLY__
c
#else
asm
#endif
#ifdef __ASSEMBLER__
asm2
#endif
#if defined(CONFIG_A) && !defined(__ASSEMBLY__)
x
#endif
`
	for _, tc := range []struct {
		code Code
		want []string
	}{
		{kernelC, []string{"true", "true", "false", "false", "false", "false", "false", "false", "CONFIG_A=y", "CONFIG_A=y", "CONFIG_A=y"}},
		{Code{Kernel: true, Assembly: True}, []string{"false", "false", "true", "true", "true", "true", "true", "true", "false", "false", "false"}},
		{Code{Assembly: Unknown}, []string{"true", "true", "true", "true", "true", "true", "true", "true", "CONFIG_A=y", "CONFIG_A=y", "CONFIG_A=y"}},
	} {
		got, err := Lines([]byte(src), tc.code)
		if err != nil {
			t.Fatal(err)
		}
		for i, w := range tc.want {
			if got[i].String() != w {
				t.Errorf("%+v line %d: %s, want %s", tc.code, i+1, got[i], w)
			}
		}
	}
}
