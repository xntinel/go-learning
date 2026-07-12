# Exercise 9: Zero-Alloc Rate-Limiter Key: A CI-Enforced Allocation Budget

A rate limiter consults a key derived from `tenant`, `route`, and a time bucket
on every single request. If building that key allocates, you pay for it a few
million times a second under load. This module builds the key with a caller-owned
fixed-size buffer so construction is genuinely zero-alloc, then locks that
property behind a table-driven allocation budget that fails CI the moment a
refactor reintroduces an escape.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
ratekey/                      independent module: example.com/ratekey
  go.mod                      go 1.26
  ratekey.go                  KeyBuf [N]byte; AppendKey (zero-alloc into caller buf),
                              KeyString (one owned alloc), SprintfKey (allocating baseline)
  cmd/
    demo/
      main.go                 builds a key, prints measured allocs/op for both paths
  ratekey_test.go             budget table (AppendKey <= 0), uniqueness/stability, benchmarks
```

Files: `ratekey.go`, `cmd/demo/main.go`, `ratekey_test.go`.
Implement: `AppendKey(dst []byte, tenant, route string, bucket int64) []byte`
(no heap allocation), `KeyString` (one allocation, for an owned key), and
`SprintfKey` (the `fmt`-based baseline that allocates).
Test: a budget table asserting `AllocsPerRun <= budget` per builder (AppendKey is
`0`), a uniqueness/stability test, and `b.Loop`/`ReportAllocs` benchmarks.
Verify: `go test -count=1 -race ./...`, then `go test -bench=. -benchmem ./...`.

### Why a caller-owned buffer is the whole trick

A function that returns a fresh `string` on the request path cannot be zero-alloc:
the returned string owns its bytes, and those bytes are a heap allocation. The
only way to build a key without allocating is to write it into memory the caller
already owns and reuses. `KeyBuf` is a fixed-size `[128]byte` array; the caller
keeps one on its stack (or in a `sync.Pool`) and hands a zero-length reslice of it
to `AppendKey`. Because the array has capacity 128 and the assembled key fits,
every `append` stays within the existing backing array and never grows — so no
allocation happens. `strconv.AppendInt` formats the bucket integer straight into
that same backing array, with no intermediate string and no `fmt` reflection.

The returned `[]byte` aliases the caller's buffer. That is the point and the
hazard: it is valid only until the buffer is reused, exactly like the `sync.Pool`
discipline from Exercise 4. For a transient use — a map lookup — that is perfect,
because Go compiles `m[string(key)]` without copying the bytes into a new string.
When you must *retain* the key (store it in a struct, send it on a channel), call
`KeyString`, which pays exactly one allocation for an owned copy. The two-tier API
makes the cost explicit: free for lookups, one alloc when you keep it.

`SprintfKey` is the baseline the budget test measures against: `fmt.Sprintf`
boxes each argument into an `any`, reflects over the verbs, and allocates the
result string — several allocations for the same three fields. Seeing the gap is
the lesson; enforcing that `AppendKey` never drifts toward it is the guard.

The budget test is the deliverable that outlives this exercise. It maps each
builder to a maximum `allocs/op` and fails if `testing.AllocsPerRun` exceeds it.
Asserting a *budget* rather than an exact count keeps the guard portable across Go
versions and the race detector (which shift absolute numbers), while still
catching the regression that matters: a zero-alloc builder that starts allocating.

Create `ratekey.go`:

```go
package ratekey

import (
	"fmt"
	"strconv"
)

// MaxKeyLen bounds a rate-limiter key. KeyBuf is a caller-owned scratch buffer:
// keep one per goroutine (or in a sync.Pool) and reslice it as buf[:0] so
// AppendKey writes in place without allocating.
const MaxKeyLen = 128

// KeyBuf is a fixed-size backing array for zero-allocation key assembly.
type KeyBuf [MaxKeyLen]byte

