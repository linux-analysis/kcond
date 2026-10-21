// Package gitfs reads the files of a git revision: a Tree is the file
// tree of one revision as an fs.FS with a Grep, a CatFile reads blobs by
// object name, and Diff returns the changes of one commit. It drives the
// git command line and depends on the standard library only.
package gitfs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Error is a failed git invocation.
type Error struct {
	Args   []string
	Dir    string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("git %s", strings.Join(e.Args, " "))
	if e.Dir != "" {
		msg += " (in " + e.Dir + ")"
	}
	msg += ": " + e.Err.Error()
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// command prepares git with args in dir: no prompts, the C locale, and
// SIGTERM with a grace period when ctx ends.
func command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// run executes git with args in dir and returns stdout. The error is
// ctx's when it ended, else an *Error.
func run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := command(ctx, dir, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return nil, &Error{Args: args, Dir: dir, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return out, nil
}

// checkRev refuses a revision git would read as an option; no ref or
// object name starts with a dash.
func checkRev(rev string) error {
	if strings.HasPrefix(rev, "-") {
		return fmt.Errorf("gitfs: bad revision %q", rev)
	}
	return nil
}

// Diff returns the changes of the commit hash against its first parent,
// with whitespace-only changes left out, as the change analysis reads
// them; with combined, a merge's are its own, the hunks that differ from
// every parent (git's --cc). It runs the plumbing diff-tree, whose output
// the user's color and prefix settings leave alone.
func Diff(ctx context.Context, dir, hash string, combined bool) (string, error) {
	if err := checkRev(hash); err != nil {
		return "", err
	}
	merges := "--diff-merges=first-parent"
	if combined {
		merges = "--diff-merges=dense-combined"
	}
	out, err := run(ctx, dir, "diff-tree", "-p", "--no-commit-id", "--root", merges,
		"--ignore-all-space", "--ignore-space-change", "--ignore-blank-lines", hash)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// CatFile reads objects through one git cat-file --batch process. It is
// safe for concurrent use.
type CatFile struct {
	mu  sync.Mutex
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

// NewCatFile starts git cat-file --batch in dir. Close stops it.
func NewCatFile(ctx context.Context, dir string) (*CatFile, error) {
	cmd := command(ctx, dir, "cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &CatFile{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<20)}, nil
}

// Get returns the blob named by spec: an object id or rev:path. ok is false
// when it does not exist or is not a blob.
func (c *CatFile) Get(spec string) (data []byte, ok bool, err error) {
	if strings.ContainsAny(spec, "\n") {
		return nil, false, fmt.Errorf("cat-file: bad object name %q", spec)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := io.WriteString(c.in, spec+"\n"); err != nil {
		return nil, false, err
	}
	header, err := c.out.ReadString('\n')
	if err != nil {
		return nil, false, err
	}
	fields := strings.Fields(header)
	if len(fields) == 2 && (fields[1] == "missing" || fields[1] == "ambiguous") {
		return nil, false, nil
	}
	if len(fields) != 3 {
		return nil, false, fmt.Errorf("cat-file: unexpected header %q", header)
	}
	size, err := strconv.Atoi(fields[2])
	if err != nil {
		return nil, false, fmt.Errorf("cat-file: unexpected header %q", header)
	}
	data = make([]byte, size+1) // the content is followed by a newline
	if _, err := io.ReadFull(c.out, data); err != nil {
		return nil, false, err
	}
	return data[:size], fields[1] == "blob", nil
}

// Close stops the process.
func (c *CatFile) Close() error {
	c.in.Close()
	return c.cmd.Wait()
}

// Tree is the file tree of one revision as a read-only fs.FS. The tree is
// listed once; file contents are read on demand. It is safe for
// concurrent use; Close releases it.
type Tree struct {
	blobs map[string]blob
	dirs  map[string][]fs.DirEntry
	cat   *CatFile
	// Grep runs git in dir on rev for as long as ctx, which bounds the
	// tree's cat-file reader too.
	ctx      context.Context
	dir, rev string
}

type blob struct {
	oid  string
	size int64
}

// Open lists the tree of rev in the repository at dir.
func Open(ctx context.Context, dir, rev string) (*Tree, error) {
	out, err := run(ctx, dir, "ls-tree", "-r", "-t", "-z", "-l", "--full-tree", rev)
	if err != nil {
		return nil, err
	}
	t := &Tree{blobs: map[string]blob{}, dirs: map[string][]fs.DirEntry{".": nil}, ctx: ctx, dir: dir, rev: rev}
	for rec := range bytes.SplitSeq(out, []byte{0}) {
		meta, name, ok := strings.Cut(string(rec), "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta) // mode type object size
		if len(f) != 4 {
			continue
		}
		parent := path.Dir(name)
		switch f[1] {
		case "blob":
			size, _ := strconv.ParseInt(f[3], 10, 64)
			t.blobs[name] = blob{f[2], size}
			t.dirs[parent] = append(t.dirs[parent], dirEntry{name: path.Base(name), size: size})
		case "tree":
			if _, ok := t.dirs[name]; !ok {
				t.dirs[name] = nil
			}
			t.dirs[parent] = append(t.dirs[parent], dirEntry{name: path.Base(name), dir: true})
		}
	}
	for d, ents := range t.dirs {
		slices.SortFunc(ents, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		t.dirs[d] = ents
	}
	if t.cat, err = NewCatFile(ctx, dir); err != nil {
		return nil, err
	}
	return t, nil
}

// Close stops the reader process.
func (t *Tree) Close() error { return t.cat.Close() }

// Grep lists, sorted, the text files matching one of globs that contain
// one of strs. git grep reads the files in parallel, far faster than
// reading each through ReadFile.
func (t *Tree) Grep(strs []string, globs ...string) ([]string, error) {
	return grep(t.ctx, t.dir, t.rev, false, strs, globs)
}

// GrepLines returns, by file, the lines of the text files matching one of
// globs that contain one of strs.
func (t *Tree) GrepLines(strs []string, globs ...string) (map[string][]string, error) {
	return grepLines(t.ctx, t.dir, t.rev, strs, globs)
}

func (t *Tree) pathErr(op, name string, err error) error {
	return &fs.PathError{Op: op, Path: name, Err: err}
}

// ReadFile returns the content of a file.
func (t *Tree) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, t.pathErr("read", name, fs.ErrInvalid)
	}
	b, ok := t.blobs[name]
	if !ok {
		if _, isDir := t.dirs[name]; isDir {
			return nil, t.pathErr("read", name, fs.ErrInvalid)
		}
		return nil, t.pathErr("read", name, fs.ErrNotExist)
	}
	data, ok, err := t.cat.Get(b.oid)
	if err != nil {
		return nil, t.pathErr("read", name, err)
	}
	if !ok {
		return nil, t.pathErr("read", name, fs.ErrNotExist)
	}
	return data, nil
}

// Stat describes a file or directory.
func (t *Tree) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, t.pathErr("stat", name, fs.ErrInvalid)
	}
	if _, ok := t.dirs[name]; ok {
		return dirEntry{name: path.Base(name), dir: true}, nil
	}
	if b, ok := t.blobs[name]; ok {
		return dirEntry{name: path.Base(name), size: b.size}, nil
	}
	return nil, t.pathErr("stat", name, fs.ErrNotExist)
}

// ReadDir lists a directory, sorted by name.
func (t *Tree) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, t.pathErr("readdir", name, fs.ErrInvalid)
	}
	ents, ok := t.dirs[name]
	if !ok {
		return nil, t.pathErr("readdir", name, fs.ErrNotExist)
	}
	return slices.Clone(ents), nil
}

// Open opens a file or directory.
func (t *Tree) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, t.pathErr("open", name, fs.ErrInvalid)
	}
	if ents, ok := t.dirs[name]; ok {
		return &treeDir{info: dirEntry{name: path.Base(name), dir: true}, ents: ents}, nil
	}
	data, err := t.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return &treeFile{info: dirEntry{name: path.Base(name), size: int64(len(data))}, r: bytes.NewReader(data)}, nil
}

