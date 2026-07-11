# Exercise 7: Zero-Copy []byte to string In A Cache-Key Hot Path

`string(b)` copies the bytes, which is the correct default. But on a hot path
that turns millions of byte buffers into map keys, that copy is pure allocation
pressure. `unsafe.String` reinterprets the same backing array as a string with no
copy — at the cost of an aliasing invariant you must uphold. This exercise builds
both, benchmarks the difference, and documents exactly when the unsafe form is
legitimate.

This module is fully self-contained: its own module, all code inline, its own
demo and tests.

## What you'll build

```text
zerocopy/                    independent module: example.com/zerocopy
  go.mod                     go 1.26
  zerocopy.go                SafeKey([]byte) string; UnsafeKey([]byte) string
  cmd/
    demo/
      main.go                runnable demo: both produce the same key
  zerocopy_test.go           equality test + alias-hazard doc + allocation benchmarks
```

- Files: `zerocopy.go`, `cmd/demo/main.go`, `zerocopy_test.go`.
- Implement: `SafeKey(b []byte) string` (copying `string(b)`) and `UnsafeKey(b []byte) string` (zero-copy `unsafe.String`), with the aliasing invariant documented.
- Test: correctness that both agree for representative inputs including empty; a documented mutate-after-alias case; benchmarks with `ReportAllocs` showing 0 allocs on the unsafe path.
- Verify: `go test -count=1 -race ./...` (and `go test -bench=. -benchmem` to see the allocation difference).

Set up the module:

```bash
mkdir -p ~/go-exercises/zerocopy/cmd/demo
cd ~/go-exercises/zerocopy
go mod init example.com/zerocopy
go mod edit -go=1.26
```

### The copy, the alias, and the invariant

A `[]byte` and a `string` have nearly the same memory layout — a pointer and a
length — but they differ in one contract: a string's bytes are immutable, a
slice's are not. `string(b)` bridges the two safely by allocating a new array and
copying `b` into it, so later writes to `b` cannot affect the string. That copy
is `O(len)` work and one allocation every call.

`unsafe.String(unsafe.SliceData(b), len(b))` instead builds a string header that
points *at the same backing array* as `b`. No allocation, no copy — and an alias.
The language then requires that you never mutate `b` for as long as the string is
reachable, because doing so mutates a value the type system believes is immutable,
which is undefined behavior (the compiler may have constant-folded, deduplicated,
or used the string as a map key whose hash is now stale). `unsafe.SliceData`
returns the pointer to the first element; for a zero-length slice that pointer may
be `nil`, and `unsafe.String(nil, 0)` is the empty string, so `UnsafeKey` guards
`len(b) == 0` explicitly rather than relying on that edge.

The legitimate use is narrow and real: a cache or dedup map keyed by bytes you
have just read and will not touch again before the lookup returns. You build the
key, probe the map, and discard the key — the alias lives only for the duration
of the lookup, during which `b` is not written. That single lookup, multiplied by
millions of requests, is where the eliminated allocation matters. Anywhere the
key outlives the buffer, or the buffer is reused (a pooled read buffer refilled
by the next `Read`), the copy is mandatory.

Create `zerocopy.go`:

```go
// zerocopy.go
package zerocopy

import "unsafe"

// SafeKey copies b into an independent string. Always correct; allocates.
func SafeKey(b []byte) string {
	return string(b)
}

// UnsafeKey reinterprets b's backing array as a string with no copy.
//
// Invariant: the caller must not mutate b while the returned string is in use.
// Legitimate only for a transient key over a buffer that is not written again
// for the string's lifetime (for example a map lookup that discards the key).
func UnsafeKey(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}
```

### The runnable demo

Create `cmd/demo/main.go`. It uses the unsafe key for a lookup and then discards
it, which is the safe usage pattern:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/zerocopy"
)

