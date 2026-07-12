# Exercise 9: Pass-by-Value vs Pass-by-Pointer — Measuring Copy Cost in a Hot Function

"Pointers are cheaper for big structs" is folklore until you measure it. This
exercise turns it into a defensible decision: a `Big` struct with an embedded array
is passed to a hot function two ways — `SumByValue(Big)` and `SumByPointer(*Big)` —
and a benchmark quantifies the copy cost, with the escape-analysis caveat that
returning a pointer to a local can force a heap allocation the value form avoids.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
hotpath/                    independent module: example.com/hotpath
  go.mod
  hotpath.go                Big (embedded array); SumByValue/SumByPointer; BumpByValue/BumpByPointer
  cmd/
    demo/
      main.go               computes sums both ways and shows the mutation difference
  hotpath_test.go           both compute the same sum; byValue does not mutate, byPointer does; benchmarks
```

- Files: `hotpath.go`, `cmd/demo/main.go`, `hotpath_test.go`.
- Implement: a large `Big` struct, `SumByValue`/`SumByPointer` that read it, and `BumpByValue`/`BumpByPointer` that show the mutation contract.
- Test: both variants compute the same result, `BumpByValue` does not mutate the caller while `BumpByPointer` does, and `BenchmarkByValue`/`BenchmarkByPointer` use `b.Loop()` with `ReportAllocs`.
- Verify: `go test -count=1 -race ./...`

### Why measure instead of reaching for the pointer

`Big` embeds a 256-element `int64` array plus a 64-element bool array — roughly two
kilobytes. `SumByValue(b Big)` copies all of that on every call; `SumByPointer(*Big)`
copies one eight-byte address. For a function called in a tight loop, that copy is a
real cost, and the benchmark makes it visible: `BenchmarkByValue` should report more
bytes copied (and, depending on the compiler, more allocations) per operation than
`BenchmarkByPointer`.

But the folklore hides a caveat, which is why you measure rather than reflexively add
a `*`. Passing by pointer can force the value to *escape to the heap*: if the
compiler cannot prove the pointer does not outlive the call, it moves `Big` from the
stack to the heap, adding a garbage-collector burden the value form never had.
Returning `&local` from a constructor is the classic trigger. So the honest workflow
is: write both, benchmark with `b.Loop()` and `b.ReportAllocs()`, and read the escape
decisions with `go build -gcflags=-m`. The mutation contract is the other half of the
choice: `SumByValue`/`BumpByValue` cannot change the caller's `Big`, while the pointer
forms can — so the decision is never *only* about speed.

To keep the benchmark honest, both accumulate their result into a package-level
`sink`, so the compiler cannot delete the call as dead code.

Create `hotpath.go`:

```go
package hotpath

// Big is a large struct dominated by an embedded array: about 2 KB, so a
// pass-by-value copy is measurable.
type Big struct {
	ID      int64
	Name    string
	Payload [256]int64
	Flags   [64]bool
}

// SumByValue receives a COPY of b (the whole array is copied) and returns a
// derived value without mutating the caller.
func SumByValue(b Big) int64 {
	var s int64
	for _, v := range b.Payload {
		s += v
	}
	return s + b.ID
}

// SumByPointer receives only the address; no array copy. It reads, it does not
// mutate, but it structurally could.
func SumByPointer(b *Big) int64 {
	var s int64
	for _, v := range b.Payload {
		s += v
	}
	return s + b.ID
}

// BumpByValue mutates its COPY; the caller's Big is unchanged.
func BumpByValue(b Big) {
	b.ID++
}

// BumpByPointer mutates through the pointer; the caller observes the change.
func BumpByPointer(b *Big) {
	b.ID++
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/hotpath"
)

func main() {
	var b hotpath.Big
	b.ID = 5
	for i := range b.Payload {
		b.Payload[i] = int64(i)
	}

	fmt.Printf("SumByValue=%d SumByPointer=%d\n",
		hotpath.SumByValue(b), hotpath.SumByPointer(&b))

	hotpath.BumpByValue(b)
	fmt.Printf("after BumpByValue:   ID=%d\n", b.ID)
	hotpath.BumpByPointer(&b)
	fmt.Printf("after BumpByPointer: ID=%d\n", b.ID)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
SumByValue=32645 SumByPointer=32645
after BumpByValue:   ID=5
after BumpByPointer: ID=6
```

### Tests

Create `hotpath_test.go`:

```go
package hotpath

import "testing"

var sink int64

func filled() Big {
	var b Big
	b.ID = 5
	for i := range b.Payload {
		b.Payload[i] = int64(i)
	}
	return b
}

func TestSumFormsAgree(t *testing.T) {
	t.Parallel()
	b := filled()
	if SumByValue(b) != SumByPointer(&b) {
		t.Fatalf("value=%d pointer=%d, want equal", SumByValue(b), SumByPointer(&b))
	}
}

func TestBumpByValueDoesNotMutate(t *testing.T) {
	t.Parallel()
	b := filled()
	BumpByValue(b)
	if b.ID != 5 {
		t.Fatalf("BumpByValue mutated caller: ID=%d, want 5", b.ID)
	}
}

func TestBumpByPointerMutates(t *testing.T) {
	t.Parallel()
	b := filled()
	BumpByPointer(&b)
	if b.ID != 6 {
		t.Fatalf("BumpByPointer did not mutate caller: ID=%d, want 6", b.ID)
	}
}

func BenchmarkByValue(b *testing.B) {
	b.ReportAllocs()
	big := filled()
	var s int64
	for b.Loop() {
		s += SumByValue(big)
	}
	sink = s
}

func BenchmarkByPointer(b *testing.B) {
	b.ReportAllocs()
	big := filled()
	var s int64
	for b.Loop() {
		s += SumByPointer(&big)
	}
	sink = s
}
```

Run the benchmarks and read the escape decisions:

```bash
go test -bench=. -benchmem -run=^$
go build -gcflags=-m ./...
```

`BenchmarkByValue` copies the ~2 KB struct into the parameter on every iteration,
which `-benchmem` reflects; `BenchmarkByPointer` copies only the address. The
`-gcflags=-m` output shows the compiler's stack-vs-heap decisions — the place to
confirm that a pointer form has not silently forced a heap escape. Exact `ns/op`
depends on the machine.

## Review

The decision this module measures is correctness *and* cost, in that order.
`TestSumFormsAgree` confirms the two shapes compute the same answer, and the two
`Bump` tests pin the contract: the value form cannot mutate the caller, the pointer
form can — that alone often decides the signature before performance enters. When it
does come down to speed, the benchmark, not intuition, is the evidence: pass a large
struct by pointer to skip the copy, but verify with `-gcflags=-m` that you have not
traded a cheap stack copy for a heap allocation, and keep the result flowing into
`sink` so the compiler does not delete the work you are timing. For a small struct,
the copy is a few words and the value form keeps the cleaner immutability contract —
which loops back to the lesson's thesis: the pointer is a contract decision first, an
optimization second.

## Resources

- [Go Blog: Escape analysis / stack vs heap](https://go.dev/blog/escape-analysis) — how the compiler decides allocation, and why a returned pointer can escape.
- [`testing.B.Loop`](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop that keeps the measured call from being optimized away.
- [Go Wiki: Compiler And Runtime Optimizations](https://go.dev/wiki/CompilerOptimizations) — reading `-gcflags=-m` inlining and escape notes.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-sync-pool-buffer-pointers.md](08-sync-pool-buffer-pointers.md) | Next: [../03-new-vs-composite-literal/00-concepts.md](../03-new-vs-composite-literal/00-concepts.md)
