package builds

import (
	"slices"

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
	StatusNotBuilt                   // another arch, the other word size, or a directory never entered outside arch/
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

// Combine is the condition of a change given the files that count in it
// (see Counted), each with the region of its lines that count: the OR of
// what each contributes (see Term). cond is nil when no file has a known
// condition.
func Combine(files []FileCond) (cond *kcond.Expr, unknown bool) {
	var terms []*kcond.Expr
	for _, f := range files {
		if e := Term(f); e != nil {
			terms = append(terms, e)
		} else {
			unknown = true
		}
	}
	if len(terms) == 0 {
		return nil, true
	}
	return kcond.Or(terms...), unknown
}

// Term is what a changed file contributes to the condition of its change.
// Sources, and headers built where the files including them are,
// contribute their Kbuild condition ANDed with their region; a source that
// is not built contributes false, and one whose condition is unknown nil.
// The other headers have no Kbuild condition and contribute their region
// when it names a Kconfig symbol or is never compiled, and nil otherwise.
func Term(f FileCond) *kcond.Expr {
	switch {
	case f.Status == StatusBuilt:
		return kcond.And(f.Kbuild, f.Region)
	case f.Status == StatusNotBuilt || f.Header && f.Region.IsConst(false):
		return kcond.Const(false)
	case f.Header && len(f.Region.Symbols()) > 0:
		return f.Region
	}
	return nil
}

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