// AppendKey writes "tenant:route:bucket" into dst and returns the used slice.
// It performs no heap allocation when dst has spare capacity (a KeyBuf[:0]):
// each append stays within the backing array and the integer is formatted with
// AppendInt directly into it. The result aliases dst and is valid only until dst
// is reused; copy it out (or use KeyString) to retain it.
func AppendKey(dst []byte, tenant, route string, bucket int64) []byte {
	dst = append(dst, tenant...)
	dst = append(dst, ':')
	dst = append(dst, route...)
	dst = append(dst, ':')
	dst = strconv.AppendInt(dst, bucket, 10)
	return dst
}

// KeyString returns an owned key string. It costs exactly one allocation (the
// returned string's bytes); use it when the key must outlive the call, not on a
// pure lookup path where m[string(AppendKey(...))] avoids the copy.
func KeyString(tenant, route string, bucket int64) string {
	var buf KeyBuf
	return string(AppendKey(buf[:0], tenant, route, bucket))
}

// SprintfKey is the allocating baseline: fmt.Sprintf boxes each argument and
// allocates the result. It exists so the budget test can measure the gap.
func SprintfKey(tenant, route string, bucket int64) string {
	return fmt.Sprintf("%s:%s:%d", tenant, route, bucket)
}
```

### The runnable demo

The demo builds a key the zero-alloc way, prints it, shows the owned variant, and
then measures both construction paths with `testing.AllocsPerRun`. Importing
`testing` from `main` is unusual but deliberate: the allocation count is the
observable this module is about.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"testing"

	"example.com/ratekey"
)

func main() {
	const (
		tenant = "acme"
		route  = "GET /v1/orders"
		bucket = 1024
	)

	var buf ratekey.KeyBuf
	key := ratekey.AppendKey(buf[:0], tenant, route, bucket)
	fmt.Printf("key:   %s\n", key)
	fmt.Printf("owned: %s\n", ratekey.KeyString(tenant, route, bucket))

	var sink int
	appendAllocs := testing.AllocsPerRun(1000, func() {
		var b ratekey.KeyBuf
		k := ratekey.AppendKey(b[:0], tenant, route, bucket)
		sink += len(k)
	})
	sprintfAllocs := testing.AllocsPerRun(1000, func() {
		sink += len(ratekey.SprintfKey(tenant, route, bucket))
	})
	_ = sink

	fmt.Printf("AppendKey allocs/op:      %.0f\n", appendAllocs)
	fmt.Printf("AppendKey is zero-alloc:  %v\n", appendAllocs == 0)
	fmt.Printf("SprintfKey allocates more: %v\n", sprintfAllocs > appendAllocs)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
key:   acme:GET /v1/orders:1024
owned: acme:GET /v1/orders:1024
AppendKey allocs/op:      0
AppendKey is zero-alloc:  true
SprintfKey allocates more: true
```

### Tests

`TestAllocationBudget` is the enforced contract: a table maps each builder to a
maximum `allocs/op`, and the test fails if `AllocsPerRun` exceeds it. `AppendKey`
is pinned to `0` — if a future edit makes it escape (returning the buffer to a
global, growing past `MaxKeyLen`, adding an `fmt` call), CI catches it here rather
than in a flame graph. `TestKeyUniqueAndStable` proves the key is correct as a key:
identical inputs yield an identical string, and any field change yields a different
one. The benchmarks use `for b.Loop()` (Go 1.24+) with `b.ReportAllocs()` to
document `allocs/op` and `B/op`.

Create `ratekey_test.go`:

