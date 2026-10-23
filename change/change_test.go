package change

import "testing"

type blobs map[string]string

func (b blobs) Get(spec string) ([]byte, bool, error) {
	s, ok := b[spec]
	return []byte(s), ok, nil
}

const fooOld = `#include <linux/foo.h>

static int foo_setup(void)
{
	int ret = 0;
#ifdef CONFIG_FOO_DEBUG
	pr_debug("setup\n");
#endif
	if (IS_ENABLED(CONFIG_FOO_EXTRA))
		ret = foo_extra();
	return ret;
}
`

const fooNew = `#include <linux/foo.h>

static int foo_setup(void)
{
	int ret = 0;
#ifdef CONFIG_FOO_DEBUG
	pr_debug("setup %d\n", ret);
#endif
	if (IS_ENABLED(CONFIG_FOO_EXTRA))
		ret = foo_extra_checked();
	return ret;
}
`

const diff = `diff --git a/drivers/foo/foo.c b/drivers/foo/foo.c
--- a/drivers/foo/foo.c
+++ b/drivers/foo/foo.c
@@ -6,6 +6,6 @@ static int foo_setup(void)
 #ifdef CONFIG_FOO_DEBUG
-	pr_debug("setup\n");
+	pr_debug("setup %d\n", ret);
 #endif
 	if (IS_ENABLED(CONFIG_FOO_EXTRA))
-		ret = foo_extra();
+		ret = foo_extra_checked();
 	return ret;
diff --git a/drivers/foo/comment.c b/drivers/foo/comment.c
--- a/drivers/foo/comment.c
+++ b/drivers/foo/comment.c
@@ -1,2 +1,3 @@
 int x;
+/* explain x */
 int y;
diff --git a/drivers/foo/moved.c b/drivers/foo/moved.c
--- a/drivers/foo/moved.c
+++ b/drivers/foo/moved.c
@@ -1,2 +1,3 @@
-int a; /* a */
+/* a */
+int a;
 int b;
diff --git a/drivers/foo/reorder.c b/drivers/foo/reorder.c
--- a/drivers/foo/reorder.c
+++ b/drivers/foo/reorder.c
@@ -1,2 +1,2 @@
+int b;
 int a;
-int b;
diff --git a/Documentation/foo.rst b/Documentation/foo.rst
--- a/Documentation/foo.rst
+++ b/Documentation/foo.rst
@@ -1 +1 @@
-old
+new
diff --git a/include/linux/foo.h b/include/linux/foo.h
--- a/include/linux/foo.h
+++ b/include/linux/foo.h
@@ -1,3 +1,3 @@
 #ifdef CONFIG_FOO
-int foo(void);
+int foo(int);
 #endif
`

func TestChanges(t *testing.T) {
	b := blobs{
		"h^:drivers/foo/foo.c":     fooOld,
		"h:drivers/foo/foo.c":      fooNew,
		"h:drivers/foo/comment.c":  "int x;\n/* explain x */\nint y;\n",
		"h^:drivers/foo/moved.c":   "int a; /* a */\nint b;\n",
		"h:drivers/foo/moved.c":    "/* a */\nint a;\nint b;\n",
		"h^:drivers/foo/reorder.c": "int a;\nint b;\n",
		"h:drivers/foo/reorder.c":  "int b;\nint a;\n",
		"h^:include/linux/foo.h":   "#ifdef CONFIG_FOO\nint foo(void);\n#endif\n",
		"h:include/linux/foo.h":    "#ifdef CONFIG_FOO\nint foo(int);\n#endif\n",
	}
	got := Changes(b, "h", diff)
	if len(got) != 5 {
		t.Fatalf("Changes = %+v, want foo.c, comment.c, moved.c, reorder.c and foo.h", got)
	}
	foo := got[0]
	if foo.Path != "drivers/foo/foo.c" || foo.Header || !foo.Code ||
		foo.Region.String() != "CONFIG_FOO_DEBUG=y || CONFIG_FOO_EXTRA" ||
		len(foo.Functions) != 1 || foo.Functions[0] != "static int foo_setup(void)" {
		t.Errorf("foo.c = %+v (region %s)", foo, foo.Region)
	}
	// The lines' conditions, each once: the deleted lines' then the added
	// lines', which repeat them.
	if len(foo.Terms) != 2 || foo.Terms[0].String() != "CONFIG_FOO_DEBUG=y" || foo.Terms[1].String() != "CONFIG_FOO_EXTRA" {
		t.Errorf("foo.c terms = %v, want CONFIG_FOO_DEBUG=y and CONFIG_FOO_EXTRA", foo.Terms)
	}
	if c := got[1]; c.Path != "drivers/foo/comment.c" || c.Code {
		t.Errorf("comment.c = %+v, want no code change", c)
	}
	if c := got[2]; c.Path != "drivers/foo/moved.c" || c.Code || c.Region.String() != "false" || len(c.Terms) != 0 {
		t.Errorf("moved.c = %+v, want no code change", c)
	}
	// A line moved past other code is a code change.
	if c := got[3]; c.Path != "drivers/foo/reorder.c" || !c.Code || c.Region.String() != "true" {
		t.Errorf("reorder.c = %+v, want a code change", c)
	}
	if h := got[4]; h.Path != "include/linux/foo.h" || !h.Header || h.Region.String() != "CONFIG_FOO=y" {
		t.Errorf("foo.h = %+v", h)
	}
}

