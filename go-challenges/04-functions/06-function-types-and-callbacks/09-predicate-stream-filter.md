# Exercise 9: Predicate/Transform Combinators Over an iter.Seq Stream

Go 1.23's range-over-func makes streaming combinators first-class: `Filter`, `Map`, and
`TakeWhile` over `iter.Seq[T]`, composed lazily and cancellable by a consumer's `break`.
This module builds those combinators and applies them to a realistic log scan — filter
by level, project a field, stop after N — proving the yield-returns-false contract that
lets a downstream `break` stop upstream work.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
stream/                     independent module: example.com/stream
  go.mod                    go 1.26
  stream.go                 Predicate[T]; Filter, Map, TakeWhile over iter.Seq
  cmd/
    demo/
      main.go               runnable demo: filter+map+take over log records
  stream_test.go            transform, take-boundary, early-break, laziness tests
```

Files: `stream.go`, `cmd/demo/main.go`, `stream_test.go`.
Implement: `type Predicate[T any] func(T) bool`, `Filter[T](iter.Seq[T], Predicate[T]) iter.Seq[T]`, `Map[A,B](iter.Seq[A], func(A) B) iter.Seq[B]`, and `TakeWhile[T](iter.Seq[T], Predicate[T]) iter.Seq[T]`, each honoring the yield-false contract.
Test: `Filter`+`Map`+`slices.Collect` yields the transformed slice; `TakeWhile` stops at the boundary; a consumer that breaks after one element stops the source (counted side effect); combinators are lazy (no work until ranged).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/stream/cmd/demo
cd ~/go-exercises/stream
go mod init example.com/stream
```

### The yield contract is the whole thing

An `iter.Seq[T]` is `func(yield func(T) bool)`. A producer calls `yield` once per
element; the moment `yield` returns `false`, the producer *must stop* and return. That
single rule is what makes streaming composable and cancellable: when a consumer `break`s
out of a `for v := range seq` loop, the runtime causes the pending `yield` to return
`false`, and a correctly-written producer sees that and stops. If your combinator ignores
the `false` and keeps producing, you break the contract — the runtime panics on a second
`yield` after `false`, and even short of that you do wasted work the consumer explicitly
declined.

Each combinator is a producer that consumes an upstream `iter.Seq` and yields a
transformed stream, threading the `false` back upstream:

- `Filter` ranges the source, and for each element that satisfies the predicate, forwards
  it to the downstream `yield`; if downstream returns `false`, `Filter` returns from its
  range (which stops the source).
- `Map` forwards every element transformed by a function, again returning if downstream
  says stop.
- `TakeWhile` yields while the predicate holds and returns the instant it fails — which
  both stops taking and stops the source.

The canonical shape for propagating the stop is `if !yield(x) { return false }` inside the
source's callback, so the source's own range loop terminates. Laziness falls out for free:
a combinator does no work until someone ranges the returned sequence, because all the work
lives inside the `func(yield ...)` body, which only runs when the consumer calls it.

The realistic application: scan a stream of log records, keep only `ERROR` level, project
the message field, and take the first two. Because everything is lazy and stop-propagating,
the pipeline reads exactly as many source records as it needs and no more.

Create `stream.go`:

```go
package stream

import "iter"

// Predicate reports whether an element satisfies a condition.
type Predicate[T any] func(T) bool

// Filter yields only the elements of seq that satisfy keep. It stops the source
// as soon as the downstream consumer stops (yield returns false).
func Filter[T any](seq iter.Seq[T], keep Predicate[T]) iter.Seq[T] {
	return func(yield func(T) bool) {
		for v := range seq {
			if keep(v) {
				if !yield(v) {
					return
				}
			}
		}
	}
}

// Map yields each element of seq transformed by fn.
func Map[A, B any](seq iter.Seq[A], fn func(A) B) iter.Seq[B] {
	return func(yield func(B) bool) {
		for v := range seq {
			if !yield(fn(v)) {
				return
			}
		}
	}
}

// TakeWhile yields elements while pred holds, stopping (and stopping the source)
// at the first element that fails the predicate.
func TakeWhile[T any](seq iter.Seq[T], pred Predicate[T]) iter.Seq[T] {
	return func(yield func(T) bool) {
		for v := range seq {
			if !pred(v) {
				return
			}
			if !yield(v) {
				return
			}
		}
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"

	"example.com/stream"
)

type record struct {
	Level string
	Msg   string
}

func main() {
	logs := []record{
		{"INFO", "started"},
		{"ERROR", "disk full"},
		{"WARN", "slow query"},
		{"ERROR", "oom killed"},
		{"ERROR", "panic recovered"},
	}

	errorsOnly := stream.Filter(slices.Values(logs), func(r record) bool {
		return r.Level == "ERROR"
	})
	messages := stream.Map(errorsOnly, func(r record) string { return r.Msg })

	// Take the first two by breaking; the break stops all upstream work.
	seen := 0
	for msg := range messages {
		fmt.Println("error:", msg)
		seen++
		if seen == 2 {
			break
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
error: disk full
error: oom killed
```

