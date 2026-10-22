// Package kernel holds what kcond knows of the layout of a kernel tree:
// the architectures and builds conditions are computed for, which files
// are kernel sources and headers, how they are compiled and which
// architecture they belong to, and the object lists the arch Makefiles
// link with rules of their own. The other packages read the tree through
// these tables, so that a rule about the tree is written once.
package kernel

import (
	"io/fs"
	"slices"
	"strings"
)

// LabelArches are the architectures labels name, in display order; x86
// stands for both x86-64 and i386.
var LabelArches = []string{"x86", "arm64", "arm", "powerpc", "s390", "riscv", "loongarch"}

// Primary is the architecture the main condition is computed for.
const Primary = "x86"

// X86_64Build is the build whose condition the main condition holds.
const X86_64Build = "x86_64"

// ConfigBuilds are the architecture builds conditions are computed for:
// x86-64, i386 and the other LabelArches.
var ConfigBuilds = slices.Concat([]string{X86_64Build, "i386"}, LabelArches[1:])

// ArchLabels are the labels builds.Summarize gives other than an architecture
// name: generic code builds on every label arch of its release but s390,
// several on more than one, other only on architectures outside
// LabelArches, none on none.
var ArchLabels = []string{"generic", "several", "other", "none"}

// OptionalArch is the label arch generic code need not build on: s390 has
// no wireless, Bluetooth or sound, so leaving it out does not make a change
// arch-specific.
const OptionalArch = "s390"

// BuildSymbols are the symbols each architecture's Kconfig forces on, with
// the build they name.
var BuildSymbols = [][2]string{{"CONFIG_X86_64", X86_64Build}, {"CONFIG_X86_32", "i386"}, {"CONFIG_ARM64", "arm64"},
	{"CONFIG_ARM", "arm"}, {"CONFIG_PPC", "powerpc"}, {"CONFIG_S390", "s390"}, {"CONFIG_RISCV", "riscv"}, {"CONFIG_LOONGARCH", "loongarch"}}

// SrcArch returns the arch/ directory of a label arch in the tree,
// arch/x86_64 for x86 before v2.6.24, or "" when the tree has none.
func SrcArch(fsys fs.FS, arch string) string {
	dirs := []string{arch}
	if arch == "x86" {
		dirs = append(dirs, "x86_64")
	}
	for _, d := range dirs {
		if _, err := fs.Stat(fsys, "arch/"+d+"/Kconfig"); err == nil {
			return d
		}
	}
	return ""
}

// srcArchDirs are the directories whose Makefiles add -I$(src)/$(SRCARCH),
// keeping a directory of assembly and glue code per architecture.
var srcArchDirs = []string{"lib/crc/", "lib/crypto/", "lib/raid/raid6/", "lib/raid/xor/"}

// archNames are the directories of arch/.
var archNames = map[string]bool{"alpha": true, "arc": true, "arm": true, "arm64": true, "csky": true, "hexagon": true,
	"loongarch": true, "m68k": true, "microblaze": true, "mips": true, "nios2": true, "openrisc": true, "parisc": true,
	"powerpc": true, "riscv": true, "s390": true, "sh": true, "sparc": true, "um": true, "x86": true, "xtensa": true}

// PathArch is the architecture of a path under arch/, with the old i386,
// x86_64, ppc and sparc64 directories folded into their successors, or
// under <dir>/<arch>/ for one of srcArchDirs; "" for other paths.
func PathArch(p string) string {
	for _, dir := range srcArchDirs {
		if rest, ok := strings.CutPrefix(p, dir); ok {
			if a, _, _ := strings.Cut(rest, "/"); archNames[a] {
				return a
			}
		}
	}
	rest, ok := strings.CutPrefix(p, "arch/")
	if !ok {
		return ""
	}
	a, sub, _ := strings.Cut(rest, "/")
	switch a {
	case "x86":
		if strings.HasPrefix(sub, "um/") {
			return "um" // user-mode Linux on an x86 host
		}
	case "i386", "x86_64":
		return "x86"
	case "ppc", "ppc64":
		return "powerpc"
	case "sparc64":
		return "sparc"
	}
	return a
}

// PageSize is a page size an architecture may have, with the Kconfig
// symbol that selects it.
type PageSize struct {
	Bytes  uint64
	Symbol string
}

// PageSizes are the page sizes of the label arches that offer a choice;
// the others have 4 KiB pages. A symbol an older tree lacks is opaque
// there, so the comparison stays unknown.
var PageSizes = map[string][]PageSize{
	"arm64":   {{4 << 10, "CONFIG_ARM64_4K_PAGES"}, {16 << 10, "CONFIG_ARM64_16K_PAGES"}, {64 << 10, "CONFIG_ARM64_64K_PAGES"}},
	"powerpc": {{4 << 10, "CONFIG_PPC_4K_PAGES"}, {16 << 10, "CONFIG_PPC_16K_PAGES"}, {64 << 10, "CONFIG_PPC_64K_PAGES"}, {256 << 10, "CONFIG_PPC_256K_PAGES"}},
	"loongarch": {{4 << 10, "CONFIG_4KB_2LEVEL"}, {4 << 10, "CONFIG_4KB_3LEVEL"}, {4 << 10, "CONFIG_4KB_4LEVEL"},
		{16 << 10, "CONFIG_16KB_2LEVEL"}, {16 << 10, "CONFIG_16KB_3LEVEL"}, {64 << 10, "CONFIG_64KB_2LEVEL"}, {64 << 10, "CONFIG_64KB_3LEVEL"}},
}
