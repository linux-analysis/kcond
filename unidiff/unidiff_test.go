package unidiff

import (
	"slices"
	"testing"
)

func TestFiles(t *testing.T) {
	diff := `diff --git a/drivers/net/foo.c b/drivers/net/foo.c
index abc1234..def5678 100644
--- a/drivers/net/foo.c
+++ b/drivers/net/foo.c
@@ -10,7 +10,8 @@ static int foo_init(void)
     int ret;
     struct foo *f;

-    f = kmalloc(sizeof(*f), GFP_KERNEL);
+    f = kzalloc(sizeof(*f), GFP_KERNEL);
+    if (!f)
+        return -ENOMEM;
     ret = foo_setup(f);
@@ -40,3 +41,2 @@
 a
--- deleted line that looks like a header
 b
diff --git a/old/name.h b/new/name.h
similarity index 90%
rename from old/name.h
rename to new/name.h
--- a/old/name.h
+++ b/new/name.h
@@ -1 +1 @@
-x
+y
\ No newline at end of file
diff --git a/gone.c b/gone.c
deleted file mode 100644
--- a/gone.c
+++ /dev/null
@@ -1,2 +0,0 @@
-a
-b
diff --git a/moved.c b/moved2.c
similarity index 100%
rename from moved.c
rename to moved2.c
diff --git a/fw.bin b/fw.bin
Binary files a/fw.bin and b/fw.bin differ
`
	files := Files(diff)
	if len(files) != 5 {
		t.Fatalf("Files returned %d files, want 5", len(files))
	}

	foo := files[0]
	if foo.OldPath != "drivers/net/foo.c" || foo.NewPath != "drivers/net/foo.c" || len(foo.Hunks) != 2 {
		t.Fatalf("foo.c = %+v", foo)
	}
	h := foo.Hunks[0]
	if h.OldStart != 10 || h.OldLines != 7 || h.NewStart != 10 || h.NewLines != 8 || h.Func != "static int foo_init(void)" {
		t.Errorf("foo.c hunk 0 header = %+v", h)
	}
	if want := []int{13}; !slices.Equal(h.Deleted, want) {
		t.Errorf("Deleted = %v, want %v", h.Deleted, want)
	}
	if want := []int{13, 14, 15}; !slices.Equal(h.Added, want) {
		t.Errorf("Added = %v, want %v", h.Added, want)
	}
	// A deleted line starting with "--" is not a file header.
	if h1 := foo.Hunks[1]; !slices.Equal(h1.Deleted, []int{41}) || h1.Func != "" {
		t.Errorf("foo.c hunk 1 = %+v", h1)
	}

	if r := files[1]; r.OldPath != "old/name.h" || r.NewPath != "new/name.h" ||
		len(r.Hunks) != 1 || r.Hunks[0].OldLines != 1 || !slices.Equal(r.Hunks[0].Deleted, []int{1}) {
		t.Errorf("rename = %+v", r)
	}
	if g := files[2]; g.OldPath != "gone.c" || g.NewPath != "" || !slices.Equal(g.Hunks[0].Deleted, []int{1, 2}) {
		t.Errorf("deletion = %+v", g)
	}
	if m := files[3]; m.OldPath != "moved.c" || m.NewPath != "moved2.c" || len(m.Hunks) != 0 {
		t.Errorf("pure rename = %+v", m)
	}
	if b := files[4]; b.OldPath != "fw.bin" || b.NewPath != "fw.bin" || len(b.Hunks) != 0 {
		t.Errorf("binary = %+v", b)
	}
}

func TestFilesCombined(t *testing.T) {
	// A merge's own hunk: the first parent's block is kept, the second
	// parent's goto replaces the first's return.
	diff := `diff --cc fs/btrfs/dev-replace.c
index dc0834f920c3,bf0b78790171..af1b898029e8
--- a/fs/btrfs/dev-replace.c
+++ b/fs/btrfs/dev-replace.c
@@@ -625,7 -624,5 +625,7 @@@ static int btrfs_dev_replace_start(stru
  	if (ret)
  		return ret;
 +	if (src_device->bdev)
 +		bdev_deny_freeze(src_device->bdev);
  	if (ret)
- 		return ret;
+ 		goto leave;
  	down_write(&dev_replace->rwsem);
diff --cc "sp ace.c"
@@@@ -1 -1 -1,2 +1,2 @@@@
   a
++ b
`
	files := Files(diff)
	if len(files) != 2 {
		t.Fatalf("Files returned %d files, want 2", len(files))
	}
	f := files[0]
	if f.OldPath != "fs/btrfs/dev-replace.c" || f.NewPath != "fs/btrfs/dev-replace.c" || len(f.Hunks) != 2 {
		t.Fatalf("dev-replace.c = %+v", f)
	}
	h0, h1 := f.Hunks[0], f.Hunks[1]
	if h0.Parent != 0 || h0.OldStart != 625 || h0.OldLines != 7 || h0.NewStart != 625 || h0.NewLines != 7 ||
		!slices.Equal(h0.Deleted, []int{630}) || !slices.Equal(h0.Added, []int{630}) {
		t.Errorf("first parent's hunk = %+v", h0)
	}
	if h1.Parent != 1 || h1.OldStart != 624 || h1.OldLines != 5 || h1.Func != "static int btrfs_dev_replace_start(stru" ||
		len(h1.Deleted) != 0 || !slices.Equal(h1.Added, []int{627, 628}) {
		t.Errorf("second parent's hunk = %+v", h1)
	}
	// An octopus merge: the line only the third parent has.
	if g := files[1]; g.NewPath != "sp ace.c" || len(g.Hunks) != 3 ||
		!slices.Equal(g.Hunks[0].Added, []int{2}) || !slices.Equal(g.Hunks[1].Added, []int{2}) || g.Hunks[2].Added != nil {
		t.Errorf("octopus = %+v", g)
	}
}

func TestFilesMalformed(t *testing.T) {
	// Garbage and truncated hunks must not panic.
	for _, diff := range []string{
		"",
		"not a diff",
		"@@ -1,2 +1,2 @@\n-a\n",
		"diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -a,b +c,d @@\n-x\n",
		"diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n-x\n",
		"diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-x\n+y\n+z\n",
		"diff --cc x\n@@@ -1 +1 @@@\n x\n",
		"diff --cc x\n@@@ -1 -1 +1 @@@\n-\n++\n+-x\n",
		"diff --cc x\n@@@\n@@ @@\n@@@ @@@\n",
	} {
		Files(diff)
	}
}
