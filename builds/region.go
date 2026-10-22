package builds

import (
	"strings"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/kbuild"
	"github.com/linux-analysis/kcond/kernel"
	"github.com/linux-analysis/kcond/source"
)

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

// pageSize replaces the page size comparisons of region with what b's
// architecture gives.
func (b Build) pageSize(region *kcond.Expr) *kcond.Expr {
	return kcond.Subst(region, func(atom string) *kcond.Expr {
		holds, ok := source.PageSizeHolds(atom, 4<<10)
		if !ok {
			return nil
		}
		sizes, choice := kernel.PageSizes[b.Arch]
		if !choice {
			return kcond.Const(holds)
		}
		var terms []*kcond.Expr
		for _, s := range sizes {
			if holds, _ := source.PageSizeHolds(atom, s.Bytes); holds {
				terms = append(terms, kcond.Var(s.Symbol, kcond.Enabled))
			}
		}
		return kcond.Or(terms...)
	})
}
