package gitfs

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

var ctx = context.Background()

// fixture is a throwaway repository with two commits on master.
type fixture struct {
	dir    string
	c1, c2 string
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Alice Author", "GIT_AUTHOR_EMAIL=alice@example.com",
		"GIT_COMMITTER_NAME=Carol Committer", "GIT_COMMITTER_EMAIL=carol@example.com",
		"GIT_AUTHOR_DATE=1700000000 +0000", "GIT_COMMITTER_DATE=1700003600 +0000",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "master")
	write(t, filepath.Join(dir, "a.c"), "int a;\nint b;\n")
	git(t, dir, "add", "a.c")
	git(t, dir, "commit", "-q", "-m", "Initial commit")
	c1 := git(t, dir, "rev-parse", "HEAD")

	write(t, filepath.Join(dir, "a.c"), "int a;\n  int b;\nint c;\n")
	git(t, dir, "add", "a.c")
	git(t, dir, "commit", "-q", "-m", "net: fix leak in foo")
	c2 := git(t, dir, "rev-parse", "HEAD")
	return &fixture{dir: dir, c1: c1, c2: c2}
}

func TestDiff(t *testing.T) {
	f := newFixture(t)
	diff, err := Diff(ctx, f.dir, f.c2)
	if err != nil {
		t.Fatal(err)
	}
	// The indentation change of int b is whitespace only; int c is added.
	if !strings.Contains(diff, "+int c;") || strings.Contains(diff, "+  int b;") || strings.Contains(diff, "net: fix leak") {
		t.Errorf("Diff = %q; want the added line only, without the message", diff)
	}
	if _, err := Diff(ctx, f.dir, "no-such-rev"); err == nil {
		t.Error("Diff(no-such-rev) succeeded")
	}
	// A revision is never an option: git show would write this file.
	out := filepath.Join(t.TempDir(), "out")
	if _, err := Diff(ctx, f.dir, "--output="+out); err == nil {
		t.Error("Diff(--output=...) succeeded")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("Diff(--output=...) wrote the file")
	}
}

func TestDiffUserConfig(t *testing.T) {
	f := newFixture(t)
	// The user's color and prefix settings change nothing.
	git(t, f.dir, "config", "color.diff", "always")
	git(t, f.dir, "config", "diff.noprefix", "true")
	diff, err := Diff(ctx, f.dir, f.c2)
	if err != nil || strings.Contains(diff, "\x1b") || !strings.Contains(diff, "diff --git a/a.c b/a.c\n") {
		t.Errorf("Diff = %q, %v; want a plain diff with a/ and b/", diff, err)
	}
	// A merge reads as its change to its first parent.
	git(t, f.dir, "checkout", "-q", "-b", "side", f.c1)
	write(t, filepath.Join(f.dir, "side.c"), "int side;\n")
	git(t, f.dir, "add", "side.c")
	git(t, f.dir, "commit", "-q", "-m", "side")
	git(t, f.dir, "checkout", "-q", "master")
	git(t, f.dir, "merge", "-q", "--no-ff", "-m", "merge", "side")
	diff, err = Diff(ctx, f.dir, "HEAD")
	if err != nil || !strings.Contains(diff, "+++ b/side.c\n") || strings.Contains(diff, "a.c") {
		t.Errorf("Diff of a merge = %q, %v; want side.c only", diff, err)
	}
}

