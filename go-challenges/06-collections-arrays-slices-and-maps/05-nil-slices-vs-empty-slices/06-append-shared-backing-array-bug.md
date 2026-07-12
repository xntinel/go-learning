# Exercise 6: Fix a request-scoped slice that corrupts a shared header template

A handler builds each request's headers by appending to a package-level default
slice that happens to have spare capacity. Because `append` reuses that capacity,
two concurrent requests write into the same backing-array slot and clobber each
other — a data race, not merely a surprising overwrite. This exercise reproduces
the bug deterministically, then fixes it with the three-index slice expression
and `slices.Clip`.

This module is fully self-contained: its own `go mod init`, its own `headers`
package, its own demo and tests.

## What you'll build

```text
reqheaders/                   independent module: example.com/reqheaders
  go.mod
  headers/headers.go          shared base, BuildUnsafe (aliases), Build (three-index), BuildClip
  headers/headers_test.go     aliasing-bug proof, no-alias proofs, cap check, -race test
  cmd/demo/main.go            shows unsafe clobber vs safe independent derivations
```

Files: `headers/headers.go`, `headers/headers_test.go`, `cmd/demo/main.go`.
Implement: a package-level `base` with spare capacity, a `BuildUnsafe` that
appends onto it directly, and `Build`/`BuildClip` that derive a fresh backing
array first.
Test: `BuildUnsafe` aliases (the second call clobbers the first); `Build` and
`BuildClip` do not; `base[:n:n]` has `cap == len`; a `-race` test drives the safe
`Build` concurrently.
Verify: `go test -count=1 -race ./...`

### How the shared backing array corrupts requests

`base` is `make([]string, 2, 8)` — length two, capacity eight. That spare
capacity is the trap. When `BuildUnsafe` does `append(base, header)`, `append`
sees room in the existing backing array (cap 8 > len 2) and, instead of
allocating, writes the new element into `base`'s backing array at index 2 and
returns a header with length 3 pointing at that same array. Call `BuildUnsafe`
again and the second `append` writes *its* header into the very same index 2,
overwriting what the first call put there. Both returned slices point at one
array, so the first request's header silently becomes the second request's. Run
those two appends on two goroutines and the detector reports a write-write data
race on the shared slot.

The fix is to stop the derived slice from seeing `base`'s spare capacity. The
full three-index slice expression `base[:n:n]` produces a slice with length `n`
and capacity `n` — no room — so the next `append` is forced to allocate a fresh
backing array and copy, and the per-request header lands in storage nobody else
shares. `slices.Clip` does the same thing by trimming capacity down to length;
`append(slices.Clip(base), header)` is equivalent. Either way the base template
is only ever read, never appended into, so concurrent `Build` calls are
race-free. If you needed to mutate elements too, `slices.Clone` would give a full
independent copy up front.

Create `headers/headers.go`:

```go
package headers

import "slices"

// base is a package-level header template with spare capacity (len 2, cap 8).
// The spare capacity is exactly what makes append reuse its backing array.
var base = newBase()

func newBase() []string {
	s := make([]string, 2, 8)
	s[0], s[1] = "X-Service: api", "X-Version: 1"
	return s
}

// BuildUnsafe appends the per-request header directly onto the shared base.
// Because base has spare capacity, append writes into base's backing array at
// index len(base); two requests therefore write the SAME slot and clobber each
// other. Kept only to demonstrate the bug.
func BuildUnsafe(reqID string) []string {
	return append(base, "X-Request-Id: "+reqID)
}

// Build caps the base to its own length with a three-index slice expression
// base[:n:n], so the next append is forced to allocate a fresh backing array.
// The per-request slice is then independent of base and of other requests.
func Build(reqID string) []string {
	n := len(base)
	return append(base[:n:n], "X-Request-Id: "+reqID)
}

// BuildClip is an equivalent fix using slices.Clip, which trims capacity down to
// length; the following append must allocate.
func BuildClip(reqID string) []string {
	return append(slices.Clip(base), "X-Request-Id: "+reqID)
}
```

### The runnable demo

The demo runs the unsafe and safe paths single-threaded (so there is no race to
trip the detector) and shows the outcome: under `BuildUnsafe`, both slices end up
with the *second* request's id; under `Build`, each keeps its own.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/reqheaders/headers"
)

