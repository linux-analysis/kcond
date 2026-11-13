// Command kcond answers CONFIG_ questions about a kernel tree: under which
// condition each architecture build compiles a file, what a commit
// changes and under which condition, which architectures may build it,
// which commits of a range match a condition, and whether a condition
// holds for a .config.
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
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/analysis"
	"github.com/linux-analysis/kcond/builds"
	"github.com/linux-analysis/kcond/change"
	"github.com/linux-analysis/kcond/dotconfig"
	"github.com/linux-analysis/kcond/gitfs"
	"github.com/linux-analysis/kcond/kernel"
	"github.com/linux-analysis/kcond/source"
)

const usage = `usage: kcond [flags] command [args]

The kernel tree is the one the current directory is in, or --tree DIR;
it is read as checked out, or at --rev REV. Paths are relative to the
current directory, or to the tree with --tree. Conditions are written
CONFIG_A && (CONFIG_B || !CONFIG_C); CONFIG_X=y and CONFIG_X=m test one
tristate value. --config selects the build the .config is for unless
--build says otherwise.

the tree:
  cond PATH[:LINE[-LINE]]...  under which condition each build compiles the
                              file or its lines: the Kbuild condition, the
                              #if region and their AND; with --config, the
                              verdict
  arches PATH...              the architectures that may build each file
  files [DIR...]              the sources a build compiles under the
                              directories, with their Kbuild conditions;
                              with --config, only those it builds
  symbols SYMBOL...           what a build's Kconfig says about symbols and,
                              with --config, what the .config sets them to

commits:
  diff HASH|A..B...           the files a commit changes, the condition of
                              its changed lines, its arch label and, with
                              --config, the verdict; a range lists its
                              commits oldest first, a merge with only the
                              hunks that differ from every parent, with
                              the Makefiles read at its end; --build or
                              --config reads that build only, without the
                              arch label
  search HASH|A..B...         the commits whose change passes the filters,
                              oldest first: hash, the first changed file that
                              passed and the condition it passed with; the
                              Makefiles are read as diff reads them, and
                              --arch and --label read every build

filters, for search:
  --config FILE  a changed line is built by the .config: the commit applies
                 to it as diff says, but one diff calls unknown does not
                 match
  --where COND   a changed line is built only where COND holds: its
                 condition implies COND, with the Kconfig dependencies (a
                 CONFIG_BONDING line passes --where CONFIG_NET); COND is a
                 condition of CONFIG_ options, the prefix optional
  --always       a changed file is built in every configuration, its
                 lines together, as diff reads them
  --arch ARCH    ARCH may build a changed file
  --label LABEL  the change's arch label is LABEL: generic, several, other,
                 none or an arch
  --strict       --config, --where, --always and --arch hold for every
                 changed line, not some

a .config:
  eval EXPR...                whether conditions hold for the --config file
  config FILE                 what a .config says: build, version, options

exit status: 0; 1 with -q when something does not apply, a range has no
commits or a search finds nothing; 2 on an error.

flags:
`

// opts are the flags every command takes.
type opts struct {
	tree, rev, build, config string
	where, arch, label       string // search filters
	always, strict           bool
	json, version, quiet     bool
	cfg                      *dotconfig.File // the --config file, read once
	stderr                   io.Writer
}

// version is what the build stamps with -X main.version.
var version = "dev"

