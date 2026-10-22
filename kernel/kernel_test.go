package kernel

import (
	"testing"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/source"
)

func TestPathArch(t *testing.T) {
	for p, want := range map[string]string{
		"arch/x86/kernel/setup.c":    "x86",
		"arch/x86_64/kernel/setup.c": "x86",
		// x86-64 builds before v2.6.24 compiled some of arch/i386.
		"arch/i386/kernel/setup.c":    "x86",
		"arch/arm64/include/asm/io.h": "arm64",
		// User-mode Linux on an x86 host is not part of an x86 kernel.
		"arch/x86/um/asm/processor.h": "um",
		"include/linux/fs.h":          "",
		// lib/crypto, lib/crc and lib/raid keep each arch's assembly and
		// glue code in its own directory.
		"lib/crypto/arm64/sha3.h":        "arm64",
		"lib/crypto/powerpc/md5-asm.S":   "powerpc",
		"lib/crypto/tests/sha3_kunit.c":  "",
		"lib/crypto/sha3.c":              "",
		"lib/crc/loongarch/crc32.h":      "loongarch",
		"lib/crc/tests/crc_kunit.c":      "",
		"lib/raid/raid6/x86/pq_arch.h":   "x86",
		"lib/raid/xor/um/xor_arch.h":     "um",
		"lib/raid/xor/alpha/xor.c":       "alpha",
		"lib/raid/xor/tests/xor_kunit.c": "",
		"lib/raid/xor/xor-core.c":        "",
	} {
		if got := PathArch(p); got != want {
			t.Errorf("PathArch(%s) = %q, want %q", p, got, want)
		}
	}
}

func TestOutsideBuild(t *testing.T) {
	for p, want := range map[string]bool{
		"Documentation/process/howto.rst":      true,
		"LICENSES/preferred/GPL-2.0":           true,
		"tools/testing/selftests/net/Makefile": true,
		"tools/perf/builtin-top.c":             true,
		"MAINTAINERS":                          true,
		"CREDITS":                              true,
		".mailmap":                             true,
		".gitignore":                           true,
		"Documentation/Kconfig":                false, // the top Kconfig sources it
		"Kbuild":                               false,
		"lib/.gitignore":                       false,
		"tools/objtool/check.c":                false, // the build runs objtool
		"tools/include/linux/types.h":          false,
		"scripts/mod/modpost.c":                false,
		"Makefile":                             false,
		"drivers/net/bonding/bond_main.c":      false,
		"arch/arm64/boot/dts/qcom/sm8650.dtsi": false,
	} {
		if got := OutsideBuild(p); got != want {
			t.Errorf("OutsideBuild(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestKernelOnly(t *testing.T) {
	for p, want := range map[string]bool{
		"kernel/signal.c":                             true,
		"arch/x86/kernel/head_64.S":                   true,
		"arch/x86/purgatory/purgatory.c":              true,
		"include/linux/kernel.h":                      false,
		"arch/x86/include/asm/pvclock.h":              false,
		"arch/x86/boot/main.c":                        false,
		"arch/x86/boot/compressed/misc.c":             false,
		"arch/powerpc/boot/main.c":                    false,
		"arch/x86/realmode/rm/wakemain.c":             false,
		"arch/x86/kernel/acpi/realmode/wakemain.c":    false,
		"arch/x86/entry/vdso/vdso32/vclock_gettime.c": false,
		"arch/x86/vdso/vclock_gettime.c":              false,
		"arch/arm64/kernel/vdso32/vgettimeofday.c":    false,
		"lib/vdso/gettimeofday.c":                     false,
		// The real-mode trampoline includes it.
		"arch/x86/kernel/verify_cpu.S": false,
	} {
		if got := kernelOnly(p); got != want {
			t.Errorf("kernelOnly(%s) = %v, want %v", p, got, want)
		}
	}
}

// Assembly files define __ASSEMBLY__, C files do not, and headers are
// compiled both ways.
func TestCodeOf(t *testing.T) {
	for p, want := range map[string]source.Code{
		"kernel/fork.c":             {Kernel: true},
		"arch/x86/kernel/head_64.S": {Kernel: true, Assembly: kcond.True},
		"arch/x86/boot/header.S":    {Assembly: kcond.True},
		"include/linux/linkage.h":   {Assembly: kcond.Unknown},
	} {
		if got := CodeOf(p); got != want {
			t.Errorf("CodeOf(%s) = %+v, want %+v", p, got, want)
		}
	}
}

// $(eval) runs the Makefile lines it is given under the current
// condition: drivers/platform/x86/intel and lenovo turn their target lists
// into obj- lines with $(foreach ..., $(eval $(call ...))), each under the
// condition of its word, and generic_pt evaluates a $(foreach) of calls.