func TestDiffs(t *testing.T) {
	f := newFixture(t)
	commit := func(args ...string) string {
		git(t, f.dir, append([]string{"commit", "-q", "-m", "c"}, args...)...)
		return git(t, f.dir, "rev-parse", "HEAD")
	}
	empty := commit("--allow-empty")
	git(t, f.dir, "checkout", "-q", "-b", "side")
	write(t, filepath.Join(f.dir, "side.c"), "int side;\n")
	git(t, f.dir, "add", "side.c")
	side := commit()
	git(t, f.dir, "checkout", "-q", "master")
	git(t, f.dir, "merge", "-q", "--no-ff", "-m", "merge", "side")
	clean := git(t, f.dir, "rev-parse", "HEAD")
	git(t, f.dir, "checkout", "-q", "side")
	write(t, filepath.Join(f.dir, "side.c"), "int side = 2;\n")
	side2 := commit("-a")
	git(t, f.dir, "checkout", "-q", "master")
	git(t, f.dir, "merge", "-q", "--no-ff", "--no-commit", "side")
	write(t, filepath.Join(f.dir, "a.c"), "int evil;\n")
	evil := commit("-a")

	hashes := []string{f.c1, f.c2, empty, side, clean, side2, evil}
	var got []string
	err := Diffs(ctx, f.dir, hashes, func(i int, diff string) {
		if i != len(got) {
			t.Errorf("Diffs passed commit %d after %d", i, len(got))
		}
		got = append(got, diff)
	})
	if err != nil || len(got) != len(hashes) {
		t.Fatalf("Diffs = %d diffs, %v; want %d", len(got), err, len(hashes))
	}
	// A commit that is no merge reads as Diff reads it; a merge as its own
	// change.
	for _, i := range []int{0, 1, 2, 3, 5} {
		if want, err := Diff(ctx, f.dir, hashes[i]); err != nil || got[i] != want {
			t.Errorf("Diffs[%d] = %q; want Diff's %q, %v", i, got[i], want, err)
		}
	}
	if got[2] != "" || got[4] != "" || !strings.Contains(got[6], "diff --cc a.c\n") || strings.Contains(got[6], "side.c") {
		t.Errorf("Diffs of the empty commit, the clean merge, the merge with an edit = %q, %q, %q", got[2], got[4], got[6])
	}
	blob := git(t, f.dir, "rev-parse", "HEAD:a.c")
	if err := Diffs(ctx, f.dir, []string{f.c2, blob}, func(int, string) {}); err == nil {
		t.Error("Diffs of a blob succeeded")
	}
	if err := Diffs(ctx, f.dir, nil, func(int, string) { t.Error("Diffs(nil) passed a diff") }); err != nil {
		t.Errorf("Diffs(nil) = %v", err)
	}
}

func TestTree(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.dir, "drivers", "net", "Makefile"), "obj-$(CONFIG_FOO) += foo.o\n")
	write(t, filepath.Join(f.dir, "drivers", "net", "foo.c"), "int foo;\n")
	git(t, f.dir, "add", "drivers")
	git(t, f.dir, "commit", "-q", "-m", "Add a driver")
	head := git(t, f.dir, "rev-parse", "HEAD")

	tree, err := Open(ctx, f.dir, head)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if err := fstest.TestFS(tree, "a.c", "drivers/net/Makefile", "drivers/net/foo.c"); err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(tree, "drivers/net/Makefile")
	if err != nil || string(data) != "obj-$(CONFIG_FOO) += foo.o\n" {
		t.Errorf("ReadFile = %q, %v", data, err)
	}
	if st, err := fs.Stat(tree, "drivers"); err != nil || !st.IsDir() {
		t.Errorf("Stat(drivers) = %v, %v; want a directory", st, err)
	}
	if _, err := fs.Stat(tree, "missing.c"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(missing.c) error = %v, want ErrNotExist", err)
	}

	// The first commit's tree has no drivers directory.
	old, err := Open(ctx, f.dir, f.c1)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if _, err := fs.Stat(old, "drivers/net/foo.c"); err == nil {
		t.Error("drivers/net/foo.c exists at the first commit")
	}
}

func TestTreeGrep(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.dir, "lib", "x.c"), "#include \"y.c\"\n")
	write(t, filepath.Join(f.dir, "lib", "y.c"), "int y;\n")
	write(t, filepath.Join(f.dir, "lib", "z.S"), "# include \"w.S\"\n")
	write(t, filepath.Join(f.dir, "lib", "x.h"), "#include \"y.c\"\n")
	write(t, filepath.Join(f.dir, "lib", "bin.c"), "\x00#include \"y.c\"\n")
	git(t, f.dir, "add", "lib")
	git(t, f.dir, "commit", "-q", "-m", "Add a library")
	head := git(t, f.dir, "rev-parse", "HEAD")
	// Colors and line numbers the user asks for do not reach the listing.
	git(t, f.dir, "config", "color.ui", "always")
	git(t, f.dir, "config", "grep.lineNumber", "true")

	tree, err := Open(ctx, f.dir, head)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	// The header is not a .c or .S file and bin.c is binary.
	got, err := tree.Grep([]string{`.c"`, `.S"`}, "*.c", "*.S")
	if err != nil || !slices.Equal(got, []string{"lib/x.c", "lib/z.S"}) {
		t.Errorf("Grep = %q, %v; want lib/x.c and lib/z.S", got, err)
	}
	if got, err := tree.Grep([]string{"no such text"}, "*.c"); err != nil || len(got) != 0 {
		t.Errorf("Grep for missing text = %q, %v; want nothing", got, err)
	}
	lines, err := tree.GrepLines([]string{"include"}, "*.c", "*.h")
	if want := map[string][]string{"lib/x.c": {`#include "y.c"`}, "lib/x.h": {`#include "y.c"`}}; err != nil || !maps.EqualFunc(lines, want, slices.Equal) {
		t.Errorf("GrepLines = %q, %v; want %q", lines, err, want)
	}
	// git grep exits with 1 for an object it cannot read too.
	oid := git(t, f.dir, "rev-parse", head+":lib/z.S")
	if err := os.Remove(filepath.Join(f.dir, ".git", "objects", oid[:2], oid[2:])); err != nil {
		t.Fatal(err)
	}
	if got, err := tree.Grep([]string{"no such text"}, "*.S"); err == nil {
		t.Errorf("Grep with a missing object = %q, nil; want an error", got)
	}
}