func main() {
	seen := map[string]int{}
	requests := [][]byte{[]byte("GET /a"), []byte("GET /b"), []byte("GET /a")}

	for _, raw := range requests {
		// The key is used only for this lookup and not retained, and raw is
		// not mutated, so the zero-copy conversion is safe here.
		seen[zerocopy.UnsafeKey(raw)]++
	}

	fmt.Println("GET /a:", seen["GET /a"])
	fmt.Println("GET /b:", seen["GET /b"])
	fmt.Println("safe == unsafe:",
		zerocopy.SafeKey([]byte("x")) == zerocopy.UnsafeKey([]byte("x")))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /a: 2
GET /b: 1
safe == unsafe: true
```

### Tests

The correctness test proves the two functions agree, including the empty-slice
edge. A second test documents the aliasing hazard concretely: mutating the
backing slice after `UnsafeKey` changes the string, which is exactly the bug the
invariant forbids. The benchmarks, run with `-benchmem`, show the copy path
allocating and the unsafe path not.

Create `zerocopy_test.go`:

```go
// zerocopy_test.go
package zerocopy

import "testing"

func TestKeysAgree(t *testing.T) {
	t.Parallel()
	inputs := [][]byte{
		[]byte(""),
		[]byte("a"),
		[]byte("GET /orders/123"),
		[]byte("héllo 世界"),
	}
	for _, in := range inputs {
		if got, want := UnsafeKey(in), SafeKey(in); got != want {
			t.Fatalf("UnsafeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAliasHazard documents why the invariant exists: mutating the backing
// bytes after an unsafe conversion is observed by the string. SafeKey is
// immune because it copied.
func TestAliasHazard(t *testing.T) {
	t.Parallel()
	b := []byte("cat")
	unsafeStr := UnsafeKey(b)
	safeStr := SafeKey(b)

	b[0] = 'h' // mutate the backing array; forbidden while unsafeStr is in use

	if safeStr != "cat" {
		t.Fatalf("SafeKey must be immune to later mutation, got %q", safeStr)
	}
	if unsafeStr != "hat" {
		t.Fatalf("expected the alias to observe the mutation, got %q", unsafeStr)
	}
}

var sink string

func BenchmarkSafeKey(b *testing.B) {
	buf := []byte("GET /orders/123")
	b.ReportAllocs()
	for range b.N {
		sink = SafeKey(buf)
	}
}

func BenchmarkUnsafeKey(b *testing.B) {
	buf := []byte("GET /orders/123")
	b.ReportAllocs()
	for range b.N {
		sink = UnsafeKey(buf)
	}
}
```

## Review

The pair is correct when `UnsafeKey` and `SafeKey` return equal strings for every
input and `UnsafeKey` never allocates. `go test -bench=. -benchmem` makes the
distinction concrete: `BenchmarkSafeKey` reports `1 allocs/op` and a nonzero
`B/op`, `BenchmarkUnsafeKey` reports `0 allocs/op`. The `TestAliasHazard` case is
the whole reason the invariant is written on the function: it deliberately does
the forbidden thing (mutate `b` after conversion) to show the alias observes it,
so a reader understands that using `UnsafeKey` where the buffer is later reused is
a real corruption, not a theoretical one. Reach for `UnsafeKey` only when you can
point to the exact lines proving the buffer is immutable for the string's
lifetime; when in doubt, `SafeKey` is the correct default and the copy is cheap
relative to a bug.

## Resources

- [unsafe.String](https://pkg.go.dev/unsafe#String) — building a string header over existing bytes, and its constraints.
- [unsafe.SliceData](https://pkg.go.dev/unsafe#SliceData) — the pointer to a slice's backing array.
- [testing.B.ReportAllocs](https://pkg.go.dev/testing#B.ReportAllocs) — enabling per-op allocation reporting in a benchmark.
- [Go Specification: Package unsafe](https://go.dev/ref/spec#Package_unsafe) — the rules governing `unsafe` conversions.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-any-attr-value-type-switch.md](08-any-attr-value-type-switch.md)
