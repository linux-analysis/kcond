// Package builds reads a kernel tree as each of its architecture builds
// does: the x86-64 and i386 builds of x86 and the builds of the other
// label arches. It gives a changed file its Kbuild condition and region
// in a build, tells which arches may or always build it, combines the
// files of a change into one condition and summarizes the arches.
package builds

import (
	"errors"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/kbuild"
	"github.com/linux-analysis/kcond/kconfig"
	"github.com/linux-analysis/kcond/kernel"
)

// KconfigOptions are the options every Kconfig analysis uses. COMPILE_TEST
// builds drivers on architectures they cannot run on, and production
// kernels never enable it, so it is off.
func KconfigOptions() kconfig.Options { return kconfig.Options{Off: []string{"COMPILE_TEST"}} }

// ArchVerdict tells whether a changed file may build on an architecture.
type ArchVerdict uint8

const (
	ArchNo ArchVerdict = iota
	ArchYes
	ArchMaybe // its condition there is unknown
)

// Build is one architecture's build of a tree; x86 has an x86-64 and an
// i386 one.
type Build struct {
	Arch   string // a label arch
	Name   string // the build's name in kernel.ConfigBuilds
	X86_64 bool   // the x86-64 build, which decides whether x86 always builds a file
	Kbuild *kbuild.Result
	Values *kconfig.Values // nil when the Kconfig could not be read
	// Defined are the CONFIG_ symbols some Kconfig file of the whole tree
	// defines (see Defined), which tells a symbol of another arch from one
	// no Kconfig has yet; nil when they could not be read.
	Defined map[string]bool
	Err     error // why Kbuild, Values or Defined is nil
}

// KconfigEnv is the environment for reading the Kconfig of an arch/
// directory; x86 is read as x86-64.
func KconfigEnv(dir string) kconfig.Env {
	arch := dir
	if dir == "x86" {
		arch = "x86_64"
	}
	return kconfig.Env{"SRCARCH": dir, "ARCH": arch}
}

// buildSpec is how one build reads a tree.
type buildSpec struct {
	arch, name string
	opts       kbuild.Options
	env        kconfig.Env
}

// buildSpecs lists the builds of the label arches the tree has: x86-64
// first, then i386, then the other arches in kernel.LabelArches order.
func buildSpecs(fsys fs.FS) []buildSpec {
	var specs []buildSpec
	for _, arch := range kernel.LabelArches {
		dir := kernel.SrcArch(fsys, arch)
		if dir == "" {
			continue
		}
		name := arch
		if arch == "x86" {
			name = kernel.X86_64Build
		}
		specs = append(specs, buildSpec{arch, name, kbuild.Options{Arch: dir}, KconfigEnv(dir)})
		if arch != "x86" {
			continue
		}
		if _, err := fs.Stat(fsys, "arch/i386/Kconfig"); err == nil {
			specs = append(specs, buildSpec{arch, "i386", kbuild.Options{Arch: "i386"}, kconfig.Env{"SRCARCH": "i386", "ARCH": "i386"}})
		} else {
			specs = append(specs, buildSpec{arch, "i386", kbuild.Options{Arch: dir, Bits: "32"}, kconfig.Env{"SRCARCH": dir, "ARCH": "i386"}})
		}
	}
	return specs
}

// specOf returns how build reads the tree; ok is false when the tree has
// no such architecture.
func specOf(fsys fs.FS, build string) (sp buildSpec, ok bool) {
	for _, sp := range buildSpecs(fsys) {
		if sp.name == build {
			return sp, true
		}
	}
	return buildSpec{}, false
}

// values analyzes the Kconfig the build reads.
func (sp buildSpec) values(fsys fs.FS) (*kconfig.Values, error) {
	tr, err := kconfig.Parse(fsys, sp.env)
	if err != nil {
		return nil, err
	}
	return kconfig.Analyze(tr, KconfigOptions()), nil
}

