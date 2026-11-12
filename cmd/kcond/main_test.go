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

	"github.com/linux-analysis/kcond/analysis"
	"github.com/linux-analysis/kcond/gitfs"
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
		{[]string{"-x"}, "kcond: flag provided but not defined: -x\nrun kcond -h for usage\n"},
		{[]string{"--tree", dir, "search", "HEAD", "--where", "A &&"}, "kcond: parse \"CONFIG_A &&\" at 11: expected an operand\n"},
		{[]string{"--tree", dir, "cond", "lib/a.c:5-"}, "kcond: usage: lib/a.c:5-: lines are LINE or LINE-LINE, from 1\n"},
		{[]string{"--tree", dir, "cond", "lib/a.c:1:5"}, "kcond: usage: lib/a.c:1:5: lines are LINE or LINE-LINE, from 1\n"},
		{[]string{"--tree", dir, "--rev", "nonesuch", "cond", "lib/a.c"}, "kcond: Not a valid object name nonesuch\n"},
		{[]string{"--tree", dir, "diff", "lib/a.c"}, "kcond: lib/a.c is not a commit\n"},
	} {
		if out, code := capture(t, c.args...); code != 2 || out != c.want {
			t.Errorf("%v = %q, %d; want %q", c.args, out, code, c.want)
		}
	}
	// git's error: is dropped as its fatal: is, and its hints with it.
	if got := message(&gitfs.Error{Stderr: "error: short object ID 1866 is ambiguous\nhint: The candidates are:"}); got != "short object ID 1866 is ambiguous" {
		t.Errorf("message = %q", got)
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
		"lib/c.c\tx86_64\tunlisted\t\ttrue\t\"unknown(lib/c.c)\"\n", "include/linux/x.h\tx86_64\theader\t\ttrue\t\"unknown(include/linux/x.h)\"\n"} {
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

// JSON writes an empty list as [], never null: a commit without kernel
// files, a file no build always compiles, a symbol without requirements
// and one the Kconfig lacks.
func TestJSONEmptyLists(t *testing.T) {
	dir, _ := newKernel(t)
	docs := commitFile(t, dir, "Documentation/a.rst", "A\n")
	for _, args := range [][]string{
		{"diff", docs},
		{"--rev", "HEAD", "arches", "lib/c.c"},
		{"--build", "x86_64", "symbols", "A", "NOPE"},
	} {
		if out, code := capture(t, append([]string{"--tree", dir, "--json"}, args...)...); code != 0 || strings.Contains(out, "null") {
			t.Errorf("%v = %q, %d; want no null", args, out, code)
		}
	}
}

// A commit is named by its full hash, whatever the argument calls it.
func TestFullHash(t *testing.T) {
	dir, fix := newKernel(t)
	if out, code := capture(t, "--tree", dir, "diff", "HEAD"); code != 0 || !strings.HasPrefix(out, fix+"\t") {
		t.Errorf("diff HEAD = %q, %d; want %s first", out, code, fix)
	}
	if out, code := capture(t, "--tree", dir, "search", "HEAD", "--where", "CONFIG_A"); code != 0 || !strings.HasPrefix(out, fix+"\t") {
		t.Errorf("search HEAD = %q, %d; want %s first", out, code, fix)
	}
	if out, code := capture(t, "--tree", dir, "--json", "diff", "HEAD"); code != 0 || !strings.Contains(out, `"rev": "`+fix+`"`) {
		t.Errorf("diff --json HEAD = %q, %d; want rev %s", out, code, fix)
	}
}

// diff leaves out a file whose changed lines are all comments, so that
// it does not make the condition unknown in part.
func TestDiffSkipsCommentOnlyFiles(t *testing.T) {
	dir, fix := newKernel(t)
	out, code := capture(t, "--tree", dir, "--json", "diff", fix)
	var res []analysis.CommitResult
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || len(res) != 1 {
		t.Fatalf("diff = %q, %d", out, code)
	}
	if r := res[0]; len(r.Files) != 1 || r.Files[0].Path != "lib/a.c" || r.Condition != "CONFIG_A" {
		t.Errorf("diff = %+v", r)
	}
}

// A file kcond cannot place is named in the condition, which a .config
// then leaves unknown unless the rest of it applies. Next to a kernel file
// a Makefile names nothing: the kernel file decides.
func TestDiffUnknownFiles(t *testing.T) {
	dir, _ := newKernel(t)
	commit := func(files map[string]string) string {
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "add", name)
		}
		git(t, dir, "commit", "-q", "-m", "change")
		return git(t, dir, "rev-parse", "HEAD")
	}
	part := commit(map[string]string{"lib/a.c": "int a = 2;\n#ifdef CONFIG_B\nint ab;\n#endif\n", "lib/c.c": "int c = 2;\n"})
	mixed := commit(map[string]string{"lib/a.c": "int a = 3;\n#ifdef CONFIG_B\nint ab;\n#endif\n", "lib/Makefile": "obj-$(CONFIG_A) += a.o\nobj-$(CONFIG_B) += b.o\nccflags-y += -DX\n"})
	for _, tc := range []struct{ config, part, mixed string }{
		{"", "unknown\t\"unknown(lib/c.c)\" || CONFIG_A", "not applicable\tCONFIG_A"},
		{"CONFIG_A=y\n", "applies\t\"unknown(lib/c.c)\" || CONFIG_A", "applies\tCONFIG_A"},
	} {
		cfg := filepath.Join(t.TempDir(), ".config")
		if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\n"+tc.config), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := capture(t, "--tree", dir, "--config", cfg, "diff", part, mixed)
		for _, want := range []string{part + "\t" + tc.part + "\n", mixed + "\t" + tc.mixed + "\n"} {
			if code != 0 || !strings.Contains(out, want) {
				t.Errorf("diff with %q = %q, %d; want %q", tc.config, out, code, want)
			}
		}
	}
}

