package kcond

import (
	"slices"
	"strings"
	"testing"
)

// checkRust compares the region of every line of a Rust source, and that
// the lines with code are those with a token.
func checkRust(t *testing.T, src string, want []string) {
	t.Helper()
	s, err := Analyze([]byte(src), Code{Rust: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Cpp) != len(want) {
		t.Fatalf("got %d lines, want %d", len(s.Cpp), len(want))
	}
	for i := range want {
		if got := s.Cpp[i].String(); got != want[i] {
			t.Errorf("line %d (%q): %q, want %q", i+1, strings.Split(src, "\n")[i], got, want[i])
		}
		if !s.Guard[i].IsConst(true) {
			t.Errorf("line %d: guard %s", i+1, s.Guard[i])
		}
	}
}

// A #[cfg] conditions its item: a fn to its closing brace, a statement to
// its semicolon, a macro call to its semicolon though it holds braces, a
// mod and the items inside it, which AND their own attributes in, and a
// bare #[cfg(not(...))] on the next item. Doc comments and other
// attributes between the cfg and the item belong to it.
func TestRustItems(t *testing.T) {
	checkRust(t, `// SPDX
use kernel::prelude::*;

#[cfg(CONFIG_A)]
#[inline]
/// Doc.
fn a() {
	let x = "}"; // a brace in a string
	x
}
#[cfg(not(CONFIG_A))]
fn a() {}
#[cfg(any(CONFIG_32BIT, CONFIG_64BIT))]
impl_safe_as!(usize as { u64 });
#[cfg(CONFIG_B = "y")]
mod b {
	#[cfg(CONFIG_C)]
	pub fn c() -> &'static str { "c" }
	pub fn d() {}
}
fn after() {}
`, []string{
		"true", "true", "true",
		"CONFIG_A", "CONFIG_A", "CONFIG_A", "CONFIG_A", "CONFIG_A", "CONFIG_A", "CONFIG_A",
		"!CONFIG_A", "!CONFIG_A",
		"CONFIG_32BIT || CONFIG_64BIT", "CONFIG_32BIT || CONFIG_64BIT",
		"CONFIG_B=y", "CONFIG_B=y", "CONFIG_B=y && CONFIG_C", "CONFIG_B=y && CONFIG_C", "CONFIG_B=y", "CONFIG_B=y",
		"true",
	})
}

// Fields, variants, arms and parameters end at their comma, and generics
// and where clauses hold commas that end nothing; a field's attribute may
// share its line.
func TestRustMembers(t *testing.T) {
	checkRust(t, `pub struct S<T, U> where T: Copy, U: Copy {
	#[cfg(CONFIG_A)]
	a: Vec<T, U>,
	b: U,
}
pub struct Dir(#[cfg(CONFIG_DEBUG_FS)] Option<Entry>);
enum E {
	#[cfg(CONFIG_B)]
	B(u32),
	C,
}
#[cfg(all(CONFIG_D, not(CONFIG_E)))]
impl<T: Foo<U>, U> S<T, U> where T: Copy, U: Copy {
	fn f(&self, #[cfg(CONFIG_F)] x: u8) {
		match self.c {
			#[cfg(CONFIG_B)]
			E::B(n) if n > 0 => 1,
			_ => 2,
		};
	}
}
`, []string{
		"true", "CONFIG_A", "CONFIG_A", "true", "true",
		"true",
		"true", "CONFIG_B", "CONFIG_B", "true", "true",
		"CONFIG_D && !CONFIG_E", "CONFIG_D && !CONFIG_E", "CONFIG_D && !CONFIG_E", "CONFIG_D && !CONFIG_E",
		"CONFIG_B && CONFIG_D && !CONFIG_E", "CONFIG_B && CONFIG_D && !CONFIG_E",
		"CONFIG_D && !CONFIG_E", "CONFIG_D && !CONFIG_E", "CONFIG_D && !CONFIG_E", "CONFIG_D && !CONFIG_E",
	})
}

// A predicate that is not a Kconfig option is an opaque atom, test never
// holds, CONFIG_X = "m" is the module mode, cfg_attr and cfg!() are not
// regions, and an inner attribute lasts to the end of its block. Raw
// strings, byte strings, lifetimes and nested comments do not confuse the
// scan.
func TestRustPredicates(t *testing.T) {
	checkRust(t, `#![cfg(CONFIG_RUST)]
#[cfg(test)]
mod tests {}
#[cfg(feature = "std")]
fn s() {}
#[cfg(CONFIG_M = "m")]
fn m() { let r = r#"}"#; let b = b"}"; let c = '}'; /* /* } */ */ }
#[cfg_attr(CONFIG_A, derive(Debug))]
struct A;
fn g() -> bool { cfg!(CONFIG_A) }
`, []string{
		"CONFIG_RUST",
		"false", "false",
		`"cfg(feature=std)" && CONFIG_RUST`, `"cfg(feature=std)" && CONFIG_RUST`,
		"CONFIG_M=m && CONFIG_RUST", "CONFIG_M=m && CONFIG_RUST",
		"CONFIG_RUST", "CONFIG_RUST", "CONFIG_RUST",
	})
}

// Rust lines with only comments or blanks are not code.
func TestRustCode(t *testing.T) {
	s, err := Analyze([]byte("/// doc\n\n#[cfg(CONFIG_A)]\nfn a() {}\n// end\n"), Code{Rust: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"", "", "# [ cfg ( CONFIG_A ) ]", "fn a ( ) { }", ""}; !slices.Equal(s.Text, want) {
		t.Errorf("Text = %q, want %q", s.Text, want)
	}
}

// A < in a match guard opens no generics, so the cfg ends with its arm; a
// quote escaped in a character literal does not end it.
func TestRustGuardComparison(t *testing.T) {
	checkRust(t, `fn f(x: u8) -> u8 {
	match x {
		#[cfg(CONFIG_A)]
		n if n < 3 => b'\'' as u8,
		_ => 2,
	}
}
`, []string{"true", "true", "CONFIG_A", "CONFIG_A", "true", "true", "true"})
}

// An attribute the file never closes leaves its lines unconditional
// rather than without a condition.
func TestRustUnclosedAttribute(t *testing.T) {
	checkRust(t, "fn a() {}\n#[cfg(CONFIG_A)\nfn b() {}\n", []string{"true", "true", "true"})
}
