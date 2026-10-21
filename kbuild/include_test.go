package kbuild

import (
	"io/fs"
	"slices"
	"testing"

	"github.com/linux-analysis/kcond"
)

// A .c or .S file a listed source #includes is built with it, under the
// condition of the directive's line, as well as under its own Makefile
// condition.
func TestIncludes(t *testing.T) {
	r := crawl(t, map[string]string{
		"Makefile":              "core-y += kernel/ lib/\ndrivers-y += drivers/ sound/\n",
		"kernel/Makefile":       "obj-y += sched/ bpf/\n",
		"kernel/sched/Makefile": "obj-y += build_policy.o\n",
		"kernel/sched/build_policy.c": `
#include "rt.c"
#ifdef CONFIG_SCHED_CLASS_EXT
# include "ext/ext.c"
#endif
`,
		"kernel/sched/rt.c": "",
		// Chains resolve; the name is looked up next to the including file.
		"kernel/sched/ext/ext.c":  "#include \"idle.c\"\n",
		"kernel/sched/ext/idle.c": "",
		// tools/ is never entered, yet the kernel compiles this file.
		"kernel/bpf/Makefile":       "obj-$(CONFIG_BPF_SYSCALL) += relo_core.o\n",
		"kernel/bpf/relo_core.c":    "#include \"../../tools/lib/bpf/relo_core.c\"\n",
		"tools/lib/bpf/relo_core.c": "",
		// A name not next to the file is looked up in the directories
		// above it, as i915's -I$(src) finds it.
		"drivers/Makefile":         "obj-$(CONFIG_DRM_I915) += i915/\n",
		"drivers/i915/Makefile":    "obj-y += gt/\n",
		"drivers/i915/gt/Makefile": "obj-y += intel_timeline.o\n",
		"drivers/i915/gt/intel_timeline.c": `
#if IS_ENABLED(CONFIG_DRM_I915_SELFTEST)
#include "gt/selftests/mock_timeline.c"
#endif
`,
		"drivers/i915/gt/selftests/mock_timeline.c": "",
		// A listed file another listed unit includes is built under
		// either condition.
		"sound/Makefile":        "obj-$(CONFIG_SND_MSND_PINNACLE) += msnd_pinnacle.o\nobj-$(CONFIG_SND_MSND_CLASSIC) += msnd_classic.o\n",
		"sound/msnd_classic.c":  "#include \"msnd_pinnacle.c\"\n",
		"sound/msnd_pinnacle.c": "",
		"lib/Makefile":          "obj-y += dead.o odd.o\n",
		// Neither a dropped region nor a comment includes a file.
		"lib/dead.c":      "#if 0\n#include \"never.c\"\n#endif\n/*\n#include \"commented.c\"\n*/\n",
		"lib/never.c":     "",
		"lib/commented.c": "",
		// Unbalanced directives leave the include unconditional.
		"lib/odd.c":      "#endif\n#include \"odd_part.c\"\n",
		"lib/odd_part.c": "",
		// Only unlisted files include it, so its condition stays unknown.
		"arch/x86/boot/main.c": "#include \"../../../lib/boot.c\"\n",
		"lib/boot.c":           "",
		// Generated files are not in the tree.
		"lib/table.c": "#include \"generated_table.c\"\n",
	})
	expect(t, r, map[string]string{
		"kernel/sched/rt.c":                         "true",
		"kernel/sched/ext/ext.c":                    "CONFIG_SCHED_CLASS_EXT=y",
		"kernel/sched/ext/idle.c":                   "CONFIG_SCHED_CLASS_EXT=y",
		"tools/lib/bpf/relo_core.c":                 "CONFIG_BPF_SYSCALL",
		"drivers/i915/gt/selftests/mock_timeline.c": "CONFIG_DRM_I915 && CONFIG_DRM_I915_SELFTEST",
		"sound/msnd_pinnacle.c":                     "CONFIG_SND_MSND_CLASSIC || CONFIG_SND_MSND_PINNACLE",
		"lib/never.c":                               "false",
		"lib/odd_part.c":                            "true",
		"lib/commented.c":                           "",
	})
	if _, st := r.Lookup("lib/boot.c"); st != Unlisted {
		t.Errorf("lib/boot.c: %v, want unlisted", st)
	}
	if got := r.Includes.Targets["lib/boot.c"]; len(got) != 1 || got[0].From != "arch/x86/boot/main.c" {
		t.Errorf("Includes[lib/boot.c] = %v, want arch/x86/boot/main.c", got)
	}
}

