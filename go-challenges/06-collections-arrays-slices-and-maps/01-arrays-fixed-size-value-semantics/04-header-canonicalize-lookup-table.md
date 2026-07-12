# Exercise 4: An ASCII Header Canonicalizer Backed by a [256]byte Lookup Table

The hot path of a parser or tokenizer often needs to translate every byte through
a fixed table — lowercase this, classify that. A `[256]byte` array indexed by the
byte value is the canonical way to do it: built once at package init, indexed with
no branches, cache-resident, and allocation-free in the loop. This exercise builds
an ASCII header-name canonicalizer on exactly that table.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
headercanon/                 independent module: example.com/headercanon
  go.mod
  canon.go                   package var lower [256]byte (init); Canonicalize(name string) string
  cmd/
    demo/
      main.go                runnable demo: canonicalize mixed-case header names
  canon_test.go              exact output table, 256 entries, idempotent, allocation benchmark
```

- Files: `canon.go`, `cmd/demo/main.go`, `canon_test.go`.
- Implement: a package-level `lower [256]byte` table built once, and `Canonicalize(name string) string` that indexes it in the hot path.
- Test: exact canonical output over mixed-case and non-letter bytes; the table has 256 entries; canonicalizing twice equals once; a benchmark documenting the allocation-free lookup.
- Verify: `go test -count=1 -race ./...`

### Why a [256]byte table, built once

A byte has 256 possible values, so a `[256]byte` array can hold the canonical form
of *every* byte with the byte itself as the index. `lower[b]` is a single indexed
load — no comparison, no branch, no bounds surprise (the index is a `byte`, always
in `[0,256)`, and indexing a `[256]byte` with it is provably in range). This is the
classic translation-table fast path: instead of `if 'A' <= b && b <= 'Z' { b += 32
}` per byte, you precompute the answer for all 256 inputs and just look it up.

The table must be built *once*, as a package-level `var`, not rebuilt on every
call. Building it per call would allocate and populate 256 bytes each time — the
exact opposite of the point. Here it is built in a small `init` function that runs
once at package load: identity for every byte, then overwrite `'A'..'Z'` with their
lowercase form. After that the table is immutable and every `Canonicalize` call
just reads it.

`Canonicalize` itself must still allocate its *output* string (strings are
immutable in Go, so producing a new one from transformed bytes requires a new
backing array). That output allocation is inherent and unavoidable. What the table
buys is that the *translation* is allocation-free and branch-free; the benchmark
below measures the pure lookup over a `[]byte` in place to show zero allocations
for the table access itself, separate from string construction.

Create `canon.go`:

```go
package headercanon

// lower maps every byte to its ASCII-lowercase form. It is a fixed-size
// translation table built once at package init and read-only thereafter.
var lower [256]byte

func init() {
	for i := 0; i < 256; i++ {
		lower[i] = byte(i)
	}
	for c := byte('A'); c <= 'Z'; c++ {
		lower[c] = c + ('a' - 'A')
	}
}

// Canonicalize returns name with every ASCII uppercase letter lowered, using the
// package-level lookup table in the hot path. Non-letter bytes pass through
// unchanged.
func Canonicalize(name string) string {
	b := []byte(name)
	for i := range b {
		b[i] = lower[b[i]]
	}
	return string(b)
}

// LowerByte exposes a single-byte lookup for benchmarks and callers that
// translate in place without building a new string.
func LowerByte(b byte) byte {
	return lower[b]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/headercanon"
)

func main() {
	names := []string{"Content-Type", "X-Request-ID", "AUTHORIZATION", "host"}
	for _, n := range names {
		fmt.Printf("%-16s -> %s\n", n, headercanon.Canonicalize(n))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Content-Type     -> content-type
X-Request-ID     -> x-request-id
AUTHORIZATION    -> authorization
host             -> host
```

### Tests

`TestCanonicalize` is a table over mixed-case names, digits, and punctuation,
asserting exact output — the hyphens, digits, and already-lowercase bytes pass
through, only `A..Z` change. `TestTableSize` asserts the table has exactly 256
entries (a compile-time fact made a runtime assertion, and a guard that the type is
`[256]byte` not something smaller). `TestIdempotent` asserts canonicalizing twice
equals once, which holds because a lowercased byte maps to itself. `BenchmarkLower`
translates a byte slice in place through `LowerByte` with `b.ReportAllocs`,
documenting the allocation-free table access.

Create `canon_test.go`:

```go
package headercanon

import (
	"testing"
)

func TestCanonicalize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"mixed", "Content-Type", "content-type"},
		{"all upper", "AUTHORIZATION", "authorization"},
		{"already lower", "host", "host"},
		{"digits and dash", "X-Request-ID-7", "x-request-id-7"},
		{"punctuation passes", "A_b.C", "a_b.c"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Canonicalize(tc.in); got != tc.want {
				t.Fatalf("Canonicalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTableSize(t *testing.T) {
	t.Parallel()

	if got := len(lower); got != 256 {
		t.Fatalf("lookup table has %d entries, want 256", got)
	}
}

func TestIdempotent(t *testing.T) {
	t.Parallel()

	in := "X-Forwarded-For"
	once := Canonicalize(in)
	twice := Canonicalize(once)
	if once != twice {
		t.Fatalf("canonicalize not idempotent: once=%q twice=%q", once, twice)
	}
}

func BenchmarkLower(b *testing.B) {
	buf := []byte("Content-Type-Header-Name-With-Mixed-Case")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		for i := range buf {
			buf[i] = LowerByte(buf[i])
		}
	}
}
```

## Review

The canonicalizer is correct when its output matches a per-byte ASCII lowercasing
for every input class — letters lowered, everything else untouched — and when the
table is built once, not per call. The `[256]byte` array is what makes the hot path
branch-free: `lower[b]` is a single load, and because the index is a `byte` it is
always in range for a 256-entry array. The trap this exercise guards against is
rebuilding the table inside `Canonicalize`; that would allocate 256 bytes per call
and defeat the entire design, so the table lives in a package `var` populated once
in `init`. Note also that this table handles ASCII only — real HTTP header
canonicalization (`textproto.CanonicalMIMEHeaderKey`) also uppercases the first
letter of each dash-separated word and is not a pure lowercasing; this exercise
isolates the lookup-table mechanic, not the full HTTP rule. Run `go test -race` and
`go test -bench=. -benchmem` to see the lookup report zero allocations per byte.

## Resources

- [Go Specification: Array types](https://go.dev/ref/spec#Array_types) — fixed-length arrays and index bounds.
- [net/textproto CanonicalMIMEHeaderKey](https://pkg.go.dev/net/textproto#CanonicalMIMEHeaderKey) — the real HTTP header canonicalization rule, for contrast.
- [Go Blog: Strings, bytes, runes and characters](https://go.dev/blog/strings) — byte vs rune and why string construction allocates.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-keymaterial-defensive-copy.md](03-keymaterial-defensive-copy.md) | Next: [05-in-place-xor-whitening-pointer.md](05-in-place-xor-whitening-pointer.md)