// dirEntry is both the fs.DirEntry and the fs.FileInfo of a tree entry.
type dirEntry struct {
	name string
	dir  bool
	size int64
}

func (e dirEntry) Name() string               { return e.name }
func (e dirEntry) IsDir() bool                { return e.dir }
func (e dirEntry) Type() fs.FileMode          { return e.Mode().Type() }
func (e dirEntry) Info() (fs.FileInfo, error) { return e, nil }
func (e dirEntry) Size() int64                { return e.size }
func (e dirEntry) ModTime() time.Time         { return time.Time{} }
func (e dirEntry) Sys() any                   { return nil }
func (e dirEntry) Mode() fs.FileMode {
	if e.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

type treeFile struct {
	info dirEntry
	r    *bytes.Reader
}

func (f *treeFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *treeFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *treeFile) Close() error               { return nil }

type treeDir struct {
	info dirEntry
	ents []fs.DirEntry
	off  int
}

func (d *treeDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *treeDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: fs.ErrInvalid}
}
func (d *treeDir) Close() error { return nil }

func (d *treeDir) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.ents[d.off:]
	if n <= 0 {
		d.off = len(d.ents)
		return slices.Clone(rest), nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(rest))
	d.off += n
	return slices.Clone(rest[:n]), nil
}

// noMatch tells whether err is git grep finding nothing: exit status 1
// with nothing on stderr, as it exits 1 for an unreadable object too,
// saying so.
func noMatch(err error) bool {
	var gerr *Error
	var exit *exec.ExitError
	return errors.As(err, &gerr) && gerr.Stderr == "" && errors.As(err, &exit) && exit.ExitCode() == 1
}