// Session reads the builds of one tree. The CONFIG_ symbols the tree's
// Kconfig files define are listed once, and the crawls share one scan of
// the tree's #include directives, the first crawl's. A Session is not
// safe for concurrent use.
type Session struct {
	fsys    fs.FS
	lookups []string
	inc     *kbuild.Includes

	listed     sync.Once
	defined    map[string]bool
	definedErr error
}

// NewSession reads the builds of the tree in fsys; lookups are the files
// the caller will look up (see kbuild.Options).
func NewSession(fsys fs.FS, lookups []string) *Session {
	return &Session{fsys: fsys, lookups: lookups}
}

// Build returns the build named name (one of kernel.ConfigBuilds), its
// crawl and its Kconfig analysis run at once; ok is false when the tree has
// no such architecture. A crawl, Kconfig or listing of the defined symbols
// that fails leaves its field nil and Err saying why, so that the files
// of its arch stay unknown rather than the arch absent.
func (s *Session) Build(name string) (b Build, ok bool) {
	sp, ok := specOf(s.fsys, name)
	if !ok {
		return Build{}, false
	}
	return s.build(sp), true
}

// Builds calls f with each build of the label arches the tree has, as
// Build returns it, one at a time so that only one crawl is held: the
// x86-64 build first, then i386, then the other arches in
// kernel.LabelArches order.
func (s *Session) Builds(f func(Build) error) error {
	for _, sp := range buildSpecs(s.fsys) {
		if err := f(s.build(sp)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) build(sp buildSpec) Build {
	b := Build{Arch: sp.arch, Name: sp.name, X86_64: sp.name == kernel.X86_64Build}
	sp.opts.Includes, sp.opts.Lookups = s.inc, s.lookups
	var analysis sync.WaitGroup
	var kconfigErr error
	analysis.Go(func() { b.Values, kconfigErr = sp.values(s.fsys) })
	analysis.Go(func() { s.listed.Do(func() { s.defined, s.definedErr = Defined(s.fsys) }) })
	res, crawlErr := kbuild.Crawl(s.fsys, sp.opts)
	if crawlErr == nil {
		b.Kbuild, s.inc = res, res.Includes
	}
	analysis.Wait()
	b.Defined = s.defined
	b.Err = errors.Join(crawlErr, kconfigErr, s.definedErr)
	return b
}

var configDef = regexp.MustCompile(`(?m)^\s*(?:menu)?config\s+([A-Za-z0-9_]+)`)

// Defined returns the CONFIG_ symbols some Kconfig file of the tree
// defines, for any architecture.
func Defined(fsys fs.FS) (map[string]bool, error) {
	defined := map[string]bool{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "Kconfig") {
			return err
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		for _, m := range configDef.FindAllSubmatch(data, -1) {
			defined["CONFIG_"+string(m[1])] = true
		}
		return nil
	})
	return defined, err
}

// free makes the CONFIG_ symbols of e that defined lacks opaque; nil
// defined frees nothing.
func free(e *kcond.Expr, defined map[string]bool) *kcond.Expr {
	if defined == nil {
		return e
	}
	switch e.Op {
	case kcond.OpVar:
		if kcond.IsSymbol(e.Name) && !defined[e.Name] {
			return kcond.Var("undefined:"+e.Name, kcond.Enabled)
		}
	case kcond.OpNot:
		return kcond.Not(free(e.Args[0], defined))
	case kcond.OpAnd, kcond.OpOr:
		args := make([]*kcond.Expr, len(e.Args))
		for i, a := range e.Args {
			args[i] = free(a, defined)
		}
		if e.Op == kcond.OpAnd {
			return kcond.And(args...)
		}
		return kcond.Or(args...)
	}
	return e
}

// Eval is a changed file's condition in build b, whose changed lines are
// under region: a file under another architecture's directory (see
// kernel.PathArch) is not built, a source gets what b's Makefiles say, a
// header the condition of the files that include it when they are all
// known (see kbuild.Result.Lookup) and the status header otherwise, and
// either is unlisted when b's crawl failed. The region is as b reads it
// (see Build.Region), and both are written plainly (see Build.Plain); a
// file whose condition is then false, as one listed under another arch's
// symbol, is not built.
func (b Build) Eval(f kernel.File, region *kcond.Expr) FileCond {
	fc := FileCond{File: f, region: b.Region(f.Path, region), found: kbuild.Unlisted}
	fc.Region = b.Plain(fc.region)
	if b.Kbuild != nil {
		fc.kbuild, fc.found = b.Kbuild.Lookup(f.Path)
	}
	switch pa := kernel.PathArch(f.Path); {
	case pa != "" && pa != b.Arch:
		fc.Status = StatusNotBuilt
	case b.Kbuild == nil:
		fc.Status = StatusUnlisted
	case fc.found == kbuild.Built:
		fc.Status = StatusBuilt
		if fc.Kbuild = b.Plain(fc.kbuild); fc.Kbuild.IsConst(false) {
			fc.Status, fc.Kbuild = StatusNotBuilt, nil
		}
	case fc.found == kbuild.NotBuilt:
		fc.Status = StatusNotBuilt
	default:
		fc.Status = StatusUnlisted
	}
	if f.Header && fc.Status == StatusUnlisted {
		fc.Status = StatusHeader
	}
	return fc
}

// term is the condition under which b builds the changed lines of a file,
// or nil with what b can tell without one: ArchNo when it does not build
// the file, ArchMaybe when the file's condition there is unknown. It reads
// the crawl's condition and the region before Plain, and judges a header
// whose includers are not known by its region, as Term does.
func (b Build) term(fc FileCond) (*kcond.Expr, ArchVerdict) {
	pa := kernel.PathArch(fc.Path)
	if pa != "" && pa != b.Arch {
		return nil, ArchNo
	}
	if b.Values == nil || (b.Kbuild == nil && !fc.Header) {
		return nil, ArchMaybe
	}
	if fc.Header && fc.found != kbuild.Built {
		e, _ := Term(FileCond{File: fc.File, Status: StatusHeader, Region: fc.region}, false)
		switch {
		case e == nil:
			return nil, ArchMaybe
		case e.IsConst(false):
			return nil, ArchNo
		}
		return e, ArchYes
	}
	switch fc.found {
	case kbuild.Built:
		return kcond.And(fc.kbuild, fc.region), ArchYes
	case kbuild.NotBuilt:
		if pa == b.Arch {
			// Built by rules the crawl does not follow, or only for
			// the other word size.
			return nil, ArchMaybe
		}
		return nil, ArchNo
	}
	return nil, ArchMaybe
}

// MayBuild tells whether b may build a changed file, fc as Eval gives it: its
// Kbuild condition there, ANDed with the region of its changed lines, must
// be possible under b's Kconfig. Symbols not in b.Defined become opaque
// (nil frees nothing), so one no Kconfig file defines yet, often added by
// the fix itself, neither rules the file in nor out.
func (b Build) MayBuild(fc FileCond) ArchVerdict {
	t, v := b.term(fc)
	if t == nil {
		return v
	}
	if b.Values.Possible(free(t, b.Defined)) {
		return ArchYes
	}
	return ArchNo
}

// AlwaysBuilds reports whether every configuration of b builds a changed file,
// fc as Eval gives it: a source listed under a condition that always
// holds, or a header whose changed lines are under a CONFIG_ condition
// that does.
func (b Build) AlwaysBuilds(fc FileCond) bool {
	t, _ := b.term(fc)
	return t != nil && b.Values.Always(free(t, b.Defined))
}

// FileArches gathers what the builds of a file's revision say about it.
type FileArches struct {
	kernel.File
	// Verdicts are by label arch; x86 merges its x86-64 and i386 builds.
	Verdicts map[string]ArchVerdict
	Always   []string // the label arches every configuration of which builds the file
	Present  []string // the label arches the revision has; empty until a build ran
}

// Add records what build b says about the file, fc as b.Eval gives it.
func (f *FileArches) Add(b Build, fc FileCond) {
	if f.Verdicts == nil {
		f.Verdicts = map[string]ArchVerdict{}
	}
	v := b.MayBuild(fc)
	if old, ok := f.Verdicts[b.Arch]; ok && (old == ArchYes || (old == ArchMaybe && v == ArchNo)) {
		v = old
	}
	f.Verdicts[b.Arch] = v
	// For x86 only the x86-64 build decides; the i386 one never does.
	if (b.X86_64 || b.Arch != "x86") && b.AlwaysBuilds(fc) {
		f.Always = append(f.Always, b.Arch)
	}
	if !slices.Contains(f.Present, b.Arch) {
		f.Present = append(f.Present, b.Arch)
	}
}

// Lists returns the label arches the file may build on, known or not, and
// those of them where its condition is unknown.
func (f *FileArches) Lists() (arches, unknown []string) {
	arches, unknown = []string{}, []string{}
	for _, a := range kernel.LabelArches {
		switch f.Verdicts[a] {
		case ArchMaybe:
			unknown = append(unknown, a)
			fallthrough
		case ArchYes:
			arches = append(arches, a)
		}
	}
	return arches, unknown
}

// ArchSummary is the architecture data of a change.
type ArchSummary struct {
	Arches []string // label arches that may build the changed code
	// Unknown are the Arches only files with an unknown condition give.
	Unknown []string
	// Uncertain is set when Unknown is not empty or a counted file was
	// never found at any revision.
	Uncertain bool
	// Always are the Arches every configuration of which builds the code.
	Always []string
	Label  string
}

// Summarize combines the files of a change the way Combine does: sources
// count, headers only when no source changed. The label covers the label
// arches present at the files' revisions: generic when every one but
// kernel.OptionalArch may build the code, the arch's name when only one does,
// several otherwise; other when every counted path is under an
// architecture outside the label set, none when no label arch builds it,
// and "" when no file was analysed.
func Summarize(files []FileArches) ArchSummary {
	s := ArchSummary{Arches: []string{}, Unknown: []string{}, Always: []string{}}
	sources := slices.ContainsFunc(files, func(f FileArches) bool { return !f.Header })
	yes, maybe, present, always := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	found, labelPath := false, false // labelPath: a counted path outside arch/ or under a label arch
	for _, f := range files {
		for _, a := range f.Present {
			present[a] = true
		}
		if !Labelled(f.File, sources) {
			continue
		}
		pa := kernel.PathArch(f.Path)
		labelPath = labelPath || pa == "" || slices.Contains(kernel.LabelArches, pa)
		found = found || len(f.Present) > 0
		if len(f.Present) == 0 {
			s.Uncertain = true
			continue
		}
		for a, v := range f.Verdicts {
			switch v {
			case ArchYes:
				yes[a] = true
			case ArchMaybe:
				maybe[a] = true
			}
		}
		for _, a := range f.Always {
			always[a] = true
		}
	}
	var in []string
	for _, a := range kernel.LabelArches {
		if !yes[a] && !maybe[a] {
			continue
		}
		s.Arches = append(s.Arches, a)
		if !yes[a] {
			s.Unknown = append(s.Unknown, a)
		}
		if always[a] {
			s.Always = append(s.Always, a)
		}
		if present[a] {
			in = append(in, a)
		}
	}
	s.Uncertain = s.Uncertain || len(s.Unknown) > 0
	switch {
	case !found:
	case len(in) == 0 && !labelPath:
		s.Label = "other"
	case len(in) == 0:
		s.Label = "none"
	case !slices.ContainsFunc(kernel.LabelArches, func(a string) bool { return present[a] && a != kernel.OptionalArch && !yes[a] && !maybe[a] }):
		s.Label = "generic"
	case len(in) == 1:
		s.Label = in[0]
	default:
		s.Label = "several"
	}
	return s
}
