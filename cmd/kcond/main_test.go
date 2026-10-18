package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
		{[]string{"--tree", dir, "--rev", "nonesuch", "cond", "lib/a.c"}, "kcond: Not a valid object name nonesuch\n"},
	} {
		if out, code := capture(t, c.args...); code == 0 || !strings.HasSuffix(out, c.want) {
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
	for _, want := range []string{"lib/a.c\tx86_64\tbuilt\tCONFIG_A\n", "lib/b.c\tx86_64\tbuilt\tCONFIG_B\n",
		"lib/c.c\tx86_64\tunlisted\t\n", "include/linux/x.h\tx86_64\theader\t\n"} {
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
		for _, want := range []string{"lib/a.c\tx86_64\tbuilt\tCONFIG_A\n", "lib/a.c\ti386\tbuilt\tCONFIG_A\n", "lib/a.c\tarm64\tbuilt\tCONFIG_A\n"} {
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
	if out, code := capture(t, "cond", "lib/a.c", "--build", "x86_64"); code != 0 || !strings.Contains(out, "lib/a.c\tx86_64\tbuilt\tCONFIG_A\n") {
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
	want := "lib/a.c\tx86_64\tbuilt\tCONFIG_A\n"
	for _, p := range []string{"./lib/a.c", "lib/../lib/a.c", filepath.Join(dir, "lib", "a.c")} {
		if out, code := capture(t, "--tree", dir, "--build", "x86_64", "cond", p); code != 0 || out != want {
			t.Errorf("cond %s = %q, %d; want %q", p, out, code, want)
		}
	}
	t.Chdir(filepath.Join(dir, "lib"))
	if out, code := capture(t, "--build", "x86_64", "cond", "a.c"); code != 0 || out != want {
		t.Errorf("cond a.c in lib = %q, %d; want %q", out, code, want)
	}
	for _, args := range [][]string{{"cond", "nope.c"}, {"cond", "../../outside.c"}, {"arches", "nope.c"}, {"arches", "Makefile"}} {
		if out, code := capture(t, args...); code == 0 {
			t.Errorf("%v = %q, %d; want an error", args, out, code)
		}
	}
	// A directory that is not a kernel tree is an error, not an empty answer.
	for _, args := range [][]string{{"cond", "lib/a.c"}, {"symbols", "A"}, {"arches", "lib/a.c"}} {
		if out, code := capture(t, append([]string{"--tree", t.TempDir()}, args...)...); code == 0 || !strings.Contains(out, "is not a kernel tree") {
			t.Errorf("%v outside a tree = %q, %d", args, out, code)
		}
	}
}