// A file of unknown condition is not built only where an option holds,
// even one every configuration enables.
func TestSearchUnknownFile(t *testing.T) {
	dir, _ := newKernel(t)
	h := commitFile(t, dir, "lib/c.c", "int c = 2;\n")
	if out, code := capture(t, "--tree", dir, "search", h, "--where", "CONFIG_X86"); code != 0 || out != "" {
		t.Errorf("search --where CONFIG_X86 = %q, %d; want no match", out, code)
	}
}

// commitFile writes one file of the kernel at dir and commits it.
func commitFile(t *testing.T, dir, name, content string) string {
	t.Helper()
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

// Code under IS_REACHABLE(CONFIG_B) in an object built into the kernel
// needs B built in too: a.o is builtin, so B=m leaves it out.
func TestDiffReachable(t *testing.T) {
	dir, _ := newKernel(t)
	hash := commitFile(t, dir, "lib/a.c", "int a = 1;\n#if IS_REACHABLE(CONFIG_B)\nint ab = 2;\n#endif\n")
	cfg := filepath.Join(t.TempDir(), ".config")
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\nCONFIG_MODULES=y\nCONFIG_A=y\nCONFIG_B=m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := capture(t, "--tree", dir, "--config", cfg, "diff", hash)
	want := hash + "\tnot applicable\tCONFIG_A && CONFIG_B=y\n  lib/a.c\tbuilt\tCONFIG_A\tregion CONFIG_A && CONFIG_B=y\n"
	if code != 0 || out != want {
		t.Errorf("diff = %q, %d; want %q", out, code, want)
	}
}

// A commit the build reads nothing of does not apply, nor does one that
// only adds a comment to a line; one that changes a Makefile may, which
// its condition names.
func TestDiffOutsideBuild(t *testing.T) {
	dir, _ := newKernel(t)
	docs := commitFile(t, dir, "Documentation/a.rst", "A\n")
	comment := commitFile(t, dir, "lib/c.c", "/* c */\nint c;\n")
	makefile := commitFile(t, dir, "lib/Makefile", "obj-$(CONFIG_A) += a.o c.o\nobj-$(CONFIG_B) += b.o\n")
	moved := commitFile(t, dir, "lib/a.c", "int a = 1; /* a */\n#ifdef CONFIG_B\nint ab;\n#endif\n")
	cfg := filepath.Join(t.TempDir(), ".config")
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\nCONFIG_A=y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := capture(t, "--tree", dir, "--config", cfg, "diff", docs, comment, makefile, moved)
	want := docs + "\tnot applicable\tfalse\n" + comment + "\tnot applicable\tfalse\n" + makefile + "\tunknown\t\"unknown(lib/Makefile)\"\n" + moved + "\tnot applicable\tfalse\n"
	if code != 0 || out != want {
		t.Errorf("diff = %q, %d; want %q", out, code, want)
	}
	// Every build is read without --config: the label shows.
	out, code = capture(t, "--tree", dir, "diff", docs, comment, makefile, moved)
	want = docs + "\tnone\tfalse\n" + comment + "\tnone\tfalse\n" + makefile + "\t\t\"unknown(lib/Makefile)\"\n" + moved + "\tnone\tfalse\n"
	if code != 0 || out != want {
		t.Errorf("diff = %q, %d; want %q", out, code, want)
	}
}

// A file under scripts/ that no Makefile and no other script names is one
// the build never reads: a semantic patch, which coccicheck runs through a
// glob, reads none, while mksysmap, which link-vmlinux.sh names, stays
// unknown. The name is the base name without extension, as a whole word,
// and a file naming itself does not count; a Kbuild file is always read.
func TestDiffScripts(t *testing.T) {
	dir, _ := newKernel(t)
	commitFile(t, dir, "scripts/link-vmlinux.sh", "#!/bin/sh\nsh scripts/mksysmap vmlinux\necho foobar\n")
	mksysmap := commitFile(t, dir, "scripts/mksysmap", "#!/bin/sh\nnm $1\n")
	cocci := commitFile(t, dir, "scripts/coccinelle/api/x.cocci", "// x: a semantic patch\n")
	foo := commitFile(t, dir, "scripts/foo.sh", "echo foo\n")
	lib := commitFile(t, dir, "scripts/Makefile.lib", "# lib\n")
	out, code := capture(t, "--tree", dir, "diff", mksysmap, cocci, foo, lib)
	want := mksysmap + "\t\t\"unknown(scripts/mksysmap)\"\n" + cocci + "\tnone\tfalse\n" + foo + "\tnone\tfalse\n" + lib + "\t\t\"unknown(scripts/Makefile.lib)\"\n"
	if code != 0 || out != want {
		t.Errorf("diff = %q, %d; want %q", out, code, want)
	}
	cfg := filepath.Join(t.TempDir(), ".config")
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\nCONFIG_A=y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = capture(t, "--tree", dir, "--config", cfg, "diff", mksysmap, cocci)
	want = mksysmap + "\tunknown\t\"unknown(scripts/mksysmap)\"\n" + cocci + "\tnot applicable\tfalse\n"
	if code != 0 || out != want {
		t.Errorf("diff --config = %q, %d; want %q", out, code, want)
	}
	out, code = capture(t, "--tree", dir, "search", mksysmap+"^.."+lib, "--label", "none")
	want = cocci + "\t\tfalse\n" + foo + "\t\tfalse\n"
	if code != 0 || out != want {
		t.Errorf("search --label none = %q, %d; want %q", out, code, want)
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
	var res []analysis.CommitResult
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
		return commitFile(t, dir, "lib/a.c", "int a = 1;\n#ifdef CONFIG_B\n"+ab+"\n#endif\n#ifdef CONFIG_A\n"+aa+"\n#endif\n")
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

// A commit that only wraps a line in #ifdef CONFIG_A ... #endif is a change
// under CONFIG_A: the directive lines carry the branch they open.
func TestDiffWrapsLine(t *testing.T) {
	dir, _ := newKernel(t)
	head := commitFile(t, dir, "lib/a.c", "#ifdef CONFIG_A\nint a = 1;\n#endif\n#ifdef CONFIG_B\nint ab;\n#endif\n")
	want := head + "\tCONFIG_A\n  lib/a.c\tbuilt\tCONFIG_A\tregion CONFIG_A\n"
	if out, code := capture(t, "--tree", dir, "--build", "x86_64", "diff", head); code != 0 || out != want {
		t.Errorf("diff = %q, %d; want %q", out, code, want)
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
		var res []analysis.CommitResult
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
	// A side branch, and a merge of it that adds a line of its own.
	git(t, dir, "checkout", "-q", "-b", "side")
	side := commitFile(t, dir, "lib/b.c", "/* the b */\nint b = 3;\n")
	git(t, dir, "checkout", "-q", "master")
	git(t, dir, "merge", "-q", "--no-ff", "--no-commit", "side")
	merge := commitFile(t, dir, "lib/a.c", "int a = 1;\n#ifdef CONFIG_B\nint ab;\n#endif\nint evil;\n")
	cfg := filepath.Join(t.TempDir(), ".config")
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\nCONFIG_MODULES=y\nCONFIG_A=y\n# CONFIG_B is not set\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A range lists its commits oldest first, a merge with its own change
	// only; the commit line leads with the verdict and the condition, and
	// the file rows follow.
	out, code := capture(t, "--tree", dir, "--config", cfg, "diff", base+"..HEAD")
	var lines []string
	for l := range strings.Lines(out) {
		if !strings.HasPrefix(l, " ") {
			lines = append(lines, strings.TrimSuffix(l, "\n"))
		}
	}
	// The commits share a commit time, so only the merge, which descends
	// from them all, is surely last.
	want := []string{fix + "\tapplies\tCONFIG_A", third + "\tnot applicable\tCONFIG_B",
		side + "\tnot applicable\tCONFIG_B", merge + "\tapplies\tCONFIG_A"}
	sorted := slices.Sorted(slices.Values(lines))
	slices.Sort(want)
	if code != 0 || !slices.Equal(sorted, want) || !strings.Contains(out, "\n  lib/b.c\tbuilt\tCONFIG_B\tregion true\n") ||
		!strings.HasSuffix(out, merge+"\tapplies\tCONFIG_A\n  lib/a.c\tbuilt\tCONFIG_A\tregion true\n") {
		t.Errorf("diff range = %q, %d; want the commit lines %q", out, code, want)
	}
	if out, code := capture(t, "--tree", dir, "--json", "diff", "HEAD..HEAD"); code != 0 || out != "kcond: HEAD..HEAD has no commits\n[]\n" {
		t.Errorf("diff of an empty range = %q, %d", out, code)
	}
	// A.. and A...B read the Makefiles at the end too: HEAD.
	whole, _ := capture(t, "--tree", dir, "--json", "diff", base+"..HEAD")
	for _, r := range []string{base + "..", base + "...HEAD"} {
		if out, code := capture(t, "--tree", dir, "--json", "diff", r); code != 0 || out != whole || !strings.Contains(out, `"rev": "`+merge+`"`) {
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
	// A header is no source: files says so and lists the rest.
	var out, errs bytes.Buffer
	if code := run([]string{"--tree", dir, "files", "include/linux/x.h", "lib"}, &out, &errs); code != 0 ||
		out.String() != "lib/a.c\tCONFIG_A\nlib/b.c\tCONFIG_B\n" || errs.String() != "kcond: include/linux/x.h is a header: files lists sources\n" {
		t.Errorf("files of a header = %q, %q, %d", out.String(), errs.String(), code)
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

// A header the listed sources include is built under their condition, as
// a file asked about and as a change.
func TestCondHeader(t *testing.T) {
	dir, _ := newKernel(t)
	commitFile(t, dir, "lib/a.c", "#include \"a.h\"\nint a = 1;\n")
	hash := commitFile(t, dir, "lib/a.h", "int ah;\n")
	out, code := capture(t, "--tree", dir, "--rev", "HEAD", "--build", "x86_64", "cond", "lib/a.h")
	if want := "lib/a.h\tx86_64\tbuilt\tCONFIG_A\ttrue\tCONFIG_A\n"; code != 0 || out != want {
		t.Errorf("cond = %q, %d; want %q", out, code, want)
	}
	out, code = capture(t, "--tree", dir, "--build", "x86_64", "diff", hash)
	if want := hash + "\tCONFIG_A\n  lib/a.h\tbuilt\tCONFIG_A\tregion true\n"; code != 0 || out != want {
		t.Errorf("diff = %q, %d; want %q", out, code, want)
	}
	// A public header too, through <...> and the guard of the header that
	// includes it.
	commitFile(t, dir, "lib/a.c", "#include \"a.h\"\n#include <linux/x.h>\nint a = 1;\n")
	hash = commitFile(t, dir, "include/linux/x.h", "#ifndef X_H\n#define X_H\n#include <linux/y.h>\n#endif\n")
	commitFile(t, dir, "include/linux/y.h", "int y;\n")
	out, code = capture(t, "--tree", dir, "--rev", "HEAD", "--build", "x86_64", "cond", "include/linux/x.h", "include/linux/y.h")
	if want := "include/linux/x.h\tx86_64\tbuilt\tCONFIG_A\ttrue\tCONFIG_A\ninclude/linux/y.h\tx86_64\tbuilt\tCONFIG_A\ttrue\tCONFIG_A\n"; code != 0 || out != want {
		t.Errorf("cond = %q, %d; want %q", out, code, want)
	}
	out, code = capture(t, "--tree", dir, "--build", "x86_64", "diff", hash)
	if want := hash + "\tCONFIG_A\n  include/linux/x.h\tbuilt\tCONFIG_A\tregion true\n"; code != 0 || out != want {
		t.Errorf("diff = %q, %d; want %q", out, code, want)
	}
}

func TestSearch(t *testing.T) {
	dir, fix := newKernel(t)
	base := git(t, dir, "rev-parse", fix+"^")
	// After the fix: a line of b.c (CONFIG_B, which depends on A on x86 and
	// which arm64 lacks); d.c under obj-y; a.c and d.c together; b.c deleted.
	third := commitFile(t, dir, "lib/b.c", "/* the b */\nint b = 2;\n")
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "add", name)
	}
	write("lib/Makefile", "obj-$(CONFIG_A) += a.o\nobj-$(CONFIG_B) += b.o\nobj-y += d.o\n")
	write("lib/d.c", "int d;\n")
	git(t, dir, "commit", "-q", "-m", "d")
	always := git(t, dir, "rev-parse", "HEAD")
	write("lib/a.c", "int a = 2;\n#ifdef CONFIG_B\nint ab;\n#endif\n")
	write("lib/d.c", "int d = 2;\n")
	git(t, dir, "commit", "-q", "-m", "two")
	two := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "rm", "-q", "lib/b.c")
	write("lib/Makefile", "obj-$(CONFIG_A) += a.o\nobj-y += d.o\n")
	git(t, dir, "commit", "-q", "-m", "del")
	del := git(t, dir, "rev-parse", "HEAD")
	cfg := filepath.Join(t.TempDir(), ".config")
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\nCONFIG_MODULES=y\nCONFIG_A=y\n# CONFIG_B is not set\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The deciding file of the two-file commit is a.c, or d.c for --always.
	rows := map[string]string{fix: "lib/a.c\tCONFIG_A", third: "lib/b.c\tCONFIG_B", always: "lib/d.c\ttrue", two: "lib/a.c\tCONFIG_A",
		del: "lib/b.c\tCONFIG_B", two + " always": "lib/d.c\ttrue"}
	for _, c := range []struct {
		args []string
		want []string
	}{
		// B depends on A, so a line under B is built only with A; the
		// deleted b.c is read at its commit.
		{[]string{"--where", "CONFIG_A"}, []string{fix, third, two, del}},
		{[]string{"--where", "CONFIG_A", "--strict"}, []string{fix, third, del}},
		{[]string{"--where", "B"}, []string{third, del}},
		{[]string{"--where", "!CONFIG_A"}, nil},
		{[]string{"--where", "CONFIG_A || CONFIG_B"}, []string{fix, third, two, del}},
		{[]string{"--always"}, []string{always, two + " always"}},
		{[]string{"--always", "--strict"}, []string{always}},
		{[]string{"--config", cfg}, []string{fix, always, two}},
		{[]string{"--config", cfg, "--where", "CONFIG_A"}, []string{fix, two}},
		{[]string{"--label", "generic"}, []string{fix, always, two}},
		{[]string{"--label", "x86"}, []string{third, del}},
		{[]string{"--arch", "arm64"}, []string{fix, always, two}},
		{[]string{"--arch", "x86", "--where", "CONFIG_B"}, []string{third, del}},
	} {
		args := append([]string{"--tree", dir, "search", base + "..HEAD"}, c.args...)
		var want string
		for _, h := range c.want {
			want += strings.TrimSuffix(h, " always") + "\t" + rows[h] + "\n"
		}
		if out, code := capture(t, args...); code != 0 || out != want {
			t.Errorf("search %v = %q, %d; want %q", c.args, out, code, want)
		}
	}
	// The two-file commit alone is judged where its Makefiles are read, at
	// itself, and -q tells whether anything matched.
	if out, code := capture(t, "--tree", dir, "search", two, "--where", "CONFIG_A"); code != 0 || out != two+"\tlib/a.c\tCONFIG_A\n" {
		t.Errorf("search of a hash = %q, %d", out, code)
	}
	if out, code := capture(t, "--tree", dir, "-q", "search", two, "--where", "CONFIG_A"); code != 0 || out != "" {
		t.Errorf("-q with a match = %q, %d", out, code)
	}
	if out, code := capture(t, "--tree", dir, "-q", "search", two, "--where", "!CONFIG_A"); code != 1 || out != "" {
		t.Errorf("-q without a match = %q, %d", out, code)
	}
	if out, code := capture(t, "--tree", dir, "--json", "search", two, "--always"); code != 0 ||
		!strings.Contains(out, `"path": "lib/d.c"`) || !strings.Contains(out, `"condition": "true"`) {
		t.Errorf("--json = %q, %d", out, code)
	}
	for _, args := range [][]string{
		{"search", two},
		{"search", two, "--label", "generic", "--strict"},
		{"search", two, "--arch", "mips"},
		{"search", two, "--label", "x"},
		{"search", two, "--arch", "x86", "--build", "x86_64"},
		{"search", two, "--label", "generic", "--where", "CONFIG_A", "--build", "arm64"},
		{"search", two, "--where", "CONFIG_A &&"},
		{"search", two, "--where", `"defined(FOO)"`},
		{"search", two, "--where", "CONFIG_A", "--rev", two},
		{"search", "--where", "CONFIG_A"},
	} {
		if out, code := capture(t, append([]string{"--tree", dir}, args...)...); code != 2 {
			t.Errorf("%v = %q, %d; want a usage error", args, out, code)
		}
	}
}

// A file --arch or --label finds is printed with its condition in the
// build of the arch that passed: e.c is CONFIG_ARM64 on arm64, which x86
// never builds.
func TestSearchArchCondition(t *testing.T) {
	dir, _ := newKernel(t)
	commitFile(t, dir, "lib/Makefile", "obj-$(CONFIG_A) += a.o\nobj-$(CONFIG_B) += b.o\nobj-$(CONFIG_ARM64) += e.o\n")
	h := commitFile(t, dir, "lib/e.c", "int e;\n")
	for _, f := range [][]string{{"--arch", "arm64"}, {"--label", "arm64"}} {
		args := append([]string{"--tree", dir, "search", h}, f...)
		if out, code := capture(t, args...); code != 0 || out != h+"\tlib/e.c\tCONFIG_ARM64\n" {
			t.Errorf("search %v = %q, %d", f, out, code)
		}
	}
	// A file no arch builds, B being x86's, is printed as diff reads it.
	commitFile(t, dir, "lib/Makefile", "obj-$(CONFIG_A) += a.o\nobj-$(CONFIG_B) += b.o\nifdef CONFIG_ARM64\nobj-$(CONFIG_B) += n.o\nendif\n")
	h = commitFile(t, dir, "lib/n.c", "int n;\n")
	if out, code := capture(t, "--tree", dir, "search", h, "--label", "none"); code != 0 || out != h+"\tlib/n.c\tfalse\n" {
		t.Errorf("search --label none = %q, %d", out, code)
	}
}

// --always reads a file's changed lines together, as diff does: lines
// under CONFIG_A and !CONFIG_A are built in every configuration, though
// neither is alone.
func TestSearchAlwaysBranches(t *testing.T) {
	dir, _ := newKernel(t)
	commitFile(t, dir, "lib/Makefile", "obj-$(CONFIG_A) += a.o\nobj-$(CONFIG_B) += b.o\nobj-y += d.o\n")
	commitFile(t, dir, "lib/d.c", "#ifdef CONFIG_A\nint d;\n#else\nint e;\n#endif\n")
	h := commitFile(t, dir, "lib/d.c", "#ifdef CONFIG_A\nint d = 1;\n#else\nint e = 1;\n#endif\n")
	if out, code := capture(t, "--tree", dir, "--build", "x86_64", "diff", h); code != 0 || !strings.HasPrefix(out, h+"\ttrue\n") {
		t.Errorf("diff = %q, %d", out, code)
	}
	for _, f := range [][]string{{"--always"}, {"--always", "--strict"}} {
		args := append([]string{"--tree", dir, "search", h}, f...)
		if out, code := capture(t, args...); code != 0 || out != h+"\tlib/d.c\ttrue\n" {
			t.Errorf("search %v = %q, %d", f, out, code)
		}
	}
}

// A header no listed source includes is judged by the regions of its
// changed lines, and --where reads =y on a bool as the build does.
func TestSearchHeader(t *testing.T) {
	dir, _ := newKernel(t)
	h := commitFile(t, dir, "include/linux/x.h", "int x(void);\n#ifdef CONFIG_A\nint xa(void);\n#endif\n")
	for _, where := range []string{"CONFIG_A", "A=y"} {
		if out, code := capture(t, "--tree", dir, "search", h, "--where", where); code != 0 || out != h+"\tinclude/linux/x.h\tCONFIG_A\n" {
			t.Errorf("search --where %s = %q, %d", where, out, code)
		}
	}
	if out, code := capture(t, "--tree", dir, "diff", h, "--where", "CONFIG_A"); code != 2 {
		t.Errorf("diff --where = %q, %d; want a usage error", out, code)
	}
}

// Next to a source, a header counts only through its changed lines under
// a CONFIG_ condition: the options of the sources decide the change, and a
// common header's own lines, read wherever its includers are, do not.
func TestDiffHeaderNextToSource(t *testing.T) {
	dir, _ := newKernel(t)
	commitFile(t, dir, "lib/a.c", "#include <linux/x.h>\nint a = 1;\n")
	commitFile(t, dir, "include/linux/x.h", "int x(void);\n#ifdef CONFIG_MODULES\nint xm(void);\n#endif\n")
	both := func(b, x string) string {
		for name, content := range map[string]string{"lib/b.c": b, "include/linux/x.h": x} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "add", name)
		}
		git(t, dir, "commit", "-q", "-m", "b and x")
		return git(t, dir, "rev-parse", "HEAD")
	}
	common := both("/* b */\nint b = 2;\n", "int x(int);\n#ifdef CONFIG_MODULES\nint xm(void);\n#endif\n")
	guarded := both("/* b */\nint b = 3;\n", "int x(int);\n#ifdef CONFIG_MODULES\nint xm(int);\n#endif\n")
	out, code := capture(t, "--tree", dir, "diff", common, guarded)
	want := common + "\tx86\tCONFIG_B\n" +
		"  include/linux/x.h\tbuilt\tCONFIG_A\tregion true\tarches x86,arm64\n" +
		"  lib/b.c\tbuilt\tCONFIG_B\tregion true\tarches x86\n" +
		guarded + "\tx86\t(CONFIG_A && CONFIG_MODULES) || CONFIG_B\n" +
		"  include/linux/x.h\tbuilt\tCONFIG_A\tregion CONFIG_MODULES\tarches x86\n" +
		"  lib/b.c\tbuilt\tCONFIG_B\tregion true\tarches x86\n"
	if code != 0 || out != want {
		t.Errorf("diff = %q, %d; want %q", out, code, want)
	}
	cfg := filepath.Join(t.TempDir(), ".config")
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\nCONFIG_A=y\n# CONFIG_B is not set\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := capture(t, "--tree", dir, "--config", cfg, "search", common, guarded); code != 0 || out != "" {
		t.Errorf("search --config = %q, %d; want no match: the header's lines do not count", out, code)
	}
	if out, code := capture(t, "--tree", dir, "search", common, "--where", "CONFIG_A"); code != 0 || out != common+"\tlib/b.c\tCONFIG_B\n" {
		t.Errorf("search --where CONFIG_A = %q, %d; want lib/b.c deciding", out, code)
	}
	if out, code := capture(t, "--tree", dir, "search", guarded, "--where", "CONFIG_MODULES"); code != 0 || out != guarded+"\tinclude/linux/x.h\tCONFIG_A && CONFIG_MODULES\n" {
		t.Errorf("search --where CONFIG_MODULES = %q, %d; want the header's guarded line", out, code)
	}
}

// A header no includer resolves counts next to a source through its
// changed lines under a CONFIG_ condition, as diff's condition counts it.
func TestSearchHeaderNextToSource(t *testing.T) {
	dir, _ := newKernel(t)
	for name, content := range map[string]string{
		"lib/b.c":           "/* b */\nint b = 2;\n",
		"include/linux/x.h": "int x(void);\n#ifdef CONFIG_A\nint xa(void);\n#endif\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "add", name)
	}
	git(t, dir, "commit", "-q", "-m", "b and x")
	h := git(t, dir, "rev-parse", "HEAD")
	cfg := filepath.Join(t.TempDir(), ".config")
	if err := os.WriteFile(cfg, []byte("# Linux/x86 6.12.0 Kernel Configuration\nCONFIG_X86_64=y\nCONFIG_MODULES=y\nCONFIG_A=y\n# CONFIG_B is not set\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := capture(t, "--tree", dir, "--config", cfg, "diff", h); code != 0 || !strings.HasPrefix(out, h+"\tapplies\tCONFIG_A || CONFIG_B\n") {
		t.Fatalf("diff = %q, %d; want applies under CONFIG_A", out, code)
	}
	if out, code := capture(t, "--tree", dir, "--config", cfg, "search", h); code != 0 || out != h+"\tinclude/linux/x.h\tCONFIG_A\n" {
		t.Errorf("search --config = %q, %d; want the header's line, as diff says", out, code)
	}
	// The header's line counts against --strict too.
	if out, code := capture(t, "--tree", dir, "search", h, "--where", "CONFIG_B", "--strict"); code != 0 || out != "" {
		t.Errorf("search --where CONFIG_B --strict = %q, %d; want no match", out, code)
	}
}

// A line Kconfig never builds, as one under !CONFIG_A in a file built
// under CONFIG_B, which depends on A, is built nowhere: no arch may build
// it, and no --where holds of it.
func TestDeadLine(t *testing.T) {
	dir, _ := newKernel(t)
	h := commitFile(t, dir, "lib/b.c", "/* b */\nint b;\n#ifndef CONFIG_A\nint stub;\n#endif\n")
	out, code := capture(t, "--tree", dir, "diff", h)
	if want := h + "\tnone\t!CONFIG_A && CONFIG_B\n  lib/b.c\tbuilt\tCONFIG_B\tregion !CONFIG_A\tarches \n"; code != 0 || out != want {
		t.Errorf("diff = %q, %d; want %q", out, code, want)
	}
	for _, where := range []string{"CONFIG_A", "!CONFIG_A", "CONFIG_B"} {
		if out, code := capture(t, "--tree", dir, "search", h, "--where", where); code != 0 || out != "" {
			t.Errorf("search --where %s = %q, %d; want no match", where, out, code)
		}
	}
}

// A file the range's end lacks is judged a line at a time by the build of
// the revision that has it, and a revision without the build leaves the
// commits it decides unknown rather than failing the search.
func TestSearchWaitingFiles(t *testing.T) {
	dir, fix := newKernel(t)
	git(t, dir, "rm", "-q", "lib/b.c")
	git(t, dir, "commit", "-q", "-m", "drop b")
	gone := git(t, dir, "rev-parse", "HEAD")
	// At the range's end B no longer depends on A; when b.c went, it did.
	commitFile(t, dir, "arch/x86/Kconfig", "config X86\n\tdef_bool y\n\nconfig MODULES\n\tbool \"m\"\n\nconfig A\n\tbool \"a\"\n\nconfig B\n\ttristate \"b\"\n")
	if out, code := capture(t, "--tree", dir, "search", fix+"..HEAD", "--where", "CONFIG_A"); code != 0 || out != gone+"\tlib/b.c\tCONFIG_B\n" {
		t.Errorf("search --where CONFIG_A = %q, %d; want lib/b.c as its revision builds it", out, code)
	}
	if out, code := capture(t, "--tree", dir, "search", fix+"..HEAD", "--build", "loongarch", "--where", "CONFIG_A"); code != 0 || out != "" {
		t.Errorf("search --build loongarch = %q, %d; want nothing and no error", out, code)
	}
}
