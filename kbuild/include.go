package kbuild

import (
	"bytes"
	"io/fs"
	"path"
	"regexp"
	"strings"

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
}

// GrepFS is a file system that lists the files containing a string faster
// than reading each of them, as a git tree does with git grep.
type GrepFS interface {
	fs.FS
	// Grep lists the text files matching one of globs that contain one of
	// strs.
	Grep(strs []string, globs ...string) ([]string, error)
}

var includeLine = regexp.MustCompile(`^\s*#\s*include\s*"([^"]+\.[cS])"`)

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
		// Unbalanced directives leave every line unconditional.
		cpp, _ := kcond.Lines(data, CodeOf(p))
		inComment := false
		for i, line := range strings.Split(string(data), "\n") {
			var code string
			code, inComment = kcond.StripComments(line, inComment)
			m := includeLine.FindStringSubmatch(code)
			if m == nil {
				continue
			}
			cond := kcond.Const(true)
			if i < len(cpp) {
				cond = cpp[i]
			}
			if target, ok := resolveInclude(fsys, path.Dir(p), m[1]); ok {
				inc.Targets[target] = append(inc.Targets[target], Include{From: p, Cond: cond})
			} else {
				inc.Unresolved = append(inc.Unresolved, Include{From: p, Cond: cond, Name: m[1]})
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
					mod.cond = kcond.And(mod.cond, rustCfg(p[:len(p)-1]))
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

// rustCfg converts the predicate of a cfg attribute. CONFIG_X holds when X
// is y or m, CONFIG_X = "y" and = "m" for one of them, as Kconfig's
// rustc_cfg defines them, and MODULE when the code is built as a module,
// as for C; not, all and any combine them. Anything else is an opaque
// atom.
func rustCfg(p string) *kcond.Expr {
	p = strings.TrimSpace(p)
	if name, args, ok := strings.Cut(p, "("); ok && strings.HasSuffix(args, ")") {
		var es []*kcond.Expr
		for _, a := range cfgArgs(args[:len(args)-1]) {
			if strings.TrimSpace(a) != "" { // a trailing comma
				es = append(es, rustCfg(a))
			}
		}
		switch strings.TrimSpace(name) {
		case "not":
			if len(es) == 1 {
				return kcond.Not(es[0])
			}
		case "all":
			return kcond.And(es...)
		case "any":
			return kcond.Or(es...)
		}
	} else if name, val, ok := strings.Cut(p, "="); kcond.IsSymbol(strings.TrimSpace(name)) {
		name = strings.TrimSpace(name)
		switch strings.TrimSpace(val) {
		case `"y"`:
			return kcond.Var(name, kcond.Builtin)
		case `"m"`:
			return kcond.Var(name, kcond.Module)
		}
		if !ok {
			return kcond.Var(name, kcond.Enabled)
		}
	} else if p == "MODULE" {
		return kcond.Var("defined(MODULE)", kcond.Enabled)
	}
	return kcond.Var("cfg("+strings.Join(strings.Fields(p), " ")+")", kcond.Enabled)
}

// cfgArgs splits the arguments of a cfg predicate at the commas outside
// parentheses and strings.
func cfgArgs(s string) []string {
	var args []string
	depth, quoted, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quoted:
			if c == '\\' {
				i++
			} else if c == '"' {
				quoted = false
			}
		case c == '"':
			quoted = true
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			args, start = append(args, s[start:i]), i+1
		}
	}
	return append(args, s[start:])
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
		for _, c := range r.compiled[in.From] {
			for _, f := range c.flags {
				if t := path.Join(f.arg, in.Name); f.kind == includeDir && isFile(fsys, t) {
					edges[t] = append(edges[t], edge{in.From, kcond.And(in.Cond, f.cond)})
				}
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
