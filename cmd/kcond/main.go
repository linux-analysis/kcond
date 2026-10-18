// Command kcond answers CONFIG_ questions about a kernel tree: under which
// condition each architecture build compiles a file, what a commit
// changes and under which condition, which architectures may build it,
// and whether a condition holds for a .config.
//
// The tree is the one the current directory is in, or --tree DIR; it is
// read as checked out, or at --rev REV. Paths are relative to the current
// directory, or to the tree with --tree. Conditions use the syntax
// CONFIG_A && (CONFIG_B || !CONFIG_C), with CONFIG_X=y and CONFIG_X=m
// testing one tristate state. Flags may come before or after the command.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/builds"
	"github.com/linux-analysis/kcond/change"
	"github.com/linux-analysis/kcond/crawl"
	"github.com/linux-analysis/kcond/dotconfig"
	"github.com/linux-analysis/kcond/gitfs"
)

const usage = `usage: kcond [flags] command [args]

commands:
  cond PATH...               the Kbuild condition of each file, per build
  diff HASH...               the files a commit changes, their regions and
                             conditions, and with --config the verdict
  arches PATH...             the architectures that may build each file
  eval EXPR...               whether conditions hold for the --config file
  config F                   what a .config says: build, version, options
  symbols SYMBOL...          what a build's Kconfig says about symbols

flags:
`

// opts are the flags every command takes.
type opts struct {
	tree, rev, build, config string
	json, version            bool
}

// version is what the build stamps with -X main.version.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	var o opts
	fs := flag.NewFlagSet("kcond", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.tree, "tree", "", "the kernel tree (default: the one the current directory is in)")
	fs.StringVar(&o.rev, "rev", "", "read the tree at this git revision instead of as checked out")
	fs.StringVar(&o.build, "build", "", "one build only: "+strings.Join(builds.ConfigBuilds, ", "))
	fs.StringVar(&o.config, "config", "", "a kernel .config, plain or gzip-compressed")
	fs.BoolVar(&o.json, "json", false, "print JSON")
	fs.BoolVar(&o.version, "version", false, "print the version and exit")
	fs.Usage = func() {} // a flag error points to -h below
	printUsage := func(w io.Writer) {
		fmt.Fprint(w, usage)
		fs.SetOutput(w)
		fs.PrintDefaults()
	}
	// Flags may follow the command and its arguments: the words after
	// each positional one are parsed again.
	err := fs.Parse(args)
	var words []string
	for err == nil && fs.NArg() > 0 {
		words = append(words, fs.Arg(0))
		err = fs.Parse(fs.Args()[1:])
	}
	switch {
	case errors.Is(err, flag.ErrHelp), err == nil && len(words) > 0 && words[0] == "help":
		printUsage(stdout)
		return 0
	case err != nil:
		fmt.Fprintln(stderr, "run kcond -h for usage")
		return 2
	}
	if o.version {
		fmt.Fprintln(stdout, "kcond", version)
		return 0
	}
	if len(words) == 0 {
		printUsage(stderr)
		return 2
	}
	if o.build != "" && !slices.Contains(builds.ConfigBuilds, o.build) {
		fmt.Fprintf(stderr, "kcond: --build takes one of %s\n", strings.Join(builds.ConfigBuilds, ", "))
		return 2
	}
	// Ctrl-C kills the process: a crawl it interrupted would read as
	// files not built.
	err = dispatch(context.Background(), o, words[0], words[1:], stdout)
	switch {
	case errors.Is(err, errUsage):
		fmt.Fprintln(stderr, "kcond:", message(err))
		return 2
	case err != nil:
		fmt.Fprintln(stderr, "kcond:", message(err))
		return 1
	}
	return 0
}

// message is what the user reads of err: git's complaint without the
// command line, and the library's errors without a second "kcond: ".
func message(err error) string {
	var gerr *gitfs.Error
	if errors.As(err, &gerr) && gerr.Stderr != "" {
		line, _, _ := strings.Cut(gerr.Stderr, "\n")
		return strings.TrimPrefix(line, "fatal: ")
	}
	return strings.TrimPrefix(err.Error(), "kcond: ")
}

var errUsage = errors.New("usage")

func dispatch(ctx context.Context, o opts, cmd string, args []string, out io.Writer) error {
	switch cmd {
	case "cond":
		return cond(ctx, o, args, out)
	case "diff":
		return diff(ctx, o, args, out)
	case "arches":
		return arches(ctx, o, args, out)
	case "eval":
		return eval(o, args, out)
	case "config":
		return config(o, args, out)
	case "symbols":
		return symbols(ctx, o, args, out)
	}
	return fmt.Errorf("%w: unknown command %q", errUsage, cmd)
}

