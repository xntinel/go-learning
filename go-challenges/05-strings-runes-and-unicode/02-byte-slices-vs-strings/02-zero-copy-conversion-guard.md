# Exercise 2: Zero-Copy string<->[]byte on a Hot Parser Path

A log-lookup service reads request bytes off a socket and matches them against an
in-memory index keyed by string. The naive path allocates a fresh string on every
`string(b)`; at high request rates that copy dominates the allocation profile. This
exercise builds both paths — a safe baseline and an unsafe zero-copy helper gated
behind a documented contract — and uses `testing.AllocsPerRun` to prove the
allocation delta is real.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
zerocopy/                   independent module: example.com/zerocopy
  go.mod                    go 1.25
  zerocopy.go               unsafeString, unsafeBytes, Lookup (safe + unsafe index probe)
  cmd/
    demo/
      main.go               runs a lookup and prints the alloc counts
  zerocopy_test.go          correctness parity + AllocsPerRun assertions + benchmark
```

- Files: `zerocopy.go`, `cmd/demo/main.go`, `zerocopy_test.go`.
- Implement: `unsafeString(b []byte) string` via `unsafe.String(unsafe.SliceData(b), len(b))`, its inverse `unsafeBytes(s string) []byte`, and an index `Lookup` that probes a `map[string]int` with a zero-copy key.
- Test: parity of `unsafeString(b)` with `string(b)` for ASCII, multibyte, and empty input; `AllocsPerRun` pinning the safe conversion at >=1 alloc and the unsafe helper at 0; a benchmark comparing both.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### The copy the type system forces, and the escape hatch

`string(b)` must copy because the result is immutable and `b` is not: if they
shared storage, a later write to `b` would mutate a string. That copy is correct and
usually cheap — but on a parser that turns *every* token into a string to probe an
index, it is often the top line in a `-benchmem` profile. Go already elides the copy
when `string(b)` is used *only* as a map key (`m[string(b)]`), so the first thing to
reach for is that special case, not `unsafe`: it is zero-allocation and completely
safe. The `Lookup` here uses exactly that form for its primary path.

When you need a string that escapes the single-use forms — you want to hold it, pass
it, store it briefly — and you can guarantee the backing bytes are read-only for the
string's whole life, `unsafe.String(unsafe.SliceData(b), len(b))` builds a string
header pointing straight at `b`'s bytes, no copy. `unsafe.SliceData(b)` returns the
`*byte` at the slice's data pointer; `unsafe.String` wraps that pointer and a length
into a string header. The inverse, `unsafe.Slice(unsafe.StringData(s), len(s))`,
reinterprets a string as a slice.

The contract is the entire point, so it is written into the helper's doc comment:
the `[]byte` must not be mutated while the returned string is alive, and neither the
string nor the slice may be retained past the backing array's lifetime. Break the
first rule and you mutate an "immutable" string — every reader of it, including the
runtime, may misbehave, and the race detector may not catch it because no write to
the string was ever expected. Break the second and you have a dangling pointer into
freed or reused memory. The safe move is to use the helper only where both ends of
the buffer's life are visible in one function, and to prefer the compiler's map-key
elision anywhere it applies.

Create `zerocopy.go`:

```go
package zerocopy

import "unsafe"

// unsafeString reinterprets b as a string with no copy. CONTRACT: the caller must
// not mutate b while the returned string is used, and must not retain the string
// past b's lifetime. Violating either corrupts the "immutable" string or dangles.
// Use only where b is provably read-only and outlives the returned string.
func unsafeString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// unsafeBytes reinterprets s as a []byte with no copy. CONTRACT: the returned
// slice must NEVER be written to (s is immutable; mutating shared storage is
// undefined) and must not outlive s.
func unsafeBytes(s string) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// Index maps a log key to a numeric id. It is read-only after Build.
type Index struct {
	m map[string]int
}

// Build constructs an Index from key->id pairs.
func Build(pairs map[string]int) *Index {
	m := make(map[string]int, len(pairs))
	for k, v := range pairs {
		m[k] = v
	}
	return &Index{m: m}
}

// Lookup probes the index with a key given as raw bytes. It uses map(string(b))
// key elision: the compiler skips the string copy because the converted value is
// used only as a map key. This is zero-allocation AND fully safe.
func (ix *Index) Lookup(key []byte) (int, bool) {
	id, ok := ix.m[string(key)]
	return id, ok
}

// LookupUnsafe probes the index using the unsafe zero-copy helper. It is here to
// contrast with Lookup; for a bare map probe, prefer Lookup's key elision.
func (ix *Index) LookupUnsafe(key []byte) (int, bool) {
	id, ok := ix.m[unsafeString(key)]
	return id, ok
}
```

### The runnable demo

The demo builds a small index and reports allocations for the safe conversion, the
map-key elision, and the unsafe helper, so you can see the copy appear and vanish.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"testing"

	"example.com/zerocopy"
)

func main() {
	ix := zerocopy.Build(map[string]int{"GET /health": 1, "POST /login": 2})
	key := []byte("GET /health")

	id, ok := ix.Lookup(key)
	fmt.Printf("lookup: id=%d ok=%v\n", id, ok)

	safe := testing.AllocsPerRun(100, func() {
		s := string(key)
		if len(s) == 0 {
			panic("unreachable")
		}
		sink(s)
	})
	elided := testing.AllocsPerRun(100, func() {
		ix.Lookup(key)
	})
	unsafeAllocs := testing.AllocsPerRun(100, func() {
		ix.LookupUnsafe(key)
	})
	fmt.Printf("string(b) escaping: %.0f alloc/op\n", safe)
	fmt.Printf("map key elision:    %.0f alloc/op\n", elided)
	fmt.Printf("unsafe helper:      %.0f alloc/op\n", unsafeAllocs)
}

var sunk string

func sink(s string) { sunk = s }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
lookup: id=1 ok=true
string(b) escaping: 1 alloc/op
map key elision:    0 alloc/op
unsafe helper:      0 alloc/op
```

