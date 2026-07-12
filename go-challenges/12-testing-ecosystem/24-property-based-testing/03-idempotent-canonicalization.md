# Exercise 3: Idempotence Property of a Path and Header Canonicalizer

Canonicalization sits on the request perimeter: before a router matches a path or
a middleware reads a header, the value is cleaned into a canonical form so that
`/a//b/../c` and `/a/c` are treated as the same route and `content-type` and
`Content-Type` as the same header. The defining property of any canonicalizer is
idempotence — `f(f(x)) == f(x)` — because a canonical form must be a fixed point.
This exercise builds a path cleaner and a header-key canonicalizer and asserts
idempotence, an output invariant, a safety property, and an oracle cross-check
against the standard library with `pgregory.net/rapid`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
canon/                      independent module: example.com/canon
  go.mod                    go 1.26, requires pgregory.net/rapid
  canon.go                  CleanPath(string) string; CanonHeaderKey(string) string
  cmd/
    demo/
      main.go               runnable demo: clean a messy path and a header name
  canon_test.go             rapid idempotence, invariant, safety, and path.Clean oracle
```

Files: `canon.go`, `cmd/demo/main.go`, `canon_test.go`.
Implement: a hand-rolled `CleanPath` (resolve `.` and `..`, collapse `//`, root-absolute, no trailing slash) and `CanonHeaderKey` (delegate to `textproto`).
Test: rapid properties — `CleanPath` is idempotent, its output satisfies the canonical predicate, it never yields a `..` that escapes the root, and it agrees with `path.Clean("/"+s)` as an oracle; `CanonHeaderKey` is idempotent.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get pgregory.net/rapid@latest
```

### Idempotence, the invariant, and a genuine oracle

`CleanPath` treats its input as a rooted path and produces the canonical form:
absolute (leading `/`), with every `.` segment dropped, every `..` resolved
against the accumulated path, every empty segment (from `//`) collapsed, and no
trailing slash except for the root itself. It is hand-rolled here — splitting on
`/` and folding segments — precisely so there is something non-trivial to test;
in production you might call `path.Clean` directly, which is exactly why
`path.Clean("/"+s)` makes an excellent independent oracle.

Four properties pin the behavior, one per pattern from the catalog:

Idempotence: `CleanPath(CleanPath(s)) == CleanPath(s)`. The output of a correct
cleaner is already canonical — no empty, `.`, or `..` segments remain — so cleaning
it again must change nothing. If this fails, the function is not producing a
canonical form and two paths the router should treat as equal could clean to
different strings.

Invariant: the output always satisfies the canonical predicate — starts with `/`,
never contains `//`, `/./`, or a `..` segment, and has no trailing slash unless it
is the bare root. This is what "canonical" means, asserted directly on the output
regardless of input.

Safety (a metamorphic-flavored security property): canonicalization never yields a
path containing `..`. A `..` that survived cleaning would let `/static/../../etc/passwd`
escape the document root — the classic path-traversal vulnerability. Because
`CleanPath` roots the path and resolves `..` against an empty prefix (dropping any
that would escape), the output can never contain `..`, so traversal is structurally
impossible. Asserting it as a property is worth far more than trusting the reading
of the loop.

Oracle: `CleanPath(s)` equals `path.Clean("/" + s)` for every input. `path.Clean`
is the trusted reference implementing the same rooted lexical cleaning, so agreeing
with it on every generated string is strong evidence the hand-rolled fold is
correct. This is a real differential test, not a circular one, because the two
implementations are genuinely independent.

`CanonHeaderKey` delegates to `textproto.CanonicalMIMEHeaderKey`, which maps
`content-type` to `Content-Type`. Its property is idempotence: the canonical form
is a fixed point, so canonicalizing an already-canonical key returns it unchanged.

Create `canon.go`:

```go
package canon

import (
	"net/textproto"
	"strings"
)

// CleanPath returns the canonical, root-absolute form of p: empty and "." segments
// dropped, ".." resolved against the accumulated path (never escaping the root),
// "//" collapsed, and no trailing slash except for the bare root "/".
func CleanPath(p string) string {
	segs := strings.Split(p, "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		switch s {
		case "", ".":
			// empty (from a leading, trailing, or doubled slash) or current dir
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, s)
		}
	}
	return "/" + strings.Join(out, "/")
}

// CanonHeaderKey returns the canonical form of an HTTP header field name, e.g.
// "content-type" becomes "Content-Type".
func CanonHeaderKey(k string) string {
	return textproto.CanonicalMIMEHeaderKey(k)
}
```

### The runnable demo

The demo cleans a deliberately messy path — doubled slashes, a `.`, a `..`, a
trailing slash — and canonicalizes a lowercase header name.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/canon"
)