// Crawl takes the directives it is given instead of scanning the tree.
func TestGivenIncludes(t *testing.T) {
	inc := &Includes{Targets: map[string][]Include{"lib/part.c": {{From: "lib/a.c", Cond: kcond.Var("CONFIG_X", kcond.Enabled)}}}}
	r, err := Crawl(tree(map[string]string{
		"Makefile":     "core-y += lib/\n",
		"lib/Makefile": "obj-y += a.o b.o\n",
		"lib/a.c":      "", "lib/part.c": "",
		"lib/b.c": "#include \"other.c\"\n", "lib/other.c": "",
	}), Options{Arch: "x86", Includes: inc})
	if err != nil {
		t.Fatal(err)
	}
	expect(t, r, map[string]string{"lib/part.c": "CONFIG_X", "lib/other.c": ""})
}

// The modules a Rust crate declares are built with it: mod NAME; names
// NAME.rs or NAME/mod.rs beside a crate root, lib.rs or mod.rs, and under
// foo/ for foo.rs, or the file of its #[path] attribute. #[cfg] attributes
// add their condition. Only declarations at the start of a line count, as
// rustfmt puts module-level items there.
func TestRustModules(t *testing.T) {
	r := crawl(t, map[string]string{
		"Makefile":                "drivers-y += drivers/\ncore-y += rust/\n",
		"drivers/Makefile":        "obj-$(CONFIG_BINDER) += binder/\n",
		"drivers/binder/Makefile": "obj-y += binder.o\nbinder-y := main.o\n",
		"drivers/binder/main.rs": `
//! The crate root.
//!
//! # #[path = "../example.rs"] mod example;
mod node;
#[macro_use]
/// Docs.
pub(crate) mod range;
mod inline {
    mod nested;
}
`,
		"drivers/binder/node.rs":          "mod wrapper;\n#[path = \"freeze.rs\"]\nmod freeze;\n",
		"drivers/binder/node/wrapper.rs":  "",
		"drivers/binder/freeze.rs":        "",
		"drivers/binder/range/mod.rs":     "mod tree;\n",
		"drivers/binder/range/tree.rs":    "",
		"drivers/binder/inline/nested.rs": "",
		"drivers/binder/nested.rs":        "",
		"drivers/example.rs":              "",
		"rust/Makefile":                   "obj-$(CONFIG_RUST) += kernel.o\n$(obj)/kernel.o: $(src)/kernel/lib.rs FORCE\n\t$(call rustc)\n",
		"rust/kernel/lib.rs": `
#[cfg(CONFIG_X)]
pub mod x;
#[cfg(not(CONFIG_Y))] pub mod y;
#[cfg(all(CONFIG_Z = "y", any(CONFIG_A, CONFIG_B = "m"),))]
mod z;
#[cfg(test)]
mod t;
#[cfg(MODULE)]
mod m;
#[cfg(
    CONFIG_W
)]
mod w;
`,
		"rust/kernel/x.rs": "", "rust/kernel/y.rs": "", "rust/kernel/z/mod.rs": "",
		"rust/kernel/t.rs": "", "rust/kernel/m.rs": "", "rust/kernel/w.rs": "",
	})
	expect(t, r, map[string]string{
		"drivers/binder/node.rs":          "CONFIG_BINDER",
		"drivers/binder/node/wrapper.rs":  "CONFIG_BINDER",
		"drivers/binder/freeze.rs":        "CONFIG_BINDER",
		"drivers/binder/range/mod.rs":     "CONFIG_BINDER",
		"drivers/binder/range/tree.rs":    "CONFIG_BINDER",
		"drivers/binder/inline/nested.rs": "",
		"drivers/binder/nested.rs":        "",
		"drivers/example.rs":              "",
		"rust/kernel/x.rs":                "CONFIG_RUST && CONFIG_X",
		"rust/kernel/y.rs":                "CONFIG_RUST && !CONFIG_Y",
		"rust/kernel/z/mod.rs":            "(CONFIG_A || CONFIG_B=m) && CONFIG_RUST && CONFIG_Z=y",
		"rust/kernel/t.rs":                "false",
		"rust/kernel/m.rs":                `"defined(MODULE)" && CONFIG_RUST`,
		"rust/kernel/w.rs":                "CONFIG_RUST && CONFIG_W",
	})
}

// grepTree is a tree whose Grep finds only the files listed in found and,
// for Rust files, in rust.
type grepTree struct {
	fs.FS
	found, rust []string
}

func (g grepTree) Grep(strs []string, globs ...string) ([]string, error) {
	switch {
	case slices.Equal(strs, []string{`.c"`, `.S"`}) && slices.Equal(globs, []string{"*.c", "*.S"}):
		return g.found, nil
	case slices.Equal(strs, []string{"mod "}) && slices.Equal(globs, []string{"*.rs"}):
		return g.rust, nil
	}
	return nil, fs.ErrInvalid
}

