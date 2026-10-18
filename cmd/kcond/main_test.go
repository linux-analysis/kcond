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

// diff leaves out a file whose changed lines are all comments, so that
// it does not make the condition unknown.
func TestDiffSkipsCommentOnlyFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	kernel := filepath.Join(t.TempDir(), "linux")
	git(t, t.TempDir(), "init", "-q", "-b", "master", kernel)
	write := func(files map[string]string) string {
		for name, content := range files {
			p := filepath.Join(kernel, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, kernel, "add", name)
		}
		git(t, kernel, "commit", "-q", "-m", "change")
		return git(t, kernel, "rev-parse", "HEAD")
	}
	write(map[string]string{"Makefile": "core-y := lib/\n", "arch/x86/Kconfig": "config X86\n\tdef_bool y\n\nconfig A\n\tbool \"a\"\n",
		"lib/Makefile": "obj-$(CONFIG_A) += a.o\n", "lib/a.c": "int a;\n", "lib/b.c": "/* b */\nint b;\n"})
	fix := write(map[string]string{"lib/a.c": "int a = 1;\n", "lib/b.c": "/* the b */\nint b;\n"})
	out, code := capture(t, "--tree", kernel, "--json", "diff", fix)
	var res []commitResult
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || len(res) != 1 {
		t.Fatalf("diff = %q, %d", out, code)
	}
	if r := res[0]; len(r.Files) != 1 || r.Files[0].Path != "lib/a.c" || r.Unknown || r.Condition != "CONFIG_A" {
		t.Errorf("diff = %+v", r)
	}
}