// A merge's combined diff reads each parent's lines in that parent: the
// second parent's line the merge drops is read in h^2, and h^, which
// loses no line, is not read.
func TestChangesMerge(t *testing.T) {
	b := blobs{
		"h^2:m.c": "#ifdef CONFIG_X\nint x;\nint x2;\n#endif\n#ifdef CONFIG_Y\n#endif\n",
		"h:m.c":   "#ifdef CONFIG_X\nint x;\n#endif\n#ifdef CONFIG_Y\nint y;\n#endif\n",
	}
	d := "diff --cc m.c\n--- a/m.c\n+++ b/m.c\n@@@ -1,5 -1,6 +1,6 @@@\n" +
		"  #ifdef CONFIG_X\n  int x;\n -int x2;\n  #endif\n  #ifdef CONFIG_Y\n++int y;\n  #endif\n"
	got := Changes(b, "h", d)
	if len(got) != 1 || !got[0].Code || got[0].Region.String() != "CONFIG_X=y || CONFIG_Y=y" {
		t.Errorf("Changes = %+v, want m.c in CONFIG_X=y || CONFIG_Y=y", got)
	}
}

// Arch macros follow the Kconfig symbols in a source only the kernel
// compiles; in a header, which the 32-bit vDSO of an x86-64 kernel also
// includes, the line may be compiled as i386 code there.
func TestChangesArchMacros(t *testing.T) {
	b := blobs{}
	var d string
	for _, f := range []string{"kernel/signal.c", "arch/x86/include/asm/pvclock.h"} {
		b["h^:"+f] = "#ifdef __i386__\nint a;\n#endif\n"
		b["h:"+f] = "#ifdef __i386__\nint b;\n#endif\n"
		d += "diff --git a/" + f + " b/" + f + "\n--- a/" + f + "\n+++ b/" + f +
			"\n@@ -1,3 +1,3 @@\n #ifdef __i386__\n-int a;\n+int b;\n #endif\n"
	}
	got := Changes(b, "h", d)
	if len(got) != 2 {
		t.Fatalf("Changes = %+v, want signal.c and pvclock.h", got)
	}
	if r := got[0].Region.String(); r != "CONFIG_X86_32" {
		t.Errorf("signal.c region = %s, want CONFIG_X86_32", r)
	}
	if r, want := got[1].Region.String(), `("defined(__i386__)" && CONFIG_X86_64) || CONFIG_X86_32`; r != want {
		t.Errorf("pvclock.h region = %s, want %s", r, want)
	}
}

func TestChangesMissingImage(t *testing.T) {
	// Without the images the region is unknown (true), and the change
	// counts as code.
	got := Changes(blobs{}, "h", diff)
	if got[0].Region.String() != "true" || !got[0].Code {
		t.Errorf("foo.c = %+v", got[0])
	}
}
