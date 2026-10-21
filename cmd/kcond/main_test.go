package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// capture runs the command and returns its output and exit code.
func capture(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code := run(args, &out, &out)
	return out.String(), code
}

func TestEvalAndConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".config")
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.43 Kernel Configuration\nCONFIG_A=y\nCONFIG_B=m\n# CONFIG_C is not set\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An option the file does not list is off; only an atom no .config
	// decides, such as a macro, is unknown.
	out, code := capture(t, "--config", cfg, "eval", "CONFIG_A && CONFIG_B", "CONFIG_B=y", "CONFIG_D", `"defined(FOO)"`)
	if code != 0 || !strings.Contains(out, "CONFIG_A && CONFIG_B\tapplies") || !strings.Contains(out, "CONFIG_B=y\tnot applicable") ||
		!strings.Contains(out, "CONFIG_D\tnot applicable") || !strings.Contains(out, "\"defined(FOO)\"\tunknown") {
		t.Errorf("eval = %q, %d", out, code)
	}
	out, code = capture(t, "--json", "config", cfg)
	if code != 0 || !strings.Contains(out, `"build": "x86_64"`) || !strings.Contains(out, `"version": "6.12.43"`) {
		t.Errorf("config = %q, %d", out, code)
	}
	if out, _ := capture(t, "--json", "--config", cfg, "eval", "CONFIG_A && CONFIG_B"); !strings.Contains(out, `"condition": "CONFIG_A && CONFIG_B"`) {
		t.Errorf("eval --json = %q, want && unescaped", out)
	}
	if _, code := capture(t, "eval"); code != 2 {
		t.Errorf("eval without a config exited %d, want 2", code)
	}
	if out, _ := capture(t, "--config", cfg, "eval", "CONFIG_A &&"); out != "kcond: parse \"CONFIG_A &&\" at 11: expected an operand\n" {
		t.Errorf("eval of a bad condition = %q", out)
	}
	if _, code := capture(t, "nonsense"); code != 2 {
		t.Errorf("unknown command exited %d, want 2", code)
	}
}

func TestMessages(t *testing.T) {
	dir, _ := newKernel(t)
	var out, errs bytes.Buffer
	if code := run([]string{"-h"}, &out, &errs); code != 0 || !strings.HasPrefix(out.String(), "usage: kcond") || errs.Len() != 0 {
		t.Errorf("-h = %q, %q, %d", out.String(), errs.String(), code)
	}
	if out, code := capture(t, "help"); code != 0 || !strings.HasPrefix(out, "usage: kcond") {
		t.Errorf("help = %q, %d", out, code)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-x"}, "flag provided but not defined: -x\nrun kcond -h for usage\n"},
		{[]string{"--tree", dir, "cond", "lib/a.c:5-"}, "kcond: usage: lib/a.c:5-: lines are LINE or LINE-LINE, from 1\n"},
		{[]string{"--tree", dir, "cond", "lib/a.c:1:5"}, "kcond: usage: lib/a.c:1:5: lines are LINE or LINE-LINE, from 1\n"},
		{[]string{"--tree", dir, "--rev", "nonesuch", "cond", "lib/a.c"}, "kcond: Not a valid object name nonesuch\n"},
	} {
		if out, code := capture(t, c.args...); code != 2 || !strings.HasSuffix(out, c.want) {
			t.Errorf("%v = %q, %d; want %q", c.args, out, code, c.want)
		}
	}
}

// TestMain drops the GIT_ variables of the calling environment: under git
// rebase --exec, GIT_DIR points the tests' git commands at the repository
// being rebased.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") {
			os.Unsetenv(k)
		}
	}
	os.Exit(m.Run())
}

