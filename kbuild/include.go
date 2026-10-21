package kbuild

import (
	"bytes"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/linux-analysis/kcond"
)

// Include is an #include directive that names a .c or .S file, or a Rust
// module declaration: From includes the file on a line the preprocessor
// keeps, or under the cfg attributes that hold, when Cond holds. Name is
// the name an #include gives, when no file is found for it.
type Include struct {
	From string
	Cond *kcond.Expr
	Name string
}

// Includes are the include directives of a tree.
type Includes struct {
	// Targets maps each .c, .S and Rust module file to the directives
	// that include it.
	Targets map[string][]Include
	// Unresolved are the #include "file.c" directives whose file is
	// found neither next to the including file nor above it, for the -I
	// flags of its object to find, as a crawl knows them.
	Unresolved []Include

	// The builds of a tree share one Includes (see Options), so the
	// #include lines of the tree, which find the includers of a header,
	// are listed once.
	mu sync.Mutex
	// names lists, by the base name of an #include name, the .c, .S and
	// .h files giving it; nil until a header is looked up.
	names map[string][]string
	// read holds the #include lines of each file, by path.
	read map[string][]incLine
}

// incLine is an #include line: the name it gives, whether it is quoted
// ("name") rather than bracketed (<name>), and the condition of its line.
type incLine struct {
	name   string
	quoted bool
	cond   *kcond.Expr
}

// GrepFS is a file system that lists the files containing a string, and
// their lines with it, faster than reading each of them, as a git tree
// does with git grep.
type GrepFS interface {
	fs.FS
	// Grep lists the text files matching one of globs that contain one of
	// strs.
	Grep(strs []string, globs ...string) ([]string, error)
	// GrepLines returns, by file, the lines of the text files matching one
	// of globs that contain one of strs, in the order of the file.
	GrepLines(strs []string, globs ...string) (map[string][]string, error)
}

var (
	includeLine = regexp.MustCompile(`^\s*#\s*include\s*(["<])([^">]+)[">]`)
	guardLine   = regexp.MustCompile(`^\s*#\s*ifndef\s+([A-Za-z_][A-Za-z0-9_]*)\s*(?:/[*/].*)?$`)
)

// incLines reads the #include lines of the source data of file p, with
// the condition of each line; unbalanced directives leave every line
// unconditional.
func incLines(data []byte, p string) []incLine {
	cpp, _ := kcond.Lines(data, CodeOf(p))
	var ds []incLine
	inComment := false
	for i, line := range strings.Split(string(data), "\n") {
		var code string
		code, inComment = kcond.StripComments(line, inComment)
		if !strings.Contains(code, "include") {
			continue
		}
		m := includeLine.FindStringSubmatch(code)
		if m == nil {
			continue
		}
		cond := kcond.Const(true)
		if i < len(cpp) {
			cond = cpp[i]
		}
		ds = append(ds, incLine{strings.Clone(m[2]), m[1] == `"`, cond}) // not a substring of the file
	}
	return ds
}

// includers returns, by file, the #include lines of the .c, .S and .h
// files of fsys that name the base name of one of headers, for targets to
// resolve; the lines of the tree are listed at the first call.
func (inc *Includes) includers(fsys fs.FS, headers []string) (map[string][]incLine, error) {
	inc.mu.Lock()
	defer inc.mu.Unlock()
	if inc.names == nil {
		names, read, err := listIncludes(fsys)
		if err != nil {
			return nil, err
		}
		inc.names, inc.read = names, read
	}
	out := map[string][]incLine{}
	for _, h := range headers {
		for _, f := range inc.names[path.Base(h)] {
			out[f] = inc.read[f]
		}
	}
	return out, nil
}