### Tests

Create `stream_test.go`:

```go
package stream

import (
	"fmt"
	"slices"
	"testing"
)

func TestFilterMapCollect(t *testing.T) {
	t.Parallel()
	src := slices.Values([]int{1, 2, 3, 4, 5, 6})
	evens := Filter(src, func(n int) bool { return n%2 == 0 })
	doubled := Map(evens, func(n int) int { return n * 10 })
	got := slices.Collect(doubled)
	if want := []int{20, 40, 60}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTakeWhileStopsAtBoundary(t *testing.T) {
	t.Parallel()
	src := slices.Values([]int{2, 4, 6, 7, 8})
	taken := TakeWhile(src, func(n int) bool { return n%2 == 0 })
	got := slices.Collect(taken)
	if want := []int{2, 4, 6}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v (must stop at first odd)", got, want)
	}
}

// TestEarlyBreakStopsSource proves the yield=false contract: breaking out of the
// consumer loop stops the source, so the counted side effect stops too.
func TestEarlyBreakStopsSource(t *testing.T) {
	t.Parallel()
	produced := 0
	// A source that counts how many elements it actually yields.
	src := func(yield func(int) bool) {
		for i := range 1000 {
			produced++
			if !yield(i) {
				return
			}
		}
	}
	filtered := Filter(src, func(int) bool { return true })

	seen := 0
	for range filtered {
		seen++
		if seen == 1 {
			break // stop after the first element
		}
	}
	if seen != 1 {
		t.Fatalf("consumer saw %d, want 1", seen)
	}
	if produced != 1 {
		t.Fatalf("source produced %d elements; want 1 (break must stop upstream)", produced)
	}
}

// TestLaziness proves no work happens until the sequence is ranged.
func TestLaziness(t *testing.T) {
	t.Parallel()
	touched := 0
	src := func(yield func(int) bool) {
		for i := range 5 {
			touched++
			if !yield(i) {
				return
			}
		}
	}
	// Build the pipeline but do not range it.
	pipeline := Map(Filter(src, func(int) bool { return true }), func(n int) int { return n })
	if touched != 0 {
		t.Fatalf("touched = %d before ranging; combinators must be lazy", touched)
	}
	_ = slices.Collect(pipeline) // now it runs
	if touched != 5 {
		t.Fatalf("touched = %d after collect, want 5", touched)
	}
}

func ExampleFilter() {
	src := slices.Values([]int{1, 2, 3, 4})
	odds := Filter(src, func(n int) bool { return n%2 == 1 })
	fmt.Println(slices.Collect(odds))
	// Output: [1 3]
}
```

## Review

The combinators are correct when they honor two properties. Stop-propagation: a consumer
that `break`s must halt the source. `TestEarlyBreakStopsSource` counts source production
and asserts it stops at one element — if `Filter` ignored the downstream `false`, the
source would run all 1000 iterations and the test would catch it. Laziness: building
`Map(Filter(...))` must touch nothing until the result is ranged, because all work lives
inside the `func(yield ...)` body; `TestLaziness` asserts the source counter is zero
before `slices.Collect` and five after. The canonical `if !yield(v) { return }` inside
each combinator's range loop is what wires the two together — omit the `return` and you
both waste work and risk a runtime panic from yielding after `false`. `slices.Values`
turns a slice into a source and `slices.Collect` drains a sequence into a slice, so the
combinators plug into the standard `iter` ecosystem.

## Resources

- [iter package: Seq and range-over-func](https://pkg.go.dev/iter)
- [slices.Values and slices.Collect](https://pkg.go.dev/slices#Values)
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions)
- [Go 1.23 release notes: iterators](https://go.dev/doc/go1.23#iterators)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-memoize-and-lazy-init.md](08-memoize-and-lazy-init.md) | Next: [10-shutdown-hook-stack.md](10-shutdown-hook-stack.md)
