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

func TestCombine(t *testing.T) {
	tr := kcond.Const(true)
	for _, tc := range []struct {
		name    string
		files   []FileCond
		want    string
		unknown bool
	}{
		{"source and region", []FileCond{
			{Status: StatusBuilt, Kbuild: p(t, "CONFIG_A"), Region: p(t, "CONFIG_R")},
		}, "CONFIG_A && CONFIG_R", false},
		{"two sources", []FileCond{
			{Status: StatusBuilt, Kbuild: p(t, "CONFIG_A"), Region: tr},
			{Status: StatusBuilt, Kbuild: p(t, "CONFIG_B"), Region: tr},
		}, "CONFIG_A || CONFIG_B", false},
		{"built header counts next to sources", []FileCond{
			{Status: StatusBuilt, Kbuild: p(t, "CONFIG_A"), Region: tr},
			{File: kernel.File{Header: true}, Status: StatusBuilt, Kbuild: p(t, "CONFIG_H"), Region: p(t, "CONFIG_R")},
		}, "CONFIG_A || (CONFIG_H && CONFIG_R)", false},
		{"built header leaves other headers in", []FileCond{
			{File: kernel.File{Header: true}, Status: StatusBuilt, Kbuild: p(t, "CONFIG_H"), Region: tr},
			{File: kernel.File{Header: true}, Status: StatusHeader, Region: p(t, "CONFIG_P")},
		}, "CONFIG_H || CONFIG_P", false},
		{"unlisted source", []FileCond{
			{Status: StatusBuilt, Kbuild: p(t, "CONFIG_A"), Region: tr},
			{Status: StatusUnlisted, Region: tr},
		}, "CONFIG_A", true},
		{"other arch only", []FileCond{
			{Status: StatusNotBuilt, Region: tr},
		}, "false", false},
		{"header region", []FileCond{
			{File: kernel.File{Header: true}, Status: StatusHeader, Region: p(t, "CONFIG_H=y")},
		}, "CONFIG_H=y", false},
		{"plain header", []FileCond{
			{File: kernel.File{Header: true}, Status: StatusHeader, Region: tr},
		}, "", true},
		{"dead header region", []FileCond{
			{File: kernel.File{Header: true}, Status: StatusHeader, Region: kcond.Const(false)},
		}, "false", false},
		{"no files", nil, "", true},
	} {
		c, unknown := Combine(tc.files)
		got := ""
		if c != nil {
			got = c.String()
		}
		if got != tc.want || unknown != tc.unknown {
			t.Errorf("%s: Combine = %q, %v; want %q, %v", tc.name, got, unknown, tc.want, tc.unknown)
		}
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
