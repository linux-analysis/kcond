package kernel

import (
	"path"
	"slices"
	"strings"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/source"
)

// File is a C, assembly or Rust source of the kernel proper, or one of its
// headers.
type File struct {
	Path   string
	Header bool
}

// FileOf returns the kernel file at p; ok is false when p is not a C,
// assembly or Rust file of the kernel proper (not documentation, tools or
// scripts).
func FileOf(p string) (f File, ok bool) {
	for _, pre := range []string{"Documentation/", "tools/", "scripts/", "LICENSES/"} {
		if strings.HasPrefix(p, pre) {
			return File{}, false
		}
	}
	switch path.Ext(p) {
	case ".c", ".S", ".s", ".rs":
		return File{Path: p}, true
	case ".h":
		return File{Path: p, Header: true}, true
	}
	return File{}, false
}

// OutsideBuild reports whether building the kernel never reads p:
// documentation, licenses, selftests, perf, and the top-level files that
// describe the tree (MAINTAINERS, CREDITS, COPYING, README and the
// dotfiles). Other tools are left in, as the build runs some (objtool,
// resolve_btfids, bootconfig), and so is any Kconfig file, as the top one
// sources Documentation/Kconfig.
func OutsideBuild(p string) bool {
	if strings.HasPrefix(path.Base(p), "Kconfig") {
		return false
	}
	if !strings.Contains(p, "/") {
		switch p {
		case "MAINTAINERS", "CREDITS", "COPYING", "README":
			return true
		}
		return strings.HasPrefix(p, ".")
	}
	for _, pre := range []string{"Documentation/", "LICENSES/", "tools/testing/", "tools/perf/"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// kernelOnly reports whether the source p is compiled only into the kernel,
// at the configuration's word size, so that its arch macros follow Kconfig
// (see source.ParseIf). Headers are not, and neither is code a 64-bit
// configuration also compiles as 16- or 32-bit code: boot code, x86
// real-mode code, vDSOs (lib/vdso is part of them) and verify_cpu.S, which
// the real-mode trampoline includes.
func kernelOnly(p string) bool {
	if path.Ext(p) == ".h" || p == "arch/x86/kernel/verify_cpu.S" {
		return false
	}
	dirs := strings.Split(path.Dir(p), "/")
	if len(dirs) > 2 && dirs[0] == "arch" && dirs[2] == "boot" {
		return false
	}
	return !slices.ContainsFunc(dirs, func(d string) bool { return d == "realmode" || strings.HasPrefix(d, "vdso") })
}

// CodeOf tells how the file p is compiled (see kernelOnly): __ASSEMBLY__ is
// defined in .S files and not in C ones, and either way in headers, which
// both include.
func CodeOf(p string) source.Code {
	c := source.Code{Kernel: kernelOnly(p)}
	switch path.Ext(p) {
	case ".S":
		c.Assembly = kcond.True
	case ".h":
		c.Assembly = kcond.Unknown
	case ".rs":
		c.Rust = true
	}
	return c
}