// listIncludes lists the #include lines of the .c, .S and .h files of
// fsys, with the condition of each, by file, and by base name the files
// giving each #include name. With Grep the directive lines of the tree
// come from one GrepLines and are read as the file, since only directives
// decide the conditions; #define is left out, as git grep takes minutes on
// its millions of matches, so a header's guard gets its #define back after
// a leading #ifndef, and a file with a continued directive line, or a line
// leaving a comment open over lines the grep left out, is read whole.
// Without Grep every file is read.
func listIncludes(fsys fs.FS) (map[string][]string, map[string][]incLine, error) {
	names, read := map[string][]string{}, map[string][]incLine{}
	add := func(f string, ls []incLine) {
		read[f] = ls
		for _, l := range ls {
			b := path.Base(l.name)
			if n := names[b]; len(n) == 0 || n[len(n)-1] != f {
				names[b] = append(n, f)
			}
		}
	}
	if g, ok := fsys.(GrepFS); ok {
		lines, err := g.GrepLines([]string{"#if", "#el", "#endif", "#include", "# ", "#\t"}, "*.c", "*.S", "*.h")
		if err != nil {
			return nil, nil, err
		}
		for f, ls := range lines {
			f := strings.Clone(f) // the names outlive the listing
			if slices.ContainsFunc(ls, func(l string) bool {
				_, open := kcond.StripComments(l, false)
				return open || strings.HasSuffix(strings.TrimRight(l, " \t\r"), "\\") // as Lines continues it
			}) {
				data, err := fs.ReadFile(fsys, f)
				if err != nil {
					return nil, nil, err
				}
				add(f, incLines(data, f))
				continue
			}
			// The guard is the first directive; comment lines the grep
			// matched may come before it.
			if i := slices.IndexFunc(ls, func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "#") }); i >= 0 && path.Ext(f) == ".h" {
				if m := guardLine.FindStringSubmatch(ls[i]); m != nil {
					ls = slices.Insert(slices.Clone(ls), i+1, "#define "+m[1])
				}
			}
			// shortcut: a block comment opened on a line the grep left out,
			// as a bare /*, is not seen, so the directives in it count: a
			// commented-out #include adds an includer, a commented #else
			// flips a branch, an unbalanced one makes the file
			// unconditional. Four files at v7.3 comment out includes, none
			// changing a condition; read a lookup's includers whole if one
			// does.
			add(f, incLines([]byte(strings.Join(ls, "\n")), f))
		}
	} else {
		err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !slices.Contains([]string{".c", ".S", ".h"}, path.Ext(p)) {
				return err
			}
			data, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}
			add(p, incLines(data, p))
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return names, read, nil
}

