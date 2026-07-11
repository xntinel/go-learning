# Exercise 9: Slice-Parameter Overload for a Hot-Path Key Join

A variadic call is not free: `f(a, b, c)` allocates a fresh backing array every
time. On a hot path that already holds a slice, that per-call allocation adds up.
You build a `JoinSegments(parts []string)` slice-parameter core plus a thin
variadic `JoinSegmentsV(parts ...string)` wrapper, and you measure the allocation
delta with a benchmark so the decision to expose a slice overload is data, not
folklore.

This module is self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
segjoin/                   independent module: example.com/segjoin
  go.mod                   go 1.25
  segjoin.go               JoinSegments([]string) core; JoinSegmentsV(...string) wrapper
  cmd/
    demo/
      main.go              runnable demo: both entry points, identical output
  segjoin_test.go          equality test + allocation benchmark (ReportAllocs)
```

- Files: `segjoin.go`, `cmd/demo/main.go`, `segjoin_test.go`.
- Implement: `JoinSegments(parts []string) string` (the core, using `strings.Builder`) and `JoinSegmentsV(parts ...string) string` (a wrapper delegating to the core).
- Test: both produce identical output over the same data; a benchmark with `b.ReportAllocs()` compares the variadic wrapper against the slice core.
- Verify: `go test -count=1 -race ./...` (and `go test -bench . -benchmem` to see allocations).

Set up the module:

```bash
mkdir -p ~/go-exercises/segjoin/cmd/demo
cd ~/go-exercises/segjoin
go mod init example.com/segjoin
go mod edit -go=1.25
```

### Why a slice core and a variadic wrapper coexist

The public sugar `JoinSegmentsV("a", "b", "c")` is pleasant to call, but every such
call synthesizes a `[]string` backing array for the arguments — one allocation per
call. When the caller already holds a `[]string` (a repository iterating a batch, a
serializer walking fields), forcing them through the variadic form means each call
allocates a throwaway slice on top of the work. The fix is to put the real work in
a `JoinSegments(parts []string)` core that takes the slice directly, and make the
variadic form a one-line wrapper: `JoinSegmentsV(parts ...string) string { return
JoinSegments(parts) }`. A caller with a slice calls the core and pays nothing
extra; a caller with loose arguments calls the wrapper and pays the one allocation
they were going to pay anyway.

The subtlety worth stating: passing a slice *into* the wrapper — `JoinSegmentsV(sl...)`
— does *not* allocate, because splatting reuses `sl`'s header. The allocation cost
is specific to the non-splatted form `JoinSegmentsV("a", "b")`, where the compiler
must build the array. So the slice core matters most for call sites that construct
the arguments inline in a loop. The benchmark makes this concrete: it compares
calling the core over a pre-materialized slice against calling the wrapper with
inline literal arguments, and `-benchmem` shows the allocation delta. That measured
delta is the decision criterion — expose the slice overload when the hot path shows
allocations you care about, and skip the extra surface when it does not.

The core uses `strings.Builder` and pre-sizes it with `Grow` so the join itself
does not reallocate as it appends, keeping the measurement about the *call*
overhead rather than builder growth.

Create `segjoin.go`:

```go
// segjoin.go
package segjoin

import "strings"

// JoinSegments joins parts with ':'. This is the slice-parameter core: a caller
// that already holds a []string calls it with no per-call argument allocation.
func JoinSegments(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	n := len(parts) - 1 // separators
	for _, p := range parts {
		n += len(p)
	}
	var b strings.Builder
	b.Grow(n)
	for i, p := range parts {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(p)
	}
	return b.String()
}

// JoinSegmentsV is the variadic public sugar. It delegates to JoinSegments so the
// two never drift. The non-splatted call form allocates a backing array for the
// arguments; the slice core does not.
func JoinSegmentsV(parts ...string) string {
	return JoinSegments(parts)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/segjoin"
)

func main() {
	parts := []string{"users", "alice", "42"}

	fmt.Println(segjoin.JoinSegments(parts))
	fmt.Println(segjoin.JoinSegmentsV("users", "alice", "42"))
	fmt.Println(segjoin.JoinSegmentsV(parts...))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
users:alice:42
users:alice:42
users:alice:42
```

### Tests

`TestCoreAndWrapperAgree` proves the two entry points are interchangeable over the
same data. The two benchmarks quantify the difference: `BenchmarkVariadicInline`
calls the wrapper with inline literals (allocating the argument array each call);
`BenchmarkSliceCore` calls the core over a pre-built slice. Run `go test -bench .
-benchmem` and read the `allocs/op` column — the inline variadic form shows an
extra allocation the slice core avoids.

Create `segjoin_test.go`:

```go
// segjoin_test.go
package segjoin

import (
	"fmt"
	"testing"
)

func TestCoreAndWrapperAgree(t *testing.T) {
	t.Parallel()

	cases := [][]string{
		{},
		{"solo"},
		{"users", "alice", "42"},
		{"a", "", "b"},
	}
	for _, parts := range cases {
		if got, want := JoinSegmentsV(parts...), JoinSegments(parts); got != want {
			t.Errorf("JoinSegmentsV(%v) = %q, core = %q", parts, got, want)
		}
	}
}

var sink string

func BenchmarkVariadicInline(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		sink = JoinSegmentsV("users", "alice", "42")
	}
}

func BenchmarkSliceCore(b *testing.B) {
	parts := []string{"users", "alice", "42"}
	b.ReportAllocs()
	for range b.N {
		sink = JoinSegments(parts)
	}
}

func Example() {
	fmt.Println(JoinSegments([]string{"users", "alice", "42"}))
	// Output: users:alice:42
}
```

## Review

The pair is correct when the wrapper and the core produce identical output for
every input, including the empty and empty-segment cases, so exposing both is pure
ergonomics with no behavioral fork. The measurement is the point: `-benchmem` shows
the inline variadic call carrying an allocation the slice core does not, and that
number — not intuition — decides whether a given hot path deserves the slice
overload. The mistake to avoid is assuming the variadic sugar is free and reaching
for it inside a tight loop that already has a slice; the fix is the slice core you
built here. Run `go test -race` for the equality test, then `go test -bench .
-benchmem`.

## Resources

- [`testing.B` and `B.ReportAllocs`](https://pkg.go.dev/testing#B.ReportAllocs)
- [`strings.Builder` and `Builder.Grow`](https://pkg.go.dev/strings#Builder.Grow)
- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-validator-combinator-errorsjoin.md](08-validator-combinator-errorsjoin.md) | Next: [10-splat-aliasing-and-copy-safety.md](10-splat-aliasing-and-copy-safety.md)
