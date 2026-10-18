package builds

import (
	"testing"

	"github.com/linux-analysis/kcond"
)

func p(t *testing.T, s string) *kcond.Expr {
	t.Helper()
	e, err := kcond.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

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
	} {
		if got := PathArch(p); got != want {
			t.Errorf("PathArch(%s) = %q, want %q", p, got, want)
		}
	}
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
		{"headers ignored next to sources", []FileCond{
			{Status: StatusBuilt, Kbuild: p(t, "CONFIG_A"), Region: tr},
			{Header: true, Status: StatusHeader, Region: tr},
		}, "CONFIG_A", false},
		{"unlisted source", []FileCond{
			{Status: StatusBuilt, Kbuild: p(t, "CONFIG_A"), Region: tr},
			{Status: StatusUnlisted, Region: tr},
		}, "CONFIG_A", true},
		{"other arch only", []FileCond{
			{Status: StatusNotBuilt, Region: tr},
		}, "false", false},
		{"header region", []FileCond{
			{Header: true, Status: StatusHeader, Region: p(t, "CONFIG_H=y")},
		}, "CONFIG_H=y", false},
		{"plain header", []FileCond{
			{Header: true, Status: StatusHeader, Region: tr},
		}, "", true},
		{"dead header region", []FileCond{
			{Header: true, Status: StatusHeader, Region: kcond.Const(false)},
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