func main() {
	// Most of what kcond allocates dies young: this saves 5-13% of the CPU
	// time for about 40% more memory.
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(200)
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	o := opts{stderr: stderr}
	fs := flag.NewFlagSet("kcond", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // run prints a flag error
	fs.StringVar(&o.tree, "tree", "", "the kernel tree (default: the one the current directory is in)")
	fs.StringVar(&o.rev, "rev", "", "read the tree at this git revision instead of as checked out")
	fs.StringVar(&o.build, "build", "", "one build only: "+strings.Join(kernel.ConfigBuilds, ", "))
	fs.StringVar(&o.config, "config", "", "a kernel .config, plain or gzip-compressed")
	fs.BoolVar(&o.json, "json", false, "print JSON")
	fs.BoolVar(&o.version, "version", false, "print the version and exit")
	fs.BoolVar(&o.quiet, "q", false, "print nothing; exit 1 unless every condition or commit applies, or a search finds nothing")
	fs.StringVar(&o.where, "where", "", "search: a changed line is built only where this condition holds")
	fs.BoolVar(&o.always, "always", false, "search: a changed file is built in every configuration, its lines together")
	fs.StringVar(&o.arch, "arch", "", "search: this architecture may build a changed file")
	fs.StringVar(&o.label, "label", "", "search: the change has this arch label")
	fs.BoolVar(&o.strict, "strict", false, "search: every changed line passes --config, --where, --always and --arch, not some")
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
		fmt.Fprintf(stderr, "kcond: %v\nrun kcond -h for usage\n", err)
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
	if o.build != "" && !slices.Contains(kernel.ConfigBuilds, o.build) {
		fmt.Fprintf(stderr, "kcond: --build takes one of %s\n", strings.Join(kernel.ConfigBuilds, ", "))
		return 2
	}
	if o.config != "" {
		f, err := readConfig(o.config)
		if err != nil {
			fmt.Fprintln(stderr, "kcond:", err)
			return 2
		}
		if f.Partial {
			fmt.Fprintln(stderr, "kcond: the .config has no header and few options off, like a defconfig; the options it leaves out count as off")
		}
		o.cfg = &f
	}
	c, known := commands[words[0]]
	if o.quiet && (c.verdict == nil || !c.verdict(o)) {
		fmt.Fprintln(stderr, "kcond: -q needs a verdict: eval, or cond or diff with --config, or a search")
		return 2
	}
	if !c.search && (o.where != "" || o.always || o.arch != "" || o.label != "" || o.strict) {
		fmt.Fprintln(stderr, "kcond: --where, --always, --arch, --label and --strict filter a search")
		return 2
	}
	if known {
		// Ctrl-C kills the process: a crawl it interrupted would read as
		// files not built.
		err = c.run(context.Background(), o, words[1:], stdout)
	} else {
		err = fmt.Errorf("%w: unknown command %q", errUsage, words[0])
	}
	switch {
	case errors.Is(err, errNo):
		return 1
	case err != nil:
		fmt.Fprintln(stderr, "kcond:", message(err))
		return 2
	}
	return 0
}

// message is what the user reads of err: git's complaint without the
// command line, and the library's errors without a second "kcond: ".
func message(err error) string {
	var gerr *gitfs.Error
	if errors.As(err, &gerr) && gerr.Stderr != "" {
		line, _, _ := strings.Cut(gerr.Stderr, "\n")
		return strings.TrimPrefix(strings.TrimPrefix(line, "fatal: "), "error: ")
	}
	return strings.TrimPrefix(err.Error(), "kcond: ")
}

var errUsage = errors.New("usage")

// errNo is what a command returns under -q when something does not apply,
// or a search finds nothing.
var errNo = errors.New("does not apply")

// quiet is the result of -q: errNo unless every one of the n verdicts is
// applies, and when there is none: nothing applies.
func quiet(n int, verdict func(i int) string) error {
	if n == 0 {
		return errNo
	}
	for i := range n {
		if verdict(i) != change.Applies {
			return errNo
		}
	}
	return nil
}

// command is a command of the line, with what its flags may ask of it.
type command struct {
	run func(ctx context.Context, o opts, args []string, out io.Writer) error
	// verdict tells whether the command gives a verdict with the flags
	// of o, for -q; nil for never.
	verdict func(o opts) bool
	search  bool // the search filters apply
}

var commands = map[string]command{
	"cond":    {run: cond, verdict: withConfig},
	"diff":    {run: diff, verdict: withConfig},
	"search":  {run: search, verdict: func(opts) bool { return true }, search: true},
	"arches":  {run: arches},
	"files":   {run: files},
	"eval":    {run: eval, verdict: withConfig},
	"config":  {run: config},
	"symbols": {run: symbols},
}

// withConfig tells whether the flags give a .config.
func withConfig(o opts) bool { return o.cfg != nil }

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

// buildName is the build --build names, or else the one the --config file
// is for; "" for neither.
func buildName(o opts) string {
	if o.build == "" && o.cfg != nil {
		return o.cfg.Build
	}
	return o.build
}

// each calls f with each build of the tree, or only the one buildName
// gives; lookups are the files f looks up.
func each(fsys fs.FS, o opts, lookups []string, f func(builds.Build) error) error {
	s := builds.NewSession(fsys, lookups)
	build := buildName(o)
	if build == "" {
		return s.Builds(func(b builds.Build) error {
			warn(o, b)
			return f(b)
		})
	}
	b, ok := s.Build(build)
	if !ok {
		return fmt.Errorf("the tree has no %s build", build)
	}
	warn(o, b)
	return f(b)
}

// note says msg on stderr.
func (o opts) note(msg string) {
	fmt.Fprintln(o.stderr, "kcond:", msg)
}

// warn says on stderr why a build's Makefiles or Kconfig could not be read,
// which leaves its files unknown.
func warn(o opts, b builds.Build) {
	if b.Err != nil {
		fmt.Fprintf(o.stderr, "kcond: the %s build: %v\n", b.Name, b.Err)
	}
}

func print(out io.Writer, o opts, v any, text func()) error {
	if !o.json {
		text()
		return nil
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false) // conditions hold && and <, which Go escapes for HTML by default
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func condText(e *kcond.Expr) string {
	if e == nil {
		return ""
	}
	return e.String()
}

// query is a file, or some of its lines, to ask about.
type query struct {
	path     string
	from, to int // 1-based, inclusive; 0 for the whole file
}

// parseQuery reads PATH[:LINE[-LINE]]; no kernel path has a colon.
func parseQuery(arg string) (query, error) {
	p, lines, ok := strings.Cut(arg, ":")
	if !ok {
		return query{path: arg}, nil
	}
	first, last, ok := strings.Cut(lines, "-")
	from, err := strconv.Atoi(first)
	to := from
	if err == nil && ok {
		to, err = strconv.Atoi(last)
	}
	if err != nil || from < 1 || to < from {
		return query{}, fmt.Errorf("%w: %s: lines are LINE or LINE-LINE, from 1", errUsage, arg)
	}
	return query{path: p, from: from, to: to}, nil
}

// lines writes the line range of q, "" for the whole file.
func (q query) lines() string {
	switch {
	case q.from == 0:
		return ""
	case q.from == q.to:
		return strconv.Itoa(q.from)
	}
	return fmt.Sprintf("%d-%d", q.from, q.to)
}

// region is the condition of the lines of q in fsys: the OR of the
// preprocessor and guard conditions of each; true for the whole file.
func region(fsys fs.FS, q query) (*kcond.Expr, error) {
	if q.from == 0 {
		return kcond.Const(true), nil
	}
	data, err := fs.ReadFile(fsys, q.path)
	if err != nil {
		return nil, err
	}
	src, err := source.Analyze(data, kernel.CodeOf(q.path))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", q.path, err)
	}
	if q.to > len(src.Text) {
		return nil, fmt.Errorf("%s:%s: the file has %d lines", q.path, q.lines(), len(src.Text))
	}
	var terms []*kcond.Expr
	for i := q.from - 1; i < q.to; i++ {
		terms = append(terms, src.Cond(i))
	}
	return kcond.Or(terms...), nil
}

// fileCond is the condition of one file, or of some of its lines, in one
// build.
type fileCond struct {
	Path      string `json:"path"`
	Lines     string `json:"lines,omitempty"`
	Build     string `json:"build"`
	Status    string `json:"status"`
	Kbuild    string `json:"kbuild,omitempty"`
	Region    string `json:"region"`
	Condition string `json:"condition,omitempty"`
	Verdict   string `json:"verdict,omitempty"`
}

func cond(ctx context.Context, o opts, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: cond takes paths, with lines as PATH:LINE or PATH:LINE-LINE", errUsage)
	}
	queries := make([]query, len(args))
	for i, a := range args {
		q, err := parseQuery(a)
		if err != nil {
			return err
		}
		if q.path, err = treePath(o, q.path); err != nil {
			return err
		}
		if _, ok := kernel.FileOf(q.path); !ok {
			return fmt.Errorf("%w: %s is not a kernel source or header", errUsage, q.path)
		}
		queries[i] = q
	}
	fsys, close, err := open(ctx, o)
	if err != nil {
		return err
	}
	defer close()
	regions := make([]*kcond.Expr, len(queries))
	for i, q := range queries {
		if err := isFile(fsys, q.path); err != nil {
			return err
		}
		if regions[i], err = region(fsys, q); err != nil {
			return err
		}
	}
	rows := []fileCond{}
	paths := make([]string, len(queries))
	for i, q := range queries {
		paths[i] = q.path
	}
	err = each(fsys, o, paths, func(b builds.Build) error {
		for i, q := range queries {
			f, _ := kernel.FileOf(q.path)
			fc := b.Eval(f, regions[i])
			e := builds.Term(fc)
			r := fileCond{Path: q.path, Lines: q.lines(), Build: b.Name, Status: fc.Status.String(), Kbuild: condText(fc.Kbuild),
				Region: fc.Region.String(), Condition: e.String()}
			if o.cfg != nil {
				r.Verdict = change.VerdictName(e.Eval(o.cfg.Config))
			}
			rows = append(rows, r)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if o.quiet {
		return quiet(len(rows), func(i int) string { return rows[i].Verdict })
	}
	return print(out, o, rows, func() {
		for _, r := range rows {
			p := r.Path
			if r.Lines != "" {
				p += ":" + r.Lines
			}
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s", p, r.Build, r.Status, r.Kbuild, r.Region, r.Condition)
			if r.Verdict != "" {
				fmt.Fprintf(out, "\t%s", r.Verdict)
			}
			fmt.Fprintln(out)
		}
	})
}

func diff(ctx context.Context, o opts, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: diff takes commit hashes or ranges A..B", errUsage)
	}
	if o.rev != "" {
		return fmt.Errorf("%w: diff reads commits; --rev does not apply", errUsage)
	}
	dir, _, err := treeDir(o)
	if err != nil {
		return err
	}
	build := buildName(o)
	var cfg kcond.Config
	if o.cfg != nil {
		cfg = o.cfg.Config
	}
	results, err := analysis.Diff(ctx, dir, args, analysis.Options{Build: build, Config: cfg, Warn: o.note})
	if err != nil {
		return err
	}
	if o.quiet {
		return quiet(len(results), func(i int) string { return results[i].Verdict })
	}
	return print(out, o, results, func() {
		// The commit line carries the summary and the file rows follow
		// indented, so grep -v '^ ' is the one-line view.
		for _, r := range results {
			line := r.Commit
			if r.Verdict != "" {
				line += "\t" + r.Verdict
			}
			if build == "" {
				line += "\t" + r.Label
			}
			fmt.Fprintf(out, "%s\t%s\n", line, r.Condition)
			for _, c := range r.Files {
				fmt.Fprintf(out, "  %s\t%s\t%s\tregion %s", c.Path, c.Status, c.Kbuild, c.Region)
				if build == "" {
					fmt.Fprintf(out, "\tarches %s", strings.Join(c.Arches, ","))
				}
				fmt.Fprintln(out)
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
		if _, ok := kernel.FileOf(paths[i]); !ok {
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
	fas := make([]builds.FileArches, len(paths))
	for i, p := range paths {
		f, _ := kernel.FileOf(p)
		fas[i] = builds.FileArches{File: f, Always: []string{}}
	}
	err = builds.NewSession(fsys, paths).Builds(func(b builds.Build) error {
		warn(o, b)
		for i := range fas {
			fas[i].Add(b, b.Eval(fas[i].File, kcond.Const(true)))
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

func eval(_ context.Context, o opts, exprs []string, out io.Writer) error {
	if o.cfg == nil || len(exprs) == 0 {
		return fmt.Errorf("%w: eval takes --config FILE and conditions", errUsage)
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
		rows = append(rows, result{e.String(), change.VerdictName(e.Eval(o.cfg.Config))})
	}
	if o.quiet {
		return quiet(len(rows), func(i int) string { return rows[i].Verdict })
	}
	return print(out, o, rows, func() {
		for _, r := range rows {
			fmt.Fprintf(out, "%s\t%s\n", r.Condition, r.Verdict)
		}
	})
}

func config(_ context.Context, o opts, args []string, out io.Writer) error {
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

// symbolInfo is what a build's Kconfig says about a symbol and, with a
// config, what the config sets it to.
type symbolInfo struct {
	Symbol   string   `json:"symbol"`
	Build    string   `json:"build"`
	Known    bool     `json:"known"` // false when the Kconfig has no bool or tristate of the name
	Values   string   `json:"values"`
	Modular  bool     `json:"modular"`
	Requires []string `json:"requires"`
	Config   string   `json:"config,omitempty"` // y, m or n in the --config file
	Off      []string `json:"off,omitempty"`    // the requirements the config leaves off
}

// value is what cfg sets a symbol to: y, m or n.
func value(cfg kcond.Config, name string) string {
	if v, ok := cfg[name]; ok {
		return string(v)
	}
	return "n"
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
	rows := []symbolInfo{}
	err = each(fsys, o, nil, func(b builds.Build) error {
		if b.Values == nil {
			return fmt.Errorf("the %s Kconfig could not be read", b.Name)
		}
		for _, n := range names {
			n = "CONFIG_" + strings.TrimPrefix(n, "CONFIG_")
			modular, known := b.Values.Modular(n)
			r := symbolInfo{Symbol: n, Build: b.Name, Known: known, Values: b.Values.Of(n).String(), Modular: modular, Requires: b.Values.Requires(n)}
			if o.cfg != nil && known {
				r.Config = value(o.cfg.Config, n)
				for _, req := range r.Requires {
					if value(o.cfg.Config, req) == "n" {
						r.Off = append(r.Off, req)
					}
				}
			}
			rows = append(rows, r)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return print(out, o, rows, func() {
		for _, r := range rows {
			if !r.Known {
				fmt.Fprintf(out, "%s\t%s\tunknown: no bool or tristate symbol of this name\n", r.Symbol, r.Build)
				continue
			}
			fmt.Fprintf(out, "%s\t%s\tvalues %s\tmodular %v", r.Symbol, r.Build, r.Values, r.Modular)
			reqs := r.Requires
			if r.Config != "" {
				// With a config, each requirement shows its value there.
				fmt.Fprintf(out, "\tconfig %s", r.Config)
				reqs = slices.Clone(reqs)
				for i, req := range reqs {
					reqs[i] = req + "=" + value(o.cfg.Config, req)
				}
			}
			fmt.Fprintf(out, "\trequires %s\n", strings.Join(reqs, ","))
		}
	})
}

// oneBuild is the build files reads: buildName's, or x86-64.
func oneBuild(fsys fs.FS, o opts) (builds.Build, error) {
	name := buildName(o)
	if name == "" {
		name = kernel.X86_64Build
	}
	b, ok := builds.NewSession(fsys, nil).Build(name)
	if !ok {
		return builds.Build{}, fmt.Errorf("the tree has no %s build", name)
	}
	if b.Kbuild == nil {
		return builds.Build{}, fmt.Errorf("the Makefiles of the %s build could not be read: %w", name, b.Err)
	}
	return b, nil
}

func files(ctx context.Context, o opts, dirs []string, out io.Writer) error {
	fsys, close, err := open(ctx, o)
	if err != nil {
		return err
	}
	defer close()
	b, err := oneBuild(fsys, o)
	if err != nil {
		return err
	}
	// Without directories, the current one: all of the tree at its top.
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	for i, d := range dirs {
		if dirs[i], err = treePath(o, d); err != nil {
			return err
		}
		if _, err := fs.Stat(fsys, dirs[i]); err != nil {
			return fmt.Errorf("%s: no such file or directory in the tree", dirs[i])
		}
		if f, _ := kernel.FileOf(dirs[i]); f.Header {
			o.note(dirs[i] + " is a header: files lists sources")
		}
	}
	under := func(p string) bool {
		for _, d := range dirs {
			if d == "." || p == d || strings.HasPrefix(p, d+"/") {
				return true
			}
		}
		return false
	}
	rows := []fileCond{}
	for _, p := range slices.Sorted(maps.Keys(b.Kbuild.Files)) {
		if !under(p) {
			continue
		}
		e := b.Plain(b.Kbuild.Files[p])
		if e.IsConst(false) {
			continue // #included only where the preprocessor drops it
		}
		r := fileCond{Path: p, Build: b.Name, Status: builds.StatusBuilt.String(), Kbuild: e.String(), Region: "true", Condition: e.String()}
		if o.cfg != nil {
			// An opaque atom leaves the file possible, and scoping an
			// analysis had better include a file than miss one.
			v := e.Eval(o.cfg.Config)
			if v == kcond.False {
				continue
			}
			r.Verdict = change.VerdictName(v)
		}
		rows = append(rows, r)
	}
	return print(out, o, rows, func() {
		for _, r := range rows {
			fmt.Fprintf(out, "%s\t%s\n", r.Path, r.Condition)
		}
	})
}

var identRe = regexp.MustCompile(`=?[A-Za-z0-9_]+`)

// parseCond reads a --where condition: CONFIG_ options, the prefix
// optional, under !, && and ||, with =y and =m.
func parseCond(s string) (*kcond.Expr, error) {
	if strings.Contains(s, `"`) {
		return nil, fmt.Errorf("%w: --where takes CONFIG_ options, not %s", errUsage, s)
	}
	s = identRe.ReplaceAllStringFunc(s, func(id string) string {
		if strings.HasPrefix(id, "=") || id == "true" || id == "false" || strings.HasPrefix(id, "CONFIG_") {
			return id
		}
		return "CONFIG_" + id
	})
	return kcond.Parse(s)
}

// search prints the commits whose change passes the filters the flags
// give (see analysis.Search).
func search(ctx context.Context, o opts, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: search takes commit hashes or ranges A..B", errUsage)
	}
	if o.rev != "" {
		return fmt.Errorf("%w: search reads commits; --rev does not apply", errUsage)
	}
	lines := o.cfg != nil || o.where != "" || o.always // the filters on changed lines
	arches := o.arch != "" || o.label != ""            // the filters on architectures
	switch {
	case !lines && !arches:
		return fmt.Errorf("%w: search needs a filter: --config, --where, --always, --arch or --label", errUsage)
	case o.strict && !lines && o.arch == "":
		return fmt.Errorf("%w: --strict needs --config, --where, --always or --arch", errUsage)
	case arches && o.build != "":
		return fmt.Errorf("%w: --arch and --label read every build", errUsage)
	case o.arch != "" && !slices.Contains(kernel.LabelArches, o.arch):
		return fmt.Errorf("%w: --arch takes one of %s", errUsage, strings.Join(kernel.LabelArches, ", "))
	case o.label != "" && !slices.Contains(kernel.LabelArches, o.label) && !slices.Contains(kernel.ArchLabels, o.label):
		return fmt.Errorf("%w: --label takes one of %s", errUsage, strings.Join(slices.Concat(kernel.ArchLabels, kernel.LabelArches), ", "))
	}
	var where *kcond.Expr
	if o.where != "" {
		var err error
		if where, err = parseCond(o.where); err != nil {
			return err
		}
	}
	dir, _, err := treeDir(o)
	if err != nil {
		return err
	}
	var cfg kcond.Config
	if o.cfg != nil {
		cfg = o.cfg.Config
	}
	fl := analysis.Filters{Config: cfg, Where: where, Always: o.always, Arch: o.arch, Label: o.label, Strict: o.strict}
	matches, err := analysis.Search(ctx, dir, args, fl, analysis.Options{Build: buildName(o), Warn: o.note})
	if err != nil {
		return err
	}
	if o.quiet {
		if len(matches) == 0 {
			return errNo
		}
		return nil
	}
	return print(out, o, matches, func() {
		for _, m := range matches {
			fmt.Fprintf(out, "%s\t%s\t%s\n", m.Commit, m.Path, m.Condition)
		}
	})
}