// open reads the tree the flags name, at --rev or as checked out. close
// releases it.
func open(ctx context.Context, o opts) (fsys fs.FS, close func(), err error) {
	dir, _, err := treeDir(o)
	if err != nil {
		return nil, nil, err
	}
	close = func() {}
	if o.rev != "" {
		t, err := gitfs.Open(ctx, dir, o.rev)
		if err != nil {
			return nil, nil, err
		}
		fsys, close = t, func() { t.Close() }
		dir += " at " + o.rev
	} else if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		fsys = gitfs.WorkTree(ctx, dir)
	} else {
		fsys = os.DirFS(dir)
	}
	// Without arch/ no build is found, and every file reads as unlisted.
	if st, err := fs.Stat(fsys, "arch"); err != nil || !st.IsDir() {
		close()
		return nil, nil, fmt.Errorf("%s is not a kernel tree: it has no arch/", dir)
	}
	return fsys, close, nil
}

// treeDir is the directory of the kernel tree, --tree or the one the
// current directory is in, with the current directory's path in it: "."
// at its top and with --tree.
func treeDir(o opts) (dir, sub string, err error) {
	if o.tree != "" {
		return o.tree, ".", nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	for d := wd; ; d = filepath.Dir(d) {
		if isTree(d) {
			sub, err := filepath.Rel(d, wd)
			return d, sub, err
		}
		if d == filepath.Dir(d) {
			return "", "", fmt.Errorf("%w: no kernel tree here: run in one or give --tree DIR", errUsage)
		}
	}
}

// isTree reports whether dir is the top of a kernel tree: it has Kbuild, a
// Makefile and arch/, which arch/x86, with Kbuild and a Makefile, lacks.
func isTree(dir string) bool {
	for _, f := range []string{"Kbuild", "Makefile", "arch"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}

// treePath is the path in the tree of a path argument, which is absolute
// or relative to the current directory, or to the tree with --tree.
func treePath(o opts, arg string) (string, error) {
	dir, sub, err := treeDir(o)
	if err == nil {
		dir, err = filepath.Abs(dir)
	}
	if err != nil {
		return "", err
	}
	p := arg
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, sub, p)
	}
	p, err = filepath.Rel(dir, p)
	if err != nil || p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s is outside the tree", errUsage, arg)
	}
	return filepath.ToSlash(p), nil
}

// isFile checks that p is a file of the tree.
func isFile(fsys fs.FS, p string) error {
	if st, err := fs.Stat(fsys, p); err != nil || st.IsDir() {
		return fmt.Errorf("%s: no such file in the tree", p)
	}
	return nil
}

// each calls f with each build of the tree, or only the one --build names.
func each(fsys fs.FS, o opts, f func(builds.Build) error) error {
	if o.build == "" {
		return builds.Each(fsys, nil, f)
	}
	b, ok := builds.Named(fsys, o.build)
	if !ok {
		return fmt.Errorf("the tree has no %s build", o.build)
	}
	return f(b)
}