// WorkTree reads the working tree of the git checkout at dir: the files
// through os.DirFS, and Grep through git grep over the tracked and
// untracked files, so that the #include scan of a checkout is as fast as
// a Tree's. It is safe for concurrent use and needs no Close.
func WorkTree(ctx context.Context, dir string) fs.FS {
	return &workTree{FS: os.DirFS(dir), ctx: ctx, dir: dir}
}

type workTree struct {
	fs.FS
	ctx context.Context
	dir string
}

// Grep lists, sorted, the text files of the working tree matching one of
// globs that contain one of strs.
func (w *workTree) Grep(strs []string, globs ...string) ([]string, error) {
	return grep(w.ctx, w.dir, "", false, strs, globs)
}

// GrepLines returns, by file, the lines of the text files of the working
// tree matching one of globs that contain one of strs.
func (w *workTree) GrepLines(strs []string, globs ...string) (map[string][]string, error) {
	return grepLines(w.ctx, w.dir, "", strs, globs)
}

// grep lists, sorted, the text files matching one of globs that contain
// one of strs: at rev, or in the working tree with its untracked files
// when rev is "". With lines, it lists the matching lines instead, in no
// order, each after its file and a NUL.
func grep(ctx context.Context, dir, rev string, lines bool, strs, globs []string) ([]string, error) {
	args := []string{"grep", "--no-color", "-z", "-I", "-F"}
	sep := []byte{0}
	if lines {
		// The user's grep.lineNumber and grep.column would add fields.
		args, sep = append(args, "--no-line-number", "--no-column"), []byte{'\n'}
	} else {
		args = append(args, "-l")
	}
	if rev == "" {
		args = append(args, "--untracked")
	}
	for _, s := range strs {
		args = append(args, "-e", s)
	}
	if rev != "" {
		args = append(args, rev)
	}
	args = append(append(args, "--"), globs...)
	out, err := run(ctx, dir, args...)
	if noMatch(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	prefix := rev + ":"
	for rec := range bytes.SplitSeq(out, sep) {
		p, ok := string(rec), true
		if rev != "" {
			p, ok = strings.CutPrefix(p, prefix)
		}
		if ok && p != "" {
			paths = append(paths, p)
		}
	}
	if !lines {
		slices.Sort(paths)
	}
	return paths, nil
}

func grepLines(ctx context.Context, dir, rev string, strs, globs []string) (map[string][]string, error) {
	recs, err := grep(ctx, dir, rev, true, strs, globs)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, rec := range recs {
		if p, line, ok := strings.Cut(rec, "\x00"); ok {
			out[p] = append(out[p], line)
		}
	}
	return out, nil
}

// RevList lists the commits of a revision range such as v6.12.1..v6.12.2
// as git rev-list does, oldest first, merges included.
func RevList(ctx context.Context, dir, spec string) ([]string, error) {
	if err := checkRev(spec); err != nil {
		return nil, err
	}
	out, err := run(ctx, dir, "rev-list", "--reverse", spec)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}
