package builds

import (
	"strings"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/kbuild"
)

// pageSize is a page size an architecture may have, with the Kconfig
// symbol that selects it.
type pageSize struct {
	bytes  uint64
	symbol string
}

// pageSizes are the page sizes of the label arches that offer a choice;
// the others have 4 KiB pages. A symbol an older tree lacks is opaque
// there (see free), so the comparison stays unknown.
var pageSizes = map[string][]pageSize{
	"arm64":   {{4 << 10, "CONFIG_ARM64_4K_PAGES"}, {16 << 10, "CONFIG_ARM64_16K_PAGES"}, {64 << 10, "CONFIG_ARM64_64K_PAGES"}},
	"powerpc": {{4 << 10, "CONFIG_PPC_4K_PAGES"}, {16 << 10, "CONFIG_PPC_16K_PAGES"}, {64 << 10, "CONFIG_PPC_64K_PAGES"}, {256 << 10, "CONFIG_PPC_256K_PAGES"}},
	"loongarch": {{4 << 10, "CONFIG_4KB_2LEVEL"}, {4 << 10, "CONFIG_4KB_3LEVEL"}, {4 << 10, "CONFIG_4KB_4LEVEL"},
		{16 << 10, "CONFIG_16KB_2LEVEL"}, {16 << 10, "CONFIG_16KB_3LEVEL"}, {64 << 10, "CONFIG_64KB_2LEVEL"}, {64 << 10, "CONFIG_64KB_3LEVEL"}},
}

// moduleAtom is the atom of #ifdef MODULE, which Kbuild defines for the
// objects of a module.
const moduleAtom = "defined(MODULE)"

// Region returns the region of the changed lines of the file at path as
// build b reads it:
//
//   - A comparison of the page size holds or not with its architecture's
//     page sizes.
//   - MODULE is defined where b compiles the source into a module, and not
//     where it compiles it into the kernel; where b's crawl does not tell
//     either, the atom stays.
//   - A macro a -D flag of the source's object defines holds where b
//     compiles the source with the flag; elsewhere a header may still
//     define it, so the atom stays.
//
// Headers and sources b does not list keep the atoms of MODULE and -D
// macros.
func (b Build) Region(path string, region *kcond.Expr) *kcond.Expr {
	region = b.pageSize(region)
	if b.Kbuild == nil {
		return region
	}
	files, st := b.Kbuild.Lookup(path)
	if st != kbuild.Built {
		return region
	}
	for _, atom := range region.Opaque() {
		name, ok := strings.CutPrefix(atom, "defined(")
		if !ok {
			continue
		}
		if name, ok = strings.CutSuffix(name, ")"); !ok {
			continue
		}
		if atom == moduleAtom {
			builtin, module := b.Kbuild.Module(path)
			terms := []*kcond.Expr{kcond.And(builtin, kcond.With(region, atom, false)), kcond.And(module, kcond.With(region, atom, true))}
			if known, ok := kcond.Implies(files, kcond.Or(builtin, module)); !ok || !known {
				terms = append(terms, kcond.And(kcond.Not(builtin), kcond.Not(module), region))
			}
			region = kcond.Or(terms...)
		} else if set, unset := b.Kbuild.Define(path, name); !set.IsConst(false) {
			region = kcond.Or(kcond.And(set, kcond.With(region, atom, true)), kcond.And(unset, region))
		}
	}
	return region
}

// Plain writes the tests of e on symbols that cannot be m in build b
// plainly: X=y is X there, and X=m false. Kbuild conditions name both
// values of an option, obj-$(CONFIG_X) giving obj-y and obj-m, and the
// crawler cannot tell a bool from a tristate. Tristates, symbols b's
// Kconfig lacks, and every symbol when it could not be read, stay.
func (b Build) Plain(e *kcond.Expr) *kcond.Expr {
	if b.Values == nil || e == nil {
		return e
	}
	return kcond.Rewrite(e, func(v *kcond.Expr) *kcond.Expr {
		if !kcond.IsSymbol(v.Name) || v.State == kcond.Enabled {
			return nil
		}
		if modular, known := b.Values.Modular(v.Name); !known || modular {
			return nil
		}
		if v.State == kcond.Module {
			return kcond.Const(false)
		}
		return kcond.Var(v.Name, kcond.Enabled)
	})
}

// pageSize replaces the page size comparisons of region with what b's
// architecture gives.
func (b Build) pageSize(region *kcond.Expr) *kcond.Expr {
	return kcond.Subst(region, func(atom string) *kcond.Expr {
		holds, ok := kcond.PageSizeHolds(atom, 4<<10)
		if !ok {
			return nil
		}
		sizes, choice := pageSizes[b.Arch]
		if !choice {
			return kcond.Const(holds)
		}
		var terms []*kcond.Expr
		for _, s := range sizes {
			if holds, _ := kcond.PageSizeHolds(atom, s.bytes); holds {
				terms = append(terms, kcond.Var(s.symbol, kcond.Enabled))
			}
		}
		return kcond.Or(terms...)
	})
}