func print(out io.Writer, o opts, v any, text func()) error {
	if !o.json {
		text()
		return nil
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func condText(e *kcond.Expr) string {
	if e == nil {
		return ""
	}
	return e.String()
}

// fileCond is the condition of one file in one build.
type fileCond struct {
	Path   string `json:"path"`
	Build  string `json:"build"`
	Status string `json:"status"`
	Kbuild string `json:"kbuild,omitempty"`
}

func cond(ctx context.Context, o opts, paths []string, out io.Writer) error {
	if len(paths) == 0 {
		return fmt.Errorf("%w: cond takes paths", errUsage)
	}
	for i, p := range paths {
		var err error
		if paths[i], err = treePath(o, p); err != nil {
			return err
		}
	}
	fsys, close, err := open(ctx, o)
	if err != nil {
		return err
	}
	defer close()
	for _, p := range paths {
		if err := isFile(fsys, p); err != nil {
			return err
		}
	}
	var rows []fileCond
	err = each(fsys, o, func(b builds.Build) error {
		for _, p := range paths {
			_, header := change.Kernel(p)
			fc := builds.BuildCond(p, header, kcond.Const(true), b)
			rows = append(rows, fileCond{Path: p, Build: b.Name, Status: fc.Status, Kbuild: condText(fc.Kbuild)})
		}
		return nil
	})
	if err != nil {
		return err
	}
	return print(out, o, rows, func() {
		for _, r := range rows {
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", r.Path, r.Build, r.Status, r.Kbuild)
		}
	})
}

// changed is one file a commit changes, with its conditions.
type changed struct {
	Path      string            `json:"path"`
	Header    bool              `json:"header"`
	Region    string            `json:"region"`
	Functions []string          `json:"functions,omitempty"`
	Rev       string            `json:"rev,omitempty"`
	Status    string            `json:"status"`
	Kbuild    string            `json:"kbuild,omitempty"`
	Builds    map[string]string `json:"builds,omitempty"`
	Arches    []string          `json:"arches"`
	Unknown   []string          `json:"unknown_arches"`
}

// commitResult is what diff reports for one commit.
type commitResult struct {
	Commit    string    `json:"commit"`
	Files     []changed `json:"files"`
	Condition string    `json:"condition"`
	Unknown   bool      `json:"unknown"`
	Label     string    `json:"label"`
	Arches    []string  `json:"arches"`
	Verdict   string    `json:"verdict,omitempty"`
}

func diff(ctx context.Context, o opts, hashes []string, out io.Writer) error {
	if len(hashes) == 0 {
		return fmt.Errorf("%w: diff takes commit hashes", errUsage)
	}
	if o.rev != "" {
		return fmt.Errorf("%w: diff reads commits; --rev does not apply", errUsage)
	}
	dir, _, err := treeDir(o)
	if err != nil {
		return err
	}
	var cfg kcond.Config
	if o.config != "" {
		f, err := readConfig(o.config)
		if err != nil {
			return err
		}
		cfg = f.Config
	}
	cat, err := gitfs.NewCatFile(ctx, dir)
	if err != nil {
		return err
	}
	defer cat.Close()
	var results []commitResult
	for _, h := range hashes {
		d, err := gitfs.Diff(ctx, dir, h)
		if err != nil {
			return err
		}
		// A file whose changed lines are all comments decides nothing.
		changes := slices.DeleteFunc(change.Changes(cat, h, d), func(ch change.FileChange) bool { return !ch.Code })
		files := make([]*crawl.File, len(changes))
		for i, ch := range changes {
			files[i] = &crawl.File{Path: ch.Path, Header: ch.Header, Region: ch.Region, Revs: []string{h + "^", h}}
		}
		found, err := crawl.Resolve(ctx, dir, files, crawl.Options{})
		if err != nil {
			return err
		}
		r := commitResult{Commit: h}
		var conds []builds.FileCond
		var fas []builds.FileArches
		for i, ch := range changes {
			res := found[i]
			c := changed{Path: ch.Path, Header: ch.Header, Region: ch.Region.String(), Functions: ch.Functions, Rev: res.Rev,
				Status: res.Cond.Status, Kbuild: condText(res.Cond.Kbuild)}
			c.Arches, c.Unknown = res.Arches.Lists()
			for name, fc := range res.Builds {
				if c.Builds == nil {
					c.Builds = map[string]string{}
				}
				c.Builds[name] = fc.Status
				if fc.Kbuild != nil {
					c.Builds[name] += " " + fc.Kbuild.String()
				}
			}
			r.Files = append(r.Files, c)
			conds = append(conds, res.Cond)
			fas = append(fas, res.Arches)
		}
		e, unknown := builds.Combine(conds)
		r.Condition, r.Unknown = condText(e), unknown
		sum := builds.Summarize(fas)
		r.Label, r.Arches = sum.Label, sum.Arches
		if cfg != nil {
			r.Verdict = change.VerdictName(change.Verdict(e, unknown, cfg))
		}
		results = append(results, r)
	}
	return print(out, o, results, func() {
		for _, r := range results {
			fmt.Fprintf(out, "%s\n", r.Commit)
			for _, c := range r.Files {
				fmt.Fprintf(out, "  %s\t%s\t%s\tregion %s\tarches %s\n", c.Path, c.Status, c.Kbuild, c.Region, strings.Join(c.Arches, ","))
			}
			fmt.Fprintf(out, "  condition: %s", r.Condition)
			if r.Unknown {
				fmt.Fprint(out, " (unknown in part)")
			}
			fmt.Fprintf(out, "\n  label: %s\n", r.Label)
			if r.Verdict != "" {
				fmt.Fprintf(out, "  verdict: %s\n", r.Verdict)
			}
		}
	})
}

// fileArches is what the builds say about one file.
type fileArches struct {
	Path    string   `json:"path"`
	Arches  []string `json:"arches"`
	Unknown []string `json:"unknown_arches"`
	Always  []string `json:"always"`
}

func arches(ctx context.Context, o opts, paths []string, out io.Writer) error {
	if len(paths) == 0 {
		return fmt.Errorf("%w: arches takes paths", errUsage)
	}
	if o.build != "" {
		return fmt.Errorf("%w: arches reads every build", errUsage)
	}
	for i, p := range paths {
		var err error
		if paths[i], err = treePath(o, p); err != nil {
			return err
		}
		if ok, _ := change.Kernel(paths[i]); !ok {
			return fmt.Errorf("%w: %s is not a kernel source or header", errUsage, paths[i])
		}
	}
	fsys, close, err := open(ctx, o)
	if err != nil {
		return err
	}
	defer close()
	for _, p := range paths {
		if err := isFile(fsys, p); err != nil {
			return err
		}
	}
	defined, err := builds.Defined(fsys)
	if err != nil {
		return err
	}
	fas := make([]builds.FileArches, len(paths))
	for i, p := range paths {
		_, header := change.Kernel(p)
		fas[i] = builds.FileArches{Path: p, Header: header}
	}
	err = builds.Each(fsys, nil, func(b builds.Build) error {
		for i := range fas {
			fas[i].Add(b, kcond.Const(true), defined)
		}
		return nil
	})
	if err != nil {
		return err
	}
	rows := make([]fileArches, len(paths))
	for i, fa := range fas {
		rows[i] = fileArches{Path: fa.Path, Always: fa.Always}
		rows[i].Arches, rows[i].Unknown = fa.Lists()
	}
	sum := builds.Summarize(fas)
	return print(out, o, struct {
		Files []fileArches `json:"files"`
		Label string       `json:"label"`
	}{rows, sum.Label}, func() {
		for _, r := range rows {
			fmt.Fprintf(out, "%s\tarches %s\tunknown %s\talways %s\n", r.Path, strings.Join(r.Arches, ","),
				strings.Join(r.Unknown, ","), strings.Join(r.Always, ","))
		}
		fmt.Fprintf(out, "label: %s\n", sum.Label)
	})
}

func readConfig(path string) (dotconfig.File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return dotconfig.File{}, err
	}
	return dotconfig.Read(data)
}

func eval(o opts, exprs []string, out io.Writer) error {
	if o.config == "" || len(exprs) == 0 {
		return fmt.Errorf("%w: eval takes --config FILE and conditions", errUsage)
	}
	f, err := readConfig(o.config)
	if err != nil {
		return err
	}
	type result struct {
		Condition string `json:"condition"`
		Verdict   string `json:"verdict"`
	}
	var rows []result
	for _, s := range exprs {
		e, err := kcond.Parse(s)
		if err != nil {
			return err
		}
		rows = append(rows, result{e.String(), change.VerdictName(e.Eval(f.Config))})
	}
	return print(out, o, rows, func() {
		for _, r := range rows {
			fmt.Fprintf(out, "%s\t%s\n", r.Condition, r.Verdict)
		}
	})
}

func config(o opts, args []string, out io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("%w: config takes one file", errUsage)
	}
	f, err := readConfig(args[0])
	if err != nil {
		return err
	}
	return print(out, o, struct {
		Build   string `json:"build"`
		Version string `json:"version"`
		Partial bool   `json:"partial"`
		Options int    `json:"options"`
	}{f.Build, f.Version, f.Partial, len(f.Config)}, func() {
		fmt.Fprintf(out, "build: %s\nversion: %s\npartial: %v\noptions: %d\n", f.Build, f.Version, f.Partial, len(f.Config))
	})
}

