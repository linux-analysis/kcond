package builds

import (
	"strings"
	"testing"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/kernel"
)

func p(t *testing.T, s string) *kcond.Expr {
	t.Helper()
	e, err := kcond.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestTerm(t *testing.T) {
	tr := kcond.Const(true)
	src, h := kernel.File{Path: "lib/c.c"}, kernel.File{Path: "include/x.h", Header: true}
	for _, tc := range []struct {
		name    string
		file    FileCond
		want    string
		unknown bool
	}{
		{"source and region", FileCond{File: src, Status: StatusBuilt, Kbuild: p(t, "CONFIG_A"), Region: p(t, "CONFIG_R")}, "CONFIG_A && CONFIG_R", false},
		{"built header", FileCond{File: h, Status: StatusBuilt, Kbuild: p(t, "CONFIG_H"), Region: p(t, "CONFIG_R")}, "CONFIG_H && CONFIG_R", false},
		{"unlisted source", FileCond{File: src, Status: StatusUnlisted, Region: tr}, `"unknown(lib/c.c)"`, true},
		{"no build at the revision", FileCond{File: src, Region: tr}, `"unknown(lib/c.c)"`, true},
		{"other arch only", FileCond{File: src, Status: StatusNotBuilt, Region: tr}, "false", false},
		{"header region", FileCond{File: h, Status: StatusHeader, Region: p(t, "CONFIG_H=y")}, "CONFIG_H=y", false},
		{"plain header", FileCond{File: h, Status: StatusHeader, Region: tr}, `"unknown(include/x.h)"`, true},
		{"dead header region", FileCond{File: h, Status: StatusHeader, Region: kcond.Const(false)}, "false", false},
	} {
		e := Term(tc.file)
		if e.String() != tc.want || IsUnknown(e) != tc.unknown {
			t.Errorf("%s: Term = %s (unknown %v), want %s (unknown %v)", tc.name, e, IsUnknown(e), tc.want, tc.unknown)
		}
	}
	// A macro's atom is opaque too, but not an unknown file.
	if IsUnknown(p(t, `"defined(FOO)"`)) {
		t.Error(`IsUnknown("defined(FOO)") = true`)
	}
}

func TestCounted(t *testing.T) {
	terms := []*kcond.Expr{kcond.Const(true), p(t, "CONFIG_X"), p(t, `"defined(MODULE)"`), p(t, "!CONFIG_Y && CONFIG_Z=m")}
	h := kernel.File{Path: "include/linux/x.h", Header: true}
	for _, tc := range []struct {
		name    string
		f       kernel.File
		terms   []*kcond.Expr
		sources bool
		want    string
	}{
		{"a source", kernel.File{Path: "lib/a.c"}, terms, true, `true, CONFIG_X, "defined(MODULE)", !CONFIG_Y && CONFIG_Z=m`},
		{"a header alone", h, terms, false, `true, CONFIG_X, "defined(MODULE)", !CONFIG_Y && CONFIG_Z=m`},
		// Next to a source, only the lines under a CONFIG_ condition.
		{"a header next to a source", h, terms, true, "CONFIG_X, !CONFIG_Y && CONFIG_Z=m"},
		{"a common header next to a source", h, terms[:1], true, ""},
	} {
		var got []string
		for _, e := range Counted(tc.f, tc.terms, tc.sources) {
			got = append(got, e.String())
		}
		if s := strings.Join(got, ", "); s != tc.want {
			t.Errorf("%s: Counted = %q, want %q", tc.name, s, tc.want)
		}
	}
}