```go
package ratekey

import (
	"fmt"
	"testing"
)

var sink int

// assertBudget fails if f averages more than budget heap allocations per call.
func assertBudget(t *testing.T, name string, budget float64, f func()) {
	t.Helper()
	got := testing.AllocsPerRun(1000, f)
	if got > budget {
		t.Errorf("%s: allocs/op = %.2f, want <= %.0f (allocation budget exceeded)", name, got, budget)
	}
}

func TestAllocationBudget(t *testing.T) {
	const (
		tenant = "acme"
		route  = "GET /v1/orders"
		bucket = 1024
	)
	cases := []struct {
		name   string
		budget float64
		fn     func()
	}{
		{"AppendKey", 0, func() {
			var b KeyBuf
			k := AppendKey(b[:0], tenant, route, bucket)
			sink += len(k)
		}},
		{"KeyString", 1, func() {
			sink += len(KeyString(tenant, route, bucket))
		}},
	}
	for _, tc := range cases {
		assertBudget(t, tc.name, tc.budget, tc.fn)
	}
}

func TestKeyUniqueAndStable(t *testing.T) {
	t.Parallel()
	// Same inputs must produce the same key (stability).
	a := KeyString("acme", "GET /v1/orders", 1024)
	b := KeyString("acme", "GET /v1/orders", 1024)
	if a != b {
		t.Fatalf("unstable key: %q != %q", a, b)
	}
	// Any field change must produce a different key (uniqueness).
	diffs := []string{
		KeyString("globex", "GET /v1/orders", 1024),
		KeyString("acme", "POST /v1/orders", 1024),
		KeyString("acme", "GET /v1/orders", 1025),
	}
	for _, d := range diffs {
		if d == a {
			t.Errorf("collision: %q collides with base %q", d, a)
		}
	}
}

func TestAppendMatchesSprintf(t *testing.T) {
	t.Parallel()
	var buf KeyBuf
	got := string(AppendKey(buf[:0], "acme", "GET /v1/orders", 1024))
	want := SprintfKey("acme", "GET /v1/orders", 1024)
	if got != want {
		t.Errorf("AppendKey = %q, want %q (must match the fmt baseline)", got, want)
	}
}

func BenchmarkAppendKey(b *testing.B) {
	b.ReportAllocs()
	var buf KeyBuf
	for b.Loop() {
		k := AppendKey(buf[:0], "acme", "GET /v1/orders", 1024)
		sink += len(k)
	}
}

func BenchmarkSprintfKey(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sink += len(SprintfKey("acme", "GET /v1/orders", 1024))
	}
}

func Example() {
	var buf KeyBuf
	key := AppendKey(buf[:0], "acme", "GET /v1/orders", 1024)
	// A map lookup on string(key) does not copy the bytes into a new string.
	seen := map[string]int{}
	seen[string(key)]++
	seen[string(key)]++
	fmt.Println(seen["acme:GET /v1/orders:1024"])
	// Output: 2
}
```

## Review

The guard is correct when `TestAllocationBudget` passes with `AppendKey` pinned to
a budget of exactly `0`: the compiler's zero-escape verdict is now enforced by CI,
not eyeballed from `-gcflags=-m`. The mechanism is a caller-owned `KeyBuf` that
`AppendKey` writes into in place — no returned string, no `fmt`, so nothing
allocates on the lookup path. The two hazards are the ones this exercise makes
concrete: the returned slice aliases the buffer, so retain it only via `KeyString`
(one honest allocation), never by stashing the raw `[]byte`; and the buffer must be
large enough that `append` never grows past `MaxKeyLen`, or a growth reallocation
silently reintroduces the escape the budget test then catches. Assert a budget, not
an exact count, so the guard survives Go upgrades and `-race`.

## Resources

- [testing.AllocsPerRun](https://pkg.go.dev/testing#AllocsPerRun) — average heap allocations per call, the basis of the budget.
- [strconv.AppendInt](https://pkg.go.dev/strconv#AppendInt) — format an integer into an existing byte slice with no allocation.
- [testing.B.Loop](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop used with `ReportAllocs`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-builder-vs-buffer-io-writer.md](08-builder-vs-buffer-io-writer.md) | Next: [../08-pointers-in-slices-and-maps/00-concepts.md](../08-pointers-in-slices-and-maps/00-concepts.md)