func main() {
	fmt.Println(canon.CleanPath("/api//v1/./users/../items/"))
	fmt.Println(canon.CleanPath("../../etc/passwd"))
	fmt.Println(canon.CanonHeaderKey("content-type"))
	fmt.Println(canon.CanonHeaderKey("X-REQUEST-ID"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/api/v1/items
/etc/passwd
Content-Type
X-Request-Id
```

Note the second line: `../../etc/passwd` cleans to `/etc/passwd`, not to something
that climbs above the root. The leading `..` segments are dropped because there is
nothing to pop — that is the safety property in action.

### The property tests

The path generator draws from a rune set biased toward the characters that matter
here — `/`, `.`, spaces, a couple of letters, a newline — so the generator spends
its budget on adversarial paths (`.../`, `//`, `/./`, trailing dots) rather than
random Unicode where the interesting cases are rare. Each property is one
assertion from the catalog. The header generator uses `rapid.StringOf` over a
letters-and-dash rune set to produce plausible field names in mixed case.

Create `canon_test.go`:

```go
package canon

import (
	"fmt"
	"path"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// genPath biases toward the structural characters where canonicalization bugs live.
func genPath() *rapid.Generator[string] {
	runes := []rune{'/', '.', ' ', 'a', 'b', '\n'}
	return rapid.StringOf(rapid.RuneFrom(runes))
}

func TestCleanPathIdempotent(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		s := genPath().Draw(t, "path")
		once := CleanPath(s)
		twice := CleanPath(once)
		if once != twice {
			t.Fatalf("not idempotent: CleanPath(%q)=%q, again=%q", s, once, twice)
		}
	})
}

func TestCleanPathInvariant(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		s := genPath().Draw(t, "path")
		got := CleanPath(s)
		if !strings.HasPrefix(got, "/") {
			t.Fatalf("output %q is not absolute", got)
		}
		if strings.Contains(got, "//") {
			t.Fatalf("output %q contains //", got)
		}
		if got != "/" && strings.HasSuffix(got, "/") {
			t.Fatalf("output %q has a trailing slash", got)
		}
		for _, seg := range strings.Split(got, "/") {
			if seg == "." || seg == ".." {
				t.Fatalf("output %q contains a %q segment", got, seg)
			}
		}
	})
}

func TestCleanPathNoTraversal(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		s := genPath().Draw(t, "path")
		got := CleanPath(s)
		for _, seg := range strings.Split(got, "/") {
			if seg == ".." {
				t.Fatalf("output %q can escape the root", got)
			}
		}
	})
}

func TestCleanPathMatchesOracle(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		s := genPath().Draw(t, "path")
		got := CleanPath(s)
		want := path.Clean("/" + s)
		if got != want {
			t.Fatalf("CleanPath(%q)=%q, oracle path.Clean(%q)=%q", s, got, "/"+s, want)
		}
	})
}

func TestCanonHeaderKeyIdempotent(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		runes := []rune{'a', 'B', '-', 'x', 'Z', '1'}
		k := rapid.StringOf(rapid.RuneFrom(runes)).Draw(t, "key")
		once := CanonHeaderKey(k)
		if CanonHeaderKey(once) != once {
			t.Fatalf("not idempotent: key %q -> %q", k, once)
		}
	})
}

func ExampleCleanPath() {
	fmt.Println(CleanPath("/a//b/../c/"))
	// Output: /a/c
}
```

## Review

The canonicalizer is correct when its output is a fixed point (idempotence), always
matches the canonical predicate (invariant), never contains a `..` segment (safety),
and agrees with `path.Clean("/"+s)` on every input (oracle). Together these four
properties pin the behavior far more tightly than any table of example paths, and
the oracle in particular catches the subtle fold bugs — a mishandled leading `..`,
a dropped trailing segment — that a hand-written test would miss.

The mistakes to avoid are canonicalizer-specific. First, do not treat the input as
a relative path and forget to root it: an unrooted cleaner can produce a leading
`..`, reopening the traversal hole — rooting the path and resolving `..` against an
empty prefix is what makes the safety property hold. Second, do not build a
"canonical" form that is not a fixed point (for example, one that lowercases on the
first pass but not idempotently); idempotence is the definition, and violating it
means two equal values can have different canonical strings. Third, when you use an
oracle, make sure it is genuinely independent — comparing a `path.Clean` wrapper
against `path.Clean` proves nothing, whereas comparing an independent segment-fold
against `path.Clean` is a real test. Run `go test -race`; the functions are pure,
so any race would indicate accidental shared state.

## Resources

- [`path` package](https://pkg.go.dev/path) — `path.Clean` and its exact lexical-cleaning semantics, the oracle for this exercise.
- [`net/textproto`](https://pkg.go.dev/net/textproto#CanonicalMIMEHeaderKey) — `CanonicalMIMEHeaderKey`, the header-name canonical form.
- [`pgregory.net/rapid`](https://pkg.go.dev/pgregory.net/rapid) — `StringOf`, `RuneFrom`, and `Check`.

---

Back to [02-roundtrip-queryparam-codec.md](02-roundtrip-queryparam-codec.md) | Next: [04-differential-oracle-parser.md](04-differential-oracle-parser.md)
