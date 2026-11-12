package analysis

import (
	"context"
	"strings"

	"github.com/linux-analysis/kcond/gitfs"
)

// Commit is a commit to read, with the revisions its Makefiles are read
// at, in order.
type Commit struct {
	Hash string
	End  string // the end of the range it is in; "" for a commit named alone
	Revs []string
}

// Walk passes f the commits args name, in order, with their diffs: hashes,
// and the commits of each A..B range, each by its full id. A hash reads
// its Makefiles at itself, then at its parent for the files it deletes; a
// range's commits read them at the range's end first, the tree a config
// that is checked against them was built from, so that the whole range
// takes one crawl. A hash reads as its change to its first parent, and a
// range's merge as its own change: the rest of what the merge brings is
// the range's other commits. warn, when not nil, is told of a range
// without commits.
func Walk(ctx context.Context, dir string, args []string, warn func(msg string), f func(c Commit, diff string)) error {
	for _, a := range args {
		_, end, ok := strings.Cut(a, "..")
		if !ok {
			h, err := gitfs.Commit(ctx, dir, a)
			if err != nil {
				return err
			}
			d, err := gitfs.Diff(ctx, dir, h)
			if err != nil {
				return err
			}
			f(Commit{Hash: h, Revs: []string{h, h + "^"}}, d)
			continue
		}
		// A...B ends at B too, and A.. at HEAD.
		if end = strings.TrimPrefix(end, "."); end == "" {
			end = "HEAD"
		}
		hashes, err := gitfs.RevList(ctx, dir, a)
		if err != nil {
			return err
		}
		if end, err = gitfs.Commit(ctx, dir, end); err != nil {
			return err
		}
		if len(hashes) == 0 && warn != nil {
			warn(a + " has no commits")
		}
		err = gitfs.Diffs(ctx, dir, hashes, func(i int, d string) {
			h := hashes[i]
			revs := []string{end, h, h + "^"}
			if h == end {
				revs = revs[1:] // the range's last commit is its end
			}
			f(Commit{h, end, revs}, d)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
