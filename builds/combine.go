package builds

import (
	"slices"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/kbuild"
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

// KbuildStatus is the status of a source file in a crawl, with its Kbuild
// condition when it is built.
func KbuildStatus(res *kbuild.Result, path string) (Status, *kcond.Expr) {
	e, st := res.Lookup(path)
	switch st {
	case kbuild.Built:
		return StatusBuilt, e
	case kbuild.NotBuilt:
		return StatusNotBuilt, nil
	}
	return StatusUnlisted, nil
}

// FileCond is a changed file with its conditions.
type FileCond struct {
	Header bool
	Status Status
	Kbuild *kcond.Expr // set when Status is StatusBuilt
	Region *kcond.Expr
}

// Combine is the condition of a change given its files: the OR of what
// each contributes (see Term). cond is nil when no file has a known
// condition.
func Combine(files []FileCond) (cond *kcond.Expr, unknown bool) {
	sources := slices.ContainsFunc(files, func(f FileCond) bool { return !f.Header })
	var terms []*kcond.Expr
	for _, f := range files {
		switch e, ok := Term(f, sources); {
		case !ok:
		case e == nil:
			unknown = true
		default:
			terms = append(terms, e)
		}
	}
	if len(terms) == 0 {
		return nil, true
	}
	return kcond.Or(terms...), unknown
}

// Term is what a changed file contributes to the condition of its change,
// sources telling whether the change has a source among its files. Sources,
// and headers built where the files including them are, contribute their
// Kbuild condition ANDed with their region; a source that is not built
// contributes false, and one whose condition is unknown nil. The other
// headers have no Kbuild condition and count only when no source changed,
// a built header being no source, through their region when it names a
// Kconfig symbol or is never compiled; ok is false for one that does not
// count.
func Term(f FileCond, sources bool) (e *kcond.Expr, ok bool) {
	switch {
	case f.Status == StatusBuilt:
		return kcond.And(f.Kbuild, f.Region), true
	case f.Header && sources:
		return nil, false
	case f.Status == StatusNotBuilt || f.Header && f.Region.IsConst(false):
		return kcond.Const(false), true
	case f.Header && len(f.Region.Symbols()) > 0:
		return f.Region, true
	}
	return nil, true
}