// git runs git in dir as a test committer.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newKernel builds a kernel tree of two commits with an x86 and an arm64
// build: lib/a.c under CONFIG_A with an #ifdef CONFIG_B block, lib/b.c
// under the tristate CONFIG_B, lib/c.c and arch/arm64/lib/z.c that no
// Makefile lists, a header, and a fix that changes a line of a.c and only
// a comment of b.c. It returns the tree and the fix's hash.
func newKernel(t *testing.T) (dir, fix string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir = filepath.Join(t.TempDir(), "linux")
	git(t, t.TempDir(), "init", "-q", "-b", "master", dir)
	write := func(files map[string]string) string {
		for name, content := range files {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "add", name)
		}
		git(t, dir, "commit", "-q", "-m", "change")
		return git(t, dir, "rev-parse", "HEAD")
	}
	write(map[string]string{
		"Kbuild":             "# top-level objects\n",
		"Makefile":           "core-y := lib/\n",
		"arch/x86/Kconfig":   "config X86\n\tdef_bool y\n\nconfig MODULES\n\tbool \"m\"\n\nconfig A\n\tbool \"a\"\n\nconfig B\n\ttristate \"b\"\n\tdepends on A\n",
		"arch/arm64/Kconfig": "config ARM64\n\tdef_bool y\n\nconfig A\n\tbool \"a\"\n",
		"lib/Makefile":       "obj-$(CONFIG_A) += a.o\nobj-$(CONFIG_B) += b.o\n",
		"lib/a.c":            "int a;\n#ifdef CONFIG_B\nint ab;\n#endif\n",
		"lib/b.c":            "/* b */\nint b;\n",
		"lib/c.c":            "int c;\n",
		"arch/arm64/lib/z.c": "int z;\n",
		"include/linux/x.h":  "int x(void);\n",
	})
	fix = write(map[string]string{"lib/a.c": "int a = 1;\n#ifdef CONFIG_B\nint ab;\n#endif\n", "lib/b.c": "/* the b */\nint b;\n"})
	return dir, fix
}

func TestCond(t *testing.T) {
	dir, _ := newKernel(t)
	out, code := capture(t, "--tree", dir, "--rev", "HEAD", "--build", "x86_64", "cond", "lib/a.c", "lib/b.c", "lib/c.c", "include/linux/x.h")
	for _, want := range []string{"lib/a.c\tx86_64\tbuilt\tCONFIG_A\ttrue\tCONFIG_A\n", "lib/b.c\tx86_64\tbuilt\tCONFIG_B\ttrue\tCONFIG_B\n",
		"lib/c.c\tx86_64\tunlisted\t\ttrue\tunknown\n", "include/linux/x.h\tx86_64\theader\t\ttrue\tunknown\n"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("cond = %q, %d: want %q", out, code, want)
		}
	}
	// The checkout reads the same without --rev, and so does a plain copy
	// of it; without --build every build has a row.
	plain := filepath.Join(t.TempDir(), "src")
	git(t, dir, "clone", "-q", dir, plain)
	if err := os.RemoveAll(filepath.Join(plain, ".git")); err != nil {
		t.Fatal(err)
	}
	for _, tree := range []string{dir, plain} {
		out, code = capture(t, "--tree", tree, "cond", "lib/a.c")
		for _, want := range []string{"lib/a.c\tx86_64\tbuilt\tCONFIG_A\ttrue\tCONFIG_A\n", "lib/a.c\ti386\tbuilt\tCONFIG_A\ttrue\tCONFIG_A\n", "lib/a.c\tarm64\tbuilt\tCONFIG_A\ttrue\tCONFIG_A\n"} {
			if code != 0 || !strings.Contains(out, want) {
				t.Errorf("cond --tree %s = %q, %d: want %q", tree, out, code, want)
			}
		}
	}
	if _, code := capture(t, "--tree", dir, "--rev", "HEAD", "--build", "mips", "cond", "lib/a.c"); code != 2 {
		t.Errorf("an unknown build exited %d, want 2", code)
	}
}

func TestArches(t *testing.T) {
	dir, _ := newKernel(t)
	out, code := capture(t, "--tree", dir, "--rev", "HEAD", "arches", "lib/a.c", "arch/arm64/lib/z.c")
	// z.c is another arch's on x86 and unlisted on arm64, where it may
	// still be built by rules the crawl does not follow, so arm64 is
	// possible and unknown.
	for _, want := range []string{"lib/a.c\tarches x86,arm64\tunknown \talways \n", "arch/arm64/lib/z.c\tarches arm64\tunknown arm64\talways \n", "label: generic\n"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("arches = %q, %d: want %q", out, code, want)
		}
	}
}