// symbolInfo is what a build's Kconfig says about a symbol.
type symbolInfo struct {
	Symbol   string   `json:"symbol"`
	Build    string   `json:"build"`
	Values   string   `json:"values"`
	Modular  bool     `json:"modular"`
	Requires []string `json:"requires"`
}

func symbols(ctx context.Context, o opts, names []string, out io.Writer) error {
	if len(names) == 0 {
		return fmt.Errorf("%w: symbols takes symbol names", errUsage)
	}
	fsys, close, err := open(ctx, o)
	if err != nil {
		return err
	}
	defer close()
	var rows []symbolInfo
	err = each(fsys, o, func(b builds.Build) error {
		if b.Values == nil {
			return fmt.Errorf("the %s Kconfig could not be read", b.Name)
		}
		for _, n := range names {
			n = "CONFIG_" + strings.TrimPrefix(n, "CONFIG_")
			modular, _ := b.Values.Modular(n)
			rows = append(rows, symbolInfo{Symbol: n, Build: b.Name, Values: b.Values.Of(n).String(), Modular: modular, Requires: b.Values.Requires(n)})
		}
		return nil
	})
	if err != nil {
		return err
	}
	return print(out, o, rows, func() {
		for _, r := range rows {
			fmt.Fprintf(out, "%s\t%s\tvalues %s\tmodular %v\trequires %s\n", r.Symbol, r.Build, r.Values, r.Modular, strings.Join(r.Requires, ","))
		}
	})
}