// ScanIncludes finds the #include "file.c" and #include "file.S"
// directives of the .c and .S files of fsys, and the module declarations
// of its Rust files. A name is looked up next to the including file, then
// in each directory above it, as the -I$(src) and -I$(srctree) of Kbuild
// flags find it; a name the tree lacks, such as a generated table, is left
// out. Without GrepFS every source is read.
func ScanIncludes(fsys fs.FS) (*Includes, error) {
	var files, rust []string
	if g, ok := fsys.(GrepFS); ok {
		var err error
		if files, err = g.Grep([]string{`.c"`, `.S"`}, "*.c", "*.S"); err != nil {
			return nil, err
		}
		if rust, err = g.Grep([]string{"mod "}, "*.rs"); err != nil {
			return nil, err
		}
	} else {
		err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			switch path.Ext(p) {
			case ".c", ".S":
				files = append(files, p)
			case ".rs":
				rust = append(rust, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	inc := &Includes{Targets: map[string][]Include{}}
	for _, p := range files {
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		if !bytes.Contains(data, []byte(`.c"`)) && !bytes.Contains(data, []byte(`.S"`)) {
			continue // what Grep leaves out
		}
		for _, d := range incLines(data, p) {
			if ext := path.Ext(d.name); !d.quoted || (ext != ".c" && ext != ".S") {
				continue
			}
			if target, ok := resolveInclude(fsys, path.Dir(p), d.name); ok {
				inc.Targets[target] = append(inc.Targets[target], Include{From: p, Cond: d.cond})
			} else {
				inc.Unresolved = append(inc.Unresolved, Include{From: p, Cond: d.cond, Name: d.name})
			}
		}
	}
	for _, p := range rust {
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		for _, m := range rustModules(string(data)) {
			if target, ok := resolveModule(fsys, p, m); ok {
				inc.Targets[target] = append(inc.Targets[target], Include{From: p, Cond: m.cond})
			}
		}
	}
	return inc, nil
}

// rustMod is a module declaration, mod name;, with the condition of its
// cfg attributes and the file its path attribute names.
type rustMod struct {
	name, file string
	cond       *kcond.Expr
}

var modDecl = regexp.MustCompile(`^(?:pub(?:\([^)]*\))?\s+)?mod\s+([A-Za-z_][A-Za-z0-9_]*)\s*;`)

// rustModules finds the module declarations of a Rust file that name a
// file: mod name; at the start of a line, as rustfmt places module-level
// items, so that those inside an inline mod name { } block are left out.
// The outer attributes before one, on its line or the lines above, give
// its condition and file; comments, doc comments included, are skipped.
func rustModules(src string) []rustMod {
	var mods []rustMod
	var attrs []string
	pending := "" // an attribute continued on the next line
	for _, line := range strings.Split(src, "\n") {
		rest := strings.TrimRight(line, " \t\r")
		if pending != "" {
			rest = pending + " " + strings.TrimSpace(rest)
			pending = ""
		} else if t := strings.TrimSpace(rest); t == "" || strings.HasPrefix(t, "//") {
			continue
		} else if rest != t {
			attrs = nil // an item nested in another
			continue
		}
		for strings.HasPrefix(rest, "#[") {
			a, after, ok := attribute(rest)
			if !ok {
				pending = rest
				break
			}
			attrs, rest = append(attrs, a), after
		}
		if pending != "" || rest == "" {
			continue
		}
		if m := modDecl.FindStringSubmatch(rest); m != nil {
			mod := rustMod{name: m[1], cond: kcond.Const(true)}
			for _, a := range attrs {
				if p, ok := strings.CutPrefix(a, "cfg("); ok && strings.HasSuffix(p, ")") {
					mod.cond = kcond.And(mod.cond, kcond.RustCfg(p[:len(p)-1]))
				} else if v, ok := strings.CutPrefix(a, "path"); ok {
					v = strings.TrimSpace(v)
					if v, ok = strings.CutPrefix(v, "="); ok {
						mod.file = strings.Trim(strings.TrimSpace(v), `"`)
					}
				}
			}
			mods = append(mods, mod)
		}
		attrs = nil
	}
	return mods
}

// attribute splits the attribute #[...] that starts s into its content and
// the text after it; ok is false when it does not end within s.
func attribute(s string) (attr, rest string, ok bool) {
	depth, quoted := 0, false
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case quoted:
			if c == '\\' {
				i++
			} else if c == '"' {
				quoted = false
			}
		case c == '"':
			quoted = true
		case c == '[':
			depth++
		case c == ']':
			if depth--; depth == 0 {
				return strings.TrimSpace(s[2:i]), strings.TrimSpace(s[i+1:]), true
			}
		}
	}
	return "", s, false
}

// resolveModule finds the file of module declaration m in the Rust file p:
// the file of its path attribute, next to p, or name.rs or name/mod.rs in
// the directory p declares modules in. A crate root, lib.rs and mod.rs
// declare them beside themselves, any other foo.rs under foo/; since the
// crate root has any name, such as rust_binder_main.rs, a file not found
// under foo/ is looked for beside foo.rs.
func resolveModule(fsys fs.FS, p string, m rustMod) (string, bool) {
	dir := path.Dir(p)
	if m.file != "" {
		f := path.Join(dir, m.file)
		return f, isFile(fsys, f)
	}
	dirs := []string{strings.TrimSuffix(p, ".rs"), dir}
	switch path.Base(p) {
	case "lib.rs", "main.rs", "mod.rs":
		dirs = dirs[1:]
	}
	for _, d := range dirs {
		for _, f := range []string{path.Join(d, m.name+".rs"), path.Join(d, m.name, "mod.rs")} {
			if isFile(fsys, f) {
				return f, true
			}
		}
	}
	return "", false
}

// isFile reports whether p is a file of fsys.
func isFile(fsys fs.FS, p string) bool {
	if !fs.ValidPath(p) {
		return false
	}
	st, err := fs.Stat(fsys, p)
	return err == nil && !st.IsDir()
}

// resolveInclude finds the file that #include "name" in dir names.
func resolveInclude(fsys fs.FS, dir, name string) (string, bool) {
	for d := dir; ; d = path.Dir(d) {
		if p := path.Join(d, name); isFile(fsys, p) {
			return p, true
		}
		if d == "." {
			return "", false
		}
	}
}

// include adds the files the listed sources #include: such a file is
// compiled wherever a source including it is, under the condition of the
// directive's line, besides under its own Makefile condition. A name the
// scan did not find is looked up in the -I directories of the including
// source's object, under the flag's condition, and a .c or .S file an
// -include flag names is included under that flag's. Included files
// include others, so the conditions propagate as directory conditions do.
func (r *Result) include(fsys fs.FS) {
	edges := map[string][]edge{}
	for t, ins := range r.Includes.Targets {
		for _, in := range ins {
			edges[t] = append(edges[t], edge{in.From, in.Cond})
		}
	}
	for _, in := range r.Includes.Unresolved {
		for _, t := range r.includeDirs(in.From, in.Name, in.Cond) {
			if isFile(fsys, t.file) {
				edges[t.file] = append(edges[t.file], edge{in.From, t.cond})
			}
		}
	}
	for src, cs := range r.compiled {
		for _, c := range cs {
			for _, f := range c.flags {
				if ext := path.Ext(f.arg); f.kind == includedFile && (ext == ".c" || ext == ".S") && isFile(fsys, f.arg) {
					edges[f.arg] = append(edges[f.arg], edge{src, f.cond})
				}
			}
		}
	}
	r.includers = edges
	r.Files = dirConditions(r.Files, edges, false)
}