func TestSymbols(t *testing.T) {
	dir, _ := newKernel(t)
	out, code := capture(t, "--tree", dir, "--rev", "HEAD", "--build", "x86_64", "symbols", "B", "CONFIG_A")
	for _, want := range []string{"CONFIG_B\tx86_64\tvalues nmy\tmodular true\trequires CONFIG_A\n", "CONFIG_A\tx86_64\tvalues ny\tmodular false\trequires \n"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("symbols = %q, %d: want %q", out, code, want)
		}
	}
}

func TestSymbolsUnknown(t *testing.T) {
	dir, _ := newKernel(t)
	out, code := capture(t, "--tree", dir, "--build", "x86_64", "symbols", "NOPE")
	if code != 0 || out != "CONFIG_NOPE\tx86_64\tunknown: no bool or tristate symbol of this name\n" {
		t.Errorf("symbols NOPE = %q, %d", out, code)
	}
}

// diff leaves out a file whose changed lines are all comments, so that
// it does not make the condition unknown.
func TestDiffSkipsCommentOnlyFiles(t *testing.T) {
	dir, fix := newKernel(t)
	out, code := capture(t, "--tree", dir, "--json", "diff", fix)
	var res []commitResult
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || len(res) != 1 {
		t.Fatalf("diff = %q, %d", out, code)
	}
	if r := res[0]; len(r.Files) != 1 || r.Files[0].Path != "lib/a.c" || r.Unknown || r.Condition != "CONFIG_A" {
		t.Errorf("diff = %+v", r)
	}
}

// A commit the build reads nothing of does not apply; one that changes a
// Makefile may.
func TestDiffOutsideBuild(t *testing.T) {
	dir, _ := newKernel(t)
	commit := func(name, content string) string {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "add", name)
		git(t, dir, "commit", "-q", "-m", name)
		return git(t, dir, "rev-parse", "HEAD")
	}
	docs := commit("Documentation/a.rst", "A\n")
	comment := commit("lib/c.c", "/* c */\nint c;\n")
	makefile := commit("lib/Makefile", "obj-$(CONFIG_A) += a.o c.o\nobj-$(CONFIG_B) += b.o\n")
	cfg := filepath.Join(t.TempDir(), ".config")
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\nCONFIG_A=y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := capture(t, "--tree", dir, "--config", cfg, "diff", docs, comment, makefile)
	want := docs + "\tnot applicable\tfalse\n" + comment + "\tnot applicable\tfalse\n" + makefile + "\tunknown\tunknown\n"
	if code != 0 || out != want {
		t.Errorf("diff = %q, %d; want %q", out, code, want)
	}
	// Every build is read without --config: the label shows.
	out, code = capture(t, "--tree", dir, "diff", docs, comment, makefile)
	want = docs + "\tnone\tfalse\n" + comment + "\tnone\tfalse\n" + makefile + "\t\tunknown\n"
	if code != 0 || out != want {
		t.Errorf("diff = %q, %d; want %q", out, code, want)
	}
}