### Tests

`TestUnsafeStringParity` proves the unsafe conversion produces byte-identical
results to `string(b)` across ASCII, multibyte UTF-8, and empty input — a zero-copy
reinterpretation must not change the bytes. `TestAllocs` pins the operational claim:
a `string(b)` that escapes allocates at least once, while the unsafe helper and the
map-key elision allocate zero. `TestMutationHazard` documents the foot-gun by
construction rather than by prose: it shows that mutating the source slice after an
unsafe conversion changes the string's observed bytes — the exact undefined behavior
the contract forbids, demonstrated in a controlled place so the reader sees why the
rule exists.

Create `zerocopy_test.go`:

```go
package zerocopy

import (
	"testing"
)

func TestUnsafeStringParity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []byte
	}{
		{"ascii", []byte("GET /health")},
		{"multibyte", []byte("café 日本語 𠀀")},
		{"empty", []byte("")},
		{"nil", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := unsafeString(tc.in)
			want := string(tc.in)
			if got != want {
				t.Fatalf("unsafeString = %q, want %q", got, want)
			}
		})
	}
}

func TestUnsafeBytesParity(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "x", "café 𠀀"} {
		got := unsafeBytes(s)
		if string(got) != s {
			t.Fatalf("unsafeBytes(%q) round-trip = %q", s, string(got))
		}
	}
}

func TestAllocs(t *testing.T) {
	// No t.Parallel(): testing.AllocsPerRun must not run under a parallel test.
	key := []byte("GET /health")
	ix := Build(map[string]int{"GET /health": 1})

	safe := testing.AllocsPerRun(1000, func() {
		s := string(key)
		sink(s)
	})
	if safe < 1 {
		t.Fatalf("escaping string(b) allocated %.2f/op; want >= 1", safe)
	}

	elided := testing.AllocsPerRun(1000, func() {
		ix.Lookup(key)
	})
	if elided != 0 {
		t.Fatalf("map-key elision allocated %.2f/op; want 0", elided)
	}

	unsafeAllocs := testing.AllocsPerRun(1000, func() {
		ix.LookupUnsafe(key)
	})
	if unsafeAllocs != 0 {
		t.Fatalf("unsafe helper allocated %.2f/op; want 0", unsafeAllocs)
	}
}

// TestMutationHazard demonstrates, in a controlled place, why the unsafe contract
// forbids mutating the source: the derived string aliases the same memory, so a
// write to the slice is observable through the string. Production code must NEVER
// do this; the test exists to make the hazard concrete.
func TestMutationHazard(t *testing.T) {
	t.Parallel()
	b := []byte("secret")
	s := unsafeString(b)
	if s != "secret" {
		t.Fatalf("pre-mutation = %q", s)
	}
	b[0] = 'S' // forbidden in real code: mutating a buffer aliased by a live string
	if s != "Secret" {
		t.Fatalf("aliasing not observed: %q (expected the write to show through)", s)
	}
}

var sunk string

func sink(s string) { sunk = s }

func BenchmarkSafeVsUnsafe(b *testing.B) {
	key := []byte("GET /health")
	ix := Build(map[string]int{"GET /health": 1})

	b.Run("safe_string_conv", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			s := string(key)
			sink(s)
		}
	})
	b.Run("map_key_elision", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			ix.Lookup(key)
		}
	})
	b.Run("unsafe_helper", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			ix.LookupUnsafe(key)
		}
	})
}
```

## Review

Correctness first: a zero-copy conversion is only legitimate if it produces the
same bytes as the copying conversion, which `TestUnsafeStringParity` and
`TestUnsafeBytesParity` assert across ASCII, multibyte, and empty inputs. The
operational claim — that the copy is real and the elision removes it — is pinned by
`TestAllocs`: an escaping `string(b)` allocates, the map-key form and the unsafe
helper do not. The mutation test is the cautionary half: it shows the aliasing is
real, which is exactly why the contract forbids writing to the source.

The senior lesson is the ordering of tools. Reach for the compiler's map-key
elision first — it is safe and zero-allocation. Reach for `unsafe.String` only when
you need a string that escapes those special forms *and* you can prove the buffer is
read-only and outlives the string. Never mutate an aliased buffer, never retain the
handle past the backing array; when you cannot guarantee both, pay the copy.

## Resources

- [`unsafe.String` / `unsafe.Slice`](https://pkg.go.dev/unsafe#String) — the zero-copy conversions and their data-pointer helpers.
- [Go spec: Conversions to and from string types](https://go.dev/ref/spec#Conversions_to_and_from_a_string_type) — why the copy is required.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — measuring the allocation delta.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-buffer-pool-response-builder.md](03-buffer-pool-response-builder.md)