// ScanIncludes reads only the files a GrepFS finds.
func TestScanIncludesGrep(t *testing.T) {
	fsys := grepTree{FS: tree(map[string]string{
		"a.c": "#include \"x.c\"\n", "b.S": "#include \"y.S\"\n", "c.c": "#include \"x.c\"\n",
		"x.c": "", "y.S": "", "lib.rs": "mod m;\n", "other.rs": "mod n;\n", "m.rs": "", "n.rs": "",
	}), found: []string{"a.c", "b.S"}, rust: []string{"lib.rs"}}
	inc, err := ScanIncludes(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if got := inc.Targets; len(got) != 3 || len(got["x.c"]) != 1 || got["x.c"][0].From != "a.c" || len(got["y.S"]) != 1 || len(got["m.rs"]) != 1 {
		t.Errorf("ScanIncludes = %v, want x.c from a.c, y.S from b.S and m.rs from lib.rs", inc)
	}
}

// An #include in code compiled as 16-bit code on x86-64 takes the mixed
// reading of the arch macros around it; one in kernel code the exact one.
func TestIncludeArchMacros(t *testing.T) {
	inc, err := ScanIncludes(tree(map[string]string{
		"arch/x86/realmode/rm/a.S": "#ifdef __i386__\n#include \"b.S\"\n#endif\n",
		"kernel/c.c":               "#ifdef __i386__\n#include \"d.c\"\n#endif\n",
		"arch/x86/realmode/rm/b.S": "", "kernel/d.c": "",
	}))
	if err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]string{
		"arch/x86/realmode/rm/b.S": `("defined(__i386__)" && CONFIG_X86_64) || CONFIG_X86_32`,
		"kernel/d.c":               "CONFIG_X86_32",
	} {
		if got := inc.Targets[f]; len(got) != 1 || got[0].Cond.String() != want {
			t.Errorf("Includes[%s] = %v, want %s", f, got, want)
		}
	}
}

// A name an #include finds neither next to the including file nor above it
// is looked up in the -I directories of the file's object: libstub
// compiles zboot-decompress-gzip.c with -I$(srctree)/lib/zlib_inflate.
// subdir-ccflags-y reaches the directories below.
func TestIncludeFlags(t *testing.T) {
	r := crawl(t, map[string]string{
		"Makefile":                             "drivers-y := drivers/\n",
		"drivers/Makefile":                     "obj-y += stub/ d/\n",
		"drivers/stub/Makefile":                "lib-$(CONFIG_ZBOOT) += zboot-decompress-gzip.o\nCFLAGS_zboot-decompress-gzip.o += -I$(srctree)/lib/zlib_inflate\n",
		"drivers/stub/zboot-decompress-gzip.c": "#include \"inflate.c\"\n",
		"lib/zlib_inflate/inflate.c":           "",
		"drivers/d/Makefile":                   "obj-y += a.o child/\nccflags-$(CONFIG_F) += -I $(src)/sub\nsubdir-ccflags-y += -I$(srctree)/inc\n",
		"drivers/d/a.c":                        "#include \"part.c\"\n",
		"drivers/d/sub/part.c":                 "",
		"drivers/d/child/Makefile":             "obj-y += b.o\n",
		"drivers/d/child/b.c":                  "#include \"shared.c\"\n#include \"generated.c\"\n",
		"inc/shared.c":                         "",
	})
	expect(t, r, map[string]string{
		"lib/zlib_inflate/inflate.c": "CONFIG_ZBOOT",
		"drivers/d/sub/part.c":       "CONFIG_F=y",
		"inc/shared.c":               "true",
	})
}

// A source compiled with -include of a .c or .S file compiles that file
// too, as the arm64 vDSO does with lib/vdso/gettimeofday.c.
func TestForcedIncludes(t *testing.T) {
	r := crawl(t, map[string]string{
		"Makefile":           "drivers-y := drivers/\n",
		"drivers/Makefile":   "obj-y += d/\n",
		"drivers/d/Makefile": "obj-$(CONFIG_V) += vgtod.o\nCFLAGS_vgtod.o += -O2 -include $(srctree)/lib/vdso/gettimeofday.c -include $(src)/env.h\n",
		"drivers/d/vgtod.c":  "", "drivers/d/env.h": "",
		"lib/vdso/gettimeofday.c": "",
	})
	expect(t, r, map[string]string{"lib/vdso/gettimeofday.c": "CONFIG_V"})
}