// A commit's files are judged by the Makefiles at the commit, so a file
// it lists for the first time is built; a file it deletes is read at the
// parent.
func TestDiffReadsMakefilesAtCommit(t *testing.T) {
	dir, _ := newKernel(t)
	if err := os.WriteFile(filepath.Join(dir, "lib", "Makefile"), []byte("obj-$(CONFIG_A) += a.o\nobj-$(CONFIG_B) += b.o c.o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "c.c"), []byte("int c = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "rm", "-q", "lib/b.c")
	git(t, dir, "commit", "-q", "-a", "-m", "c for b")
	head := git(t, dir, "rev-parse", "HEAD")
	out, code := capture(t, "--tree", dir, "--json", "--build", "x86_64", "diff", head)
	var res []commitResult
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || len(res) != 1 || len(res[0].Files) != 2 {
		t.Fatalf("diff = %q, %d", out, code)
	}
	for _, f := range res[0].Files {
		switch f.Path {
		case "lib/c.c":
			if f.Status != "built" || f.Kbuild != "CONFIG_B" || f.Rev != head {
				t.Errorf("lib/c.c = %+v, want built under CONFIG_B at %s", f, head)
			}
		case "lib/b.c":
			if f.Status != "built" || f.Rev != head+"^" {
				t.Errorf("lib/b.c = %+v, want built at the parent", f)
			}
		default:
			t.Errorf("unexpected file %+v", f)
		}
	}
}

// A file row writes the region as the build reads it, like the condition:
// a bool tested with #ifdef is the bare symbol, not X=y; a tristate keeps
// its =y.
func TestDiffRegionPlain(t *testing.T) {
	dir, _ := newKernel(t)
	commit := func(ab, aa string) string {
		src := "int a = 1;\n#ifdef CONFIG_B\n" + ab + "\n#endif\n#ifdef CONFIG_A\n" + aa + "\n#endif\n"
		if err := os.WriteFile(filepath.Join(dir, "lib", "a.c"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "commit", "-q", "-a", "-m", "a")
		return git(t, dir, "rev-parse", "HEAD")
	}
	commit("int ab;", "int aa;")
	for _, c := range []struct{ ab, aa, want string }{
		{"int ab;", "int aa = 1;", "CONFIG_A\n  lib/a.c\tbuilt\tCONFIG_A\tregion CONFIG_A\n"},
		{"int ab = 2;", "int aa = 1;", "CONFIG_A && CONFIG_B=y\n  lib/a.c\tbuilt\tCONFIG_A\tregion CONFIG_B=y\n"},
	} {
		head := commit(c.ab, c.aa)
		if out, code := capture(t, "--tree", dir, "--build", "x86_64", "diff", head); code != 0 || out != head+"\t"+c.want {
			t.Errorf("diff = %q, %d; want %q", out, code, head+"\t"+c.want)
		}
	}
}

// diff judges a commit by the build --build names or the --config file is
// for: a change to arm64 code applies to an arm64 config.
func TestDiffBuild(t *testing.T) {
	dir, _ := newKernel(t)
	commit := func(files map[string]string) string {
		for name, content := range files {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "add", name)
		}
		git(t, dir, "commit", "-q", "-m", "arm64")
		return git(t, dir, "rev-parse", "HEAD")
	}
	commit(map[string]string{
		"Makefile":                   "core-y := lib/\ninclude $(srctree)/arch/$(SRCARCH)/Makefile\n",
		"arch/x86/Makefile":          "\n",
		"arch/arm64/Makefile":        "core-y += arch/arm64/kernel/\n",
		"arch/arm64/kernel/Makefile": "obj-y += cpu.o\n",
		"arch/arm64/kernel/cpu.c":    "int cpu;\n",
	})
	fix := commit(map[string]string{"arch/arm64/kernel/cpu.c": "int cpu = 1;\n"})
	arm64, x86 := filepath.Join(t.TempDir(), "arm64.config"), filepath.Join(t.TempDir(), "x86.config")
	if err := os.WriteFile(arm64, []byte("# Linux/arm64 6.12.0 Kernel Configuration\nCONFIG_ARM64=y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(x86, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args           []string
		verdict, label string
	}{
		// Only the build the flags pick is read, so there is no label.
		{[]string{"--config", arm64}, "applies", ""},
		{[]string{"--config", x86}, "not applicable", ""},
		{[]string{"--config", arm64, "--build", "x86_64"}, "not applicable", ""},
		{nil, "", "arm64"},
	} {
		out, code := capture(t, append([]string{"--tree", dir, "--json", "diff", fix}, c.args...)...)
		var res []commitResult
		if code != 0 || json.Unmarshal([]byte(out), &res) != nil || len(res) != 1 || res[0].Verdict != c.verdict || res[0].Label != c.label {
			t.Errorf("diff %v = %q, %d; want %s, label %q", c.args, out, code, c.verdict, c.label)
		}
	}
	// The text leaves out the label column and the arches.
	want := fix + "\ttrue\n  arch/arm64/kernel/cpu.c\tbuilt\ttrue\tregion true\n"
	if out, code := capture(t, "--tree", dir, "--build", "arm64", "diff", fix); code != 0 || out != want {
		t.Errorf("diff --build arm64 = %q, %d; want %q", out, code, want)
	}
}

// A build whose Makefiles cannot be read says why on stderr.
func TestBuildErrors(t *testing.T) {
	dir, _ := newKernel(t)
	git(t, dir, "rm", "-q", "Makefile")
	git(t, dir, "commit", "-q", "-m", "no Makefile")
	var out, errs bytes.Buffer
	code := run([]string{"--tree", dir, "--rev", "HEAD", "--build", "x86_64", "cond", "lib/a.c"}, &out, &errs)
	if code != 0 || !strings.Contains(out.String(), "lib/a.c\tx86_64\tunlisted") ||
		!strings.HasPrefix(errs.String(), "kcond: the x86_64 build: kbuild: no top-level Makefile") {
		t.Errorf("cond = %q, %q, %d; want unlisted, with the reason on stderr", out.String(), errs.String(), code)
	}
}

func TestFlagsAfterCommand(t *testing.T) {
	dir, _ := newKernel(t)
	out, code := capture(t, "cond", "lib/a.c", "--tree", dir, "--rev", "HEAD", "--build", "x86_64", "--json")
	if code != 0 || !strings.Contains(out, `"kbuild": "CONFIG_A"`) {
		t.Errorf("cond with flags after = %q, %d", out, code)
	}
	if _, code := capture(t, "arches", "lib/a.c", "--tree", dir, "--rev", "HEAD", "--build", "x86_64"); code != 2 {
		t.Errorf("arches --build exited %d, want 2", code)
	}
}

func TestVersion(t *testing.T) {
	if out, code := capture(t, "--version"); code != 0 || out != "kcond dev\n" {
		t.Errorf("--version = %q, %d", out, code)
	}
}

func TestTreeDefault(t *testing.T) {
	dir, _ := newKernel(t)
	t.Chdir(dir)
	if out, code := capture(t, "cond", "lib/a.c", "--build", "x86_64"); code != 0 || !strings.Contains(out, "lib/a.c\tx86_64\tbuilt\tCONFIG_A\ttrue\tCONFIG_A\n") {
		t.Errorf("cond in the tree = %q, %d", out, code)
	}
	if _, code := capture(t, "--rev", "HEAD", "diff", "HEAD"); code != 2 {
		t.Errorf("diff --rev exited %d, want 2", code)
	}
	t.Chdir(t.TempDir())
	if _, code := capture(t, "cond", "lib/a.c"); code != 2 {
		t.Errorf("cond outside a tree exited %d, want 2", code)
	}
}

// Paths are relative to the current directory in the tree, as git reads
// them, or absolute; a path that names no file of the tree is an error.
func TestPaths(t *testing.T) {
	dir, _ := newKernel(t)
	want := "lib/a.c\tx86_64\tbuilt\tCONFIG_A\ttrue\tCONFIG_A\n"
	for _, p := range []string{"./lib/a.c", "lib/../lib/a.c", filepath.Join(dir, "lib", "a.c")} {
		if out, code := capture(t, "--tree", dir, "--build", "x86_64", "cond", p); code != 0 || out != want {
			t.Errorf("cond %s = %q, %d; want %q", p, out, code, want)
		}
	}
	t.Chdir(filepath.Join(dir, "lib"))
	if out, code := capture(t, "--build", "x86_64", "cond", "a.c"); code != 0 || out != want {
		t.Errorf("cond a.c in lib = %q, %d; want %q", out, code, want)
	}
	if out, code := capture(t, "files"); code != 0 || out != "lib/a.c\tCONFIG_A\nlib/b.c\tCONFIG_B\n" {
		t.Errorf("files in lib = %q, %d", out, code)
	}
	for _, args := range [][]string{{"cond", "nope.c"}, {"cond", "../../outside.c"}, {"arches", "nope.c"}, {"arches", "Makefile"}, {"files", "nope"}} {
		if out, code := capture(t, args...); code != 2 {
			t.Errorf("%v = %q, %d; want exit 2", args, out, code)
		}
	}
	// A directory that is not a kernel tree is an error, not an empty answer.
	for _, args := range [][]string{{"cond", "lib/a.c"}, {"symbols", "A"}, {"arches", "lib/a.c"}, {"files"}} {
		if out, code := capture(t, append([]string{"--tree", t.TempDir()}, args...)...); code != 2 || !strings.Contains(out, "is not a kernel tree") {
			t.Errorf("%v outside a tree = %q, %d", args, out, code)
		}
	}
}

func TestCondLines(t *testing.T) {
	dir, _ := newKernel(t)
	cfg := filepath.Join(t.TempDir(), ".config")
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\nCONFIG_MODULES=y\nCONFIG_A=y\n# CONFIG_B is not set\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The config picks the build; a line range adds the #if region of the
	// lines (#ifdef CONFIG_B holds for a built-in B) and the verdict on
	// their condition.
	out, code := capture(t, "--tree", dir, "--config", cfg, "cond", "lib/a.c:3", "lib/a.c:1-4", "lib/a.c")
	for _, want := range []string{"lib/a.c:3\tx86_64\tbuilt\tCONFIG_A\tCONFIG_B=y\tCONFIG_A && CONFIG_B=y\tnot applicable\n",
		"lib/a.c:1-4\tx86_64\tbuilt\tCONFIG_A\ttrue\tCONFIG_A\tapplies\n", "lib/a.c\tx86_64\tbuilt\tCONFIG_A\ttrue\tCONFIG_A\tapplies\n"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("cond = %q, %d: want %q", out, code, want)
		}
	}
	if strings.Contains(out, "arm64") {
		t.Errorf("cond read a build the config is not for: %q", out)
	}
	for _, bad := range []string{"lib/a.c:9", "lib/a.c:0", "lib/a.c:3-2", "Makefile"} {
		if _, code := capture(t, "--tree", dir, "cond", bad); code == 0 {
			t.Errorf("cond %s exited 0", bad)
		}
	}
}

func TestDiffRange(t *testing.T) {
	dir, fix := newKernel(t)
	base := git(t, dir, "rev-parse", fix+"^")
	if err := os.WriteFile(filepath.Join(dir, "lib", "b.c"), []byte("/* the b */\nint b = 2;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "commit", "-q", "-a", "-m", "b")
	third := git(t, dir, "rev-parse", "HEAD")
	cfg := filepath.Join(t.TempDir(), ".config")
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\nCONFIG_MODULES=y\nCONFIG_A=y\n# CONFIG_B is not set\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A range lists its commits oldest first; the commit line leads with
	// the verdict and the condition, and the file rows follow.
	out, code := capture(t, "--tree", dir, "--config", cfg, "diff", base+"..HEAD")
	var lines []string
	for l := range strings.Lines(out) {
		if !strings.HasPrefix(l, " ") {
			lines = append(lines, strings.TrimSuffix(l, "\n"))
		}
	}
	want := []string{fix + "\tapplies\tCONFIG_A", third + "\tnot applicable\tCONFIG_B"}
	if code != 0 || !slices.Equal(lines, want) || !strings.Contains(out, "\n  lib/b.c\tbuilt\tCONFIG_B\tregion true\n") {
		t.Errorf("diff range = %q, %d; want the commit lines %q", out, code, want)
	}
	if out, code := capture(t, "--tree", dir, "--json", "diff", "HEAD..HEAD"); code != 0 || out != "kcond: HEAD..HEAD has no commits\n[]\n" {
		t.Errorf("diff of an empty range = %q, %d", out, code)
	}
	// A.. and A...B read the Makefiles at the end too: HEAD.
	whole, _ := capture(t, "--tree", dir, "--json", "diff", base+"..HEAD")
	for _, r := range []string{base + "..", base + "...HEAD"} {
		if out, code := capture(t, "--tree", dir, "--json", "diff", r); code != 0 || out != whole || !strings.Contains(out, `"rev": "HEAD"`) {
			t.Errorf("diff %s = %q, %d; want %q", r, out, code, whole)
		}
	}
}

func TestQuiet(t *testing.T) {
	dir, fix := newKernel(t)
	cfg := filepath.Join(t.TempDir(), ".config")
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\nCONFIG_MODULES=y\nCONFIG_A=y\n# CONFIG_B is not set\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// -q prints nothing and exits 1 unless everything applies; unknown
	// does not apply.
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"--config", cfg, "-q", "eval", "CONFIG_A"}, 0},
		{[]string{"--config", cfg, "-q", "eval", "CONFIG_A", "CONFIG_B"}, 1},
		{[]string{"--config", cfg, "-q", "eval", `"defined(FOO)"`}, 1},
		{[]string{"--tree", dir, "--config", cfg, "-q", "diff", fix}, 0},
		{[]string{"--tree", dir, "--config", cfg, "-q", "cond", "lib/a.c", "lib/b.c"}, 1},
		{[]string{"--tree", dir, "--config", cfg, "-q", "cond", "lib/a.c:1"}, 0},
		// Nothing tells which builds include a header outside any #if.
		{[]string{"--tree", dir, "--config", cfg, "-q", "cond", "include/linux/x.h"}, 1},
		// -q needs a verdict; errors exit 2.
		{[]string{"--tree", dir, "-q", "diff", fix}, 2},
		{[]string{"--config", cfg, "-q", "config", cfg}, 2},
		{[]string{"--tree", dir, "--rev", "nonesuch", "cond", "lib/a.c"}, 2},
	} {
		if out, code := capture(t, c.args...); code != c.code || (c.code < 2 && out != "") {
			t.Errorf("%v = %q, %d; want exit %d", c.args, out, code, c.code)
		}
	}
}