func main() {
	// Unsafe: two derivations alias the shared base; the second clobbers the first.
	a := headers.BuildUnsafe("req-A")
	b := headers.BuildUnsafe("req-B")
	fmt.Printf("unsafe a last: %s\n", a[len(a)-1])
	fmt.Printf("unsafe b last: %s\n", b[len(b)-1])

	// Safe: each derivation gets its own backing array.
	c := headers.Build("req-C")
	d := headers.Build("req-D")
	fmt.Printf("safe  c last: %s\n", c[len(c)-1])
	fmt.Printf("safe  d last: %s\n", d[len(d)-1])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
unsafe a last: X-Request-Id: req-B
unsafe b last: X-Request-Id: req-B
safe  c last: X-Request-Id: req-C
safe  d last: X-Request-Id: req-D
```

The first two lines are the bug: request A's header was overwritten by request
B's because they shared one backing array.

### Tests

`TestUnsafeAppendAliasesBase` pins the bug: two `BuildUnsafe` calls produce slices
whose last element is identical, and it is the second call's value.
`TestBuildDoesNotAlias` and `TestBuildClipDoesNotAlias` prove the two fixes keep
each derivation independent, and the former also asserts `base[:n:n]` has
`cap == len`, which is what forces the fresh allocation.
`TestConcurrentBuildIsRaceFree` drives `Build` from fifty goroutines under
`-race`; it passes because the base is only read, never appended into. The unsafe
path is deliberately never run concurrently, because doing so would be a real data
race that fails the gate — which is precisely the point.

Create `headers/headers_test.go`:

```go
package headers

import (
	"strings"
	"sync"
	"testing"
)

func lastOf(s []string) string { return s[len(s)-1] }

func TestUnsafeAppendAliasesBase(t *testing.T) {
	a := BuildUnsafe("A")
	b := BuildUnsafe("B")
	// Both appended into the same backing slot, so a's last header was
	// overwritten by b's. This is the bug the three-index fix removes.
	if lastOf(a) != lastOf(b) {
		t.Fatalf("expected aliasing: a=%q b=%q", lastOf(a), lastOf(b))
	}
	if !strings.HasSuffix(lastOf(a), "B") {
		t.Fatalf("a was not clobbered by b: %q", lastOf(a))
	}
}

func TestBuildDoesNotAlias(t *testing.T) {
	t.Parallel()
	a := Build("A")
	b := Build("B")
	if !strings.HasSuffix(lastOf(a), "A") || !strings.HasSuffix(lastOf(b), "B") {
		t.Fatalf("derivations aliased: a=%q b=%q", lastOf(a), lastOf(b))
	}
	// A three-index derivation has cap == len, so the next append allocates.
	n := len(base)
	capped := base[:n:n]
	if cap(capped) != n {
		t.Fatalf("cap(base[:n:n]) = %d, want %d", cap(capped), n)
	}
}

func TestBuildClipDoesNotAlias(t *testing.T) {
	t.Parallel()
	a := BuildClip("A")
	b := BuildClip("B")
	if !strings.HasSuffix(lastOf(a), "A") || !strings.HasSuffix(lastOf(b), "B") {
		t.Fatalf("derivations aliased: a=%q b=%q", lastOf(a), lastOf(b))
	}
}

func TestConcurrentBuildIsRaceFree(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := Build(string(rune('A' + i%26)))
			if len(got) != len(base)+1 {
				t.Errorf("len = %d, want %d", len(got), len(base)+1)
			}
		}()
	}
	wg.Wait()
}
```

## Review

The bug is real when `TestUnsafeAppendAliasesBase` shows two independent-looking
results sharing one header, and the fix is proven when `Build` and `BuildClip`
keep them separate and the concurrent test runs clean under `-race`. The
mechanism to internalize is that `append` reuses spare capacity, so any slice
handed a base with room to grow can silently write into that base's array;
`base[:n:n]` and `slices.Clip` remove the room and force a fresh allocation. This
is the difference between a logic bug you might catch in review and a data race
that only shows up under production concurrency, which is why the deterministic
single-threaded proof and the `-race` concurrent test are both here.

## Resources

- [Go Specification: Full slice expressions](https://go.dev/ref/spec#Slice_expressions) — the `a[low:high:max]` three-index form and its capacity.
- [slices.Clip](https://pkg.go.dev/slices#Clip) — trims capacity to length so the next append allocates.
- [Go blog: Arrays, slices (and strings): the mechanics of append](https://go.dev/blog/slices) — how append reuses or reallocates backing arrays.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-nil-map-read-vs-write-panic.md](07-nil-map-read-vs-write-panic.md)
