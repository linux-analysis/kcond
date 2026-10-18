package gitfs

import (
	"os"
	"strings"
	"testing"
)

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