func TestOpenBadRevision(t *testing.T) {
	f := newFixture(t)
	if _, err := Open(ctx, f.dir, "no-such-rev"); err == nil {
		t.Error("Open(no-such-rev) succeeded")
	}
}

func TestCatFile(t *testing.T) {
	f := newFixture(t)
	c, err := NewCatFile(ctx, f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	data, ok, err := c.Get(f.c2 + "^:a.c")
	if err != nil || !ok || string(data) != "int a;\nint b;\n" {
		t.Errorf("Get(c2^:a.c) = %q, %v, %v", data, ok, err)
	}
	if _, ok, err := c.Get(f.c1 + ":missing.c"); err != nil || ok {
		t.Errorf("Get(missing) = %v, %v; want not found", ok, err)
	}
	// The process survives a miss.
	if data, ok, err := c.Get(f.c1 + ":a.c"); err != nil || !ok || len(data) == 0 {
		t.Errorf("Get after a miss = %q, %v, %v", data, ok, err)
	}
}

func TestWorkTree(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.dir, "lib", "x.c"), "#include \"y.c\"\n")
	write(t, filepath.Join(f.dir, "lib", "y.c"), "int y;\n")
	git(t, f.dir, "add", "lib")
	git(t, f.dir, "commit", "-q", "-m", "Add a library")
	// The working tree is what is read, untracked files included.
	write(t, filepath.Join(f.dir, "lib", "z.S"), "# include \"w.S\"\n")
	w := WorkTree(ctx, f.dir)
	if data, err := fs.ReadFile(w, "lib/z.S"); err != nil || string(data) != "# include \"w.S\"\n" {
		t.Errorf("ReadFile = %q, %v", data, err)
	}
	g := w.(interface {
		Grep([]string, ...string) ([]string, error)
	})
	got, err := g.Grep([]string{`.c"`, `.S"`}, "*.c", "*.S")
	if err != nil || !slices.Equal(got, []string{"lib/x.c", "lib/z.S"}) {
		t.Errorf("Grep = %q, %v; want lib/x.c and lib/z.S", got, err)
	}
	if got, err := g.Grep([]string{"no such text"}, "*.c"); err != nil || len(got) != 0 {
		t.Errorf("Grep for missing text = %q, %v; want nothing", got, err)
	}
}

func TestRevList(t *testing.T) {
	f := newFixture(t)
	commit := func(name, content string) string {
		write(t, filepath.Join(f.dir, name), content)
		git(t, f.dir, "add", name)
		git(t, f.dir, "commit", "-q", "-m", name)
		return git(t, f.dir, "rev-parse", "HEAD")
	}
	base := commit("a.c", "int a;\n")
	one := commit("b.c", "int b;\n")
	two := commit("c.c", "int c;\n")
	branch := git(t, f.dir, "rev-parse", "--abbrev-ref", "HEAD")
	git(t, f.dir, "checkout", "-q", "-b", "side", base)
	side := commit("d.c", "int d;\n")
	git(t, f.dir, "checkout", "-q", branch)
	git(t, f.dir, "merge", "-q", "--no-ff", "-m", "merge", "side")
	got, err := RevList(ctx, f.dir, base+"..HEAD")
	if err != nil {
		t.Fatal(err)
	}
	// Oldest first, the merge too; two follows one on their line.
	merge := git(t, f.dir, "rev-parse", "HEAD")
	sorted, want := slices.Clone(got), []string{one, two, side, merge}
	slices.Sort(sorted)
	slices.Sort(want)
	if !slices.Equal(sorted, want) || slices.Index(got, one) > slices.Index(got, two) {
		t.Errorf("RevList = %v; want %v oldest first", got, want)
	}
	if got, err := RevList(ctx, f.dir, "HEAD..HEAD"); err != nil || len(got) != 0 {
		t.Errorf("RevList of an empty range = %v, %v", got, err)
	}
	if got, err := RevList(ctx, f.dir, "--all"); err == nil {
		t.Errorf("RevList(--all) = %v; want an error", got)
	}
}