func TestFiles(t *testing.T) {
	dir, _ := newKernel(t)
	if out, code := capture(t, "--tree", dir, "files", "lib"); code != 0 || out != "lib/a.c\tCONFIG_A\nlib/b.c\tCONFIG_B\n" {
		t.Errorf("files = %q, %d", out, code)
	}
	cfg := filepath.Join(t.TempDir(), ".config")
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\nCONFIG_MODULES=y\nCONFIG_A=y\n# CONFIG_B is not set\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The config picks the build and keeps the files it builds.
	if out, code := capture(t, "--tree", dir, "--config", cfg, "files"); code != 0 || out != "lib/a.c\tCONFIG_A\n" {
		t.Errorf("files --config = %q, %d", out, code)
	}
	if out, code := capture(t, "--tree", dir, "--json", "files", "include"); code != 0 || out != "[]\n" {
		t.Errorf("files of a directory without sources = %q, %d", out, code)
	}
	// A source #included only under #if 0 is never compiled.
	for name, content := range map[string]string{
		"lib/Makefile": "obj-$(CONFIG_A) += a.o\nobj-$(CONFIG_B) += b.o\nobj-y += dead.o\n",
		"lib/dead.c":   "#if 0\n#include \"never.c\"\n#endif\n",
		"lib/never.c":  "int never;\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, code := capture(t, "--tree", dir, "files", "lib"); code != 0 || out != "lib/a.c\tCONFIG_A\nlib/b.c\tCONFIG_B\nlib/dead.c\ttrue\n" {
		t.Errorf("files with a dead #include = %q, %d", out, code)
	}
}

func TestSymbolsConfig(t *testing.T) {
	dir, _ := newKernel(t)
	cfg := filepath.Join(t.TempDir(), ".config")
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\nCONFIG_MODULES=y\nCONFIG_A=y\n# CONFIG_B is not set\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The config adds each symbol's value and the values of what it
	// requires.
	out, code := capture(t, "--tree", dir, "--config", cfg, "symbols", "B", "A")
	for _, want := range []string{"CONFIG_B\tx86_64\tvalues nmy\tmodular true\tconfig n\trequires CONFIG_A=y\n",
		"CONFIG_A\tx86_64\tvalues ny\tmodular false\tconfig y\trequires \n"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("symbols = %q, %d: want %q", out, code, want)
		}
	}
	// JSON names the requirements the config leaves off.
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\nCONFIG_MODULES=y\nCONFIG_B=m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = capture(t, "--tree", dir, "--config", cfg, "--json", "symbols", "B")
	if code != 0 || !strings.Contains(out, `"config": "m"`) || !strings.Contains(out, "\"off\": [\n      \"CONFIG_A\"\n    ]") {
		t.Errorf("symbols --json = %q, %d", out, code)
	}
}
