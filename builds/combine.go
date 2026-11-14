package builds

import (
	"slices"
	"strings"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/kbuild"
	"github.com/linux-analysis/kcond/kernel"
)

// Status tells how a build relates to a changed file.
type Status uint8

// The statuses of a file; the zero Status is that of a file not looked up.
const (
	StatusBuilt    Status = iota + 1 // listed by a Makefile or #included by a listed source; Kbuild holds
	StatusUnlisted                   // neither listed by a Makefile the crawl follows nor #included by a listed source
	StatusNotBuilt                   // another arch, the other word size, a directory never entered outside arch/, or a condition the Kconfig never lets hold
	StatusHeader                     // a header no listed file includes
	StatusMissing                    // not found at any revision tried
)

func (s Status) String() string {
	return [...]string{"", "built", "unlisted", "not built", "header", "missing"}[s]
}

// FileCond is a changed file with its conditions in a build.
type FileCond struct {
	kernel.File
	Status Status
	Kbuild *kcond.Expr // set when Status is StatusBuilt
	Region *kcond.Expr

	// What Eval found before writing the conditions plainly, for MayBuild
	// and AlwaysBuilds: the crawl's status and condition of the file and the region
	// as the build reads it.
	found          kbuild.Status
	kbuild, region *kcond.Expr
}

// Term is what a changed file contributes to the condition of its change,
// the OR of its files' terms. Sources, and headers built where the files
// including them are, contribute their Kbuild condition ANDed with their
// region (see within); a source that is not built contributes false. The
// other headers have no Kbuild condition and contribute their region when
// it names a Kconfig symbol or is never compiled. A file whose condition
// is unknown contributes its Unknown atom.
func Term(f FileCond) *kcond.Expr {
	switch {
	case f.Status == StatusBuilt:
		return within(f.Kbuild, f.Region)
	case f.Status == StatusNotBuilt || f.Header && f.Region.IsConst(false):
		return kcond.Const(false)
	case f.Header && len(f.Region.Symbols()) > 0:
		return f.Region
	}
	return Unknown(f.Path)
}

// within is k && r, written as r where r holds only where k does, as a
// region read through the files including a source holds only where they
// are compiled (see kbuild.Result.Macros), which kcond.And cannot see.
func within(k, r *kcond.Expr) *kcond.Expr {
	if rs := r.Symbols(); !slices.ContainsFunc(k.Symbols(), func(s string) bool { return !slices.Contains(rs, s) }) {
		if implies, ok := kcond.Implies(r, k); ok && implies {
			return r
		}
	}
	return kcond.And(k, r)
}

// Unknown is the condition of a file that is read where kcond cannot
// tell: an opaque atom naming it, which no configuration decides.
func Unknown(path string) *kcond.Expr {
	return kcond.Var(unknownAtom+path+")", kcond.Enabled)
}

// IsUnknown reports whether e is the Unknown atom of a file.
func IsUnknown(e *kcond.Expr) bool {
	return e.Op == kcond.OpVar && strings.HasPrefix(e.Name, unknownAtom)
}

const unknownAtom = "unknown("

// Counted returns the conditions of the changed lines of a file that count
// in its change, terms being those of all its changed lines and sources
// telling whether a source of the change changes code; none when no line
// counts. A change to a source is decided by the options
// of its sources: next to one, a header counts only through its lines
// under a CONFIG_ condition, an #ifdef or IS_ENABLED() of its own, as a
// header's other lines are read wherever the files including it are,
// which says nothing of the change. Every line of a source counts, and
// every line of a header when no source changes.
func Counted(f kernel.File, terms []*kcond.Expr, sources bool) []*kcond.Expr {
	if !f.Header || !sources {
		return terms
	}
	return slices.DeleteFunc(slices.Clone(terms), func(t *kcond.Expr) bool { return len(t.Symbols()) == 0 })
}
