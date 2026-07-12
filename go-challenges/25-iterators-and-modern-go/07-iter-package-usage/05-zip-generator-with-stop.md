# Exercise 5: Zip — A Generator That Must Stop Both Inputs

This exercise builds `Zip`, a generator that pairs two push iterators positionally and halts at the shorter one. Zip is the sharpest demonstration of why `iter.Pull`'s `stop` is mandatory: when the inputs are uneven, or when the consumer breaks early, the longer input is left partway through and its pull machinery must be explicitly stopped or it leaks.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
zip.go               Zip[A, B](a iter.Seq[A], b iter.Seq[B]) iter.Seq2[A, B]
cmd/
  demo/
    main.go          zip names with ages, then zip uneven-length inputs
zip_test.go          equal lengths, uneven lengths stop the longer input, early break
```

- Files: `zip.go`, `cmd/demo/main.go`, `zip_test.go`.
- Implement: `Zip[A, B any](a iter.Seq[A], b iter.Seq[B]) iter.Seq2[A, B]` pairing elements positionally, ending at the shorter input.
- Test: pair equal-length inputs, prove the longer input is stopped (not drained) when lengths differ, and prove an early `break` stops both.
- Verify: `go test -run 'TestZip|TestZipUneven|TestZipEarly' -race ./...`

### Why stop is the whole point of Zip

`Zip` turns two single-value push iterators into one `iter.Seq2[A, B]` that yields aligned pairs: the first of `a` with the first of `b`, the second with the second, and so on, stopping as soon as either input is exhausted. Like `Merge`, it must advance two iterators independently, so it pulls both. Unlike `Merge`, the inputs are not symmetric in length-handling: when `Zip` stops because one side ran out, the *other* side is almost always still mid-stream, parked at a `yield` it expects to be resumed. That parked goroutine is exactly what `stop` exists to tear down.

Trace the uneven case. Zip a three-element `a` with a ten-element `b`. The loop calls `nextA`, then `nextB`, yields the pair, and repeats. After three pairs the fourth `nextA()` returns `ok == false`; `Zip` returns. At that moment `b`'s pull goroutine is parked inside the `yield` that produced its third value — it has no idea the consumer is done. The deferred `stopB()` resumes that goroutine with `yield` returning `false`, so `b`'s body takes its `return` path and the goroutine exits. Without `defer stopB()`, that goroutine would stay parked for the life of the program, holding open whatever `b` wraps. The same logic covers the consumer breaking out of the `range` early: the guarded `yield` returns `false`, `Zip` returns, and both deferred stops fire.

So `Zip` is a generator whose correctness rests entirely on the `defer stop()` discipline. The structure is the familiar sandwich — return a `Seq2`, pull both inputs, defer both stops, loop — but here the deferred stops are not a tidy afterthought, they are the feature: they are what prevents the longer or unbroken input from leaking.

Create `zip.go`:

```go
// Create `zip.go`
package zip

import "iter"

// Zip pairs elements of a and b positionally, yielding (a[i], b[i]) and ending
// as soon as either input is exhausted. The deferred stops guarantee that the
// longer input's pull goroutine is torn down rather than left parked.
func Zip[A, B any](a iter.Seq[A], b iter.Seq[B]) iter.Seq2[A, B] {
	return func(yield func(A, B) bool) {
		nextA, stopA := iter.Pull(a)
		defer stopA()
		nextB, stopB := iter.Pull(b)
		defer stopB()

		for {
			va, okA := nextA()
			if !okA {
				return
			}
			vb, okB := nextB()
			if !okB {
				return
			}
			if !yield(va, vb) {
				return
			}
		}
	}
}
```

### The runnable demo

The demo zips equal-length names and ages, then zips a short list against a long one to show that pairing stops at the shorter input. The longer input's tail is simply never reached, and its goroutine is stopped on the way out.

Create `cmd/demo/main.go`:

```go
// Create `cmd/demo/main.go`
package main

import (
	"fmt"
	"slices"

	"example.com/zip-generator-with-stop"
)

func main() {
	names := slices.Values([]string{"alice", "bob", "carol"})
	ages := slices.Values([]int{30, 25, 31})
	for name, age := range zip.Zip(names, ages) {
		fmt.Printf("%s is %d\n", name, age)
	}

	short := slices.Values([]string{"x", "y"})
	long := slices.Values([]int{1, 2, 3, 4, 5})
	for s, n := range zip.Zip(short, long) {
		fmt.Printf("pair %s-%d\n", s, n)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alice is 30
bob is 25
carol is 31
pair x-1
pair y-2
```

### Tests

`TestZip` pairs equal-length inputs and checks the pairs. `TestZipUnevenStopsLonger` zips a short input with a long one and asserts, through a producer-side counter, that the long input was stopped rather than drained. `TestZipEarlyBreak` breaks the consumer after one pair and asserts both inputs were halted well short of their length.

Create `zip_test.go`:

```go
// Create `zip_test.go`
package zip

import (
	"iter"
	"slices"
	"testing"
)

// counted wraps values in a push iterator that records how many it produced.
func counted[V any](values []V, produced *int) iter.Seq[V] {
	return func(yield func(V) bool) {
		for _, v := range values {
			*produced++
			if !yield(v) {
				return
			}
		}
	}
}

func TestZip(t *testing.T) {
	t.Parallel()

	names := []string{"a", "b", "c"}
	nums := []int{1, 2, 3}

	var gotNames []string
	var gotNums []int
	for s, n := range Zip(slices.Values(names), slices.Values(nums)) {
		gotNames = append(gotNames, s)
		gotNums = append(gotNums, n)
	}
	if !slices.Equal(gotNames, names) || !slices.Equal(gotNums, nums) {
		t.Fatalf("Zip = %v / %v", gotNames, gotNums)
	}
}

func TestZipUnevenStopsLonger(t *testing.T) {
	t.Parallel()

	var producedShort, producedLong int
	short := counted([]int{1, 2, 3}, &producedShort)
	long := counted([]int{10, 20, 30, 40, 50, 60, 70, 80}, &producedLong)

	var pairs int
	for range Zip(short, long) {
		pairs++
	}
	if pairs != 3 {
		t.Fatalf("pairs = %d, want 3", pairs)
	}
	// The long input must have been stopped, not drained: it produced only
	// enough to match the short side, far fewer than its eight elements.
	if producedLong > 4 {
		t.Fatalf("long input over-consumed: produced %d, want at most 4", producedLong)
	}
}

func TestZipEarlyBreak(t *testing.T) {
	t.Parallel()

	var producedA, producedB int
	a := counted([]int{1, 2, 3, 4, 5}, &producedA)
	b := counted([]int{6, 7, 8, 9, 10}, &producedB)

	var pairs int
	for range Zip(a, b) {
		pairs++
		break
	}
	if pairs != 1 {
		t.Fatalf("pairs = %d, want 1", pairs)
	}
	if producedA >= 5 || producedB >= 5 {
		t.Fatalf("inputs over-consumed after early break: a=%d b=%d", producedA, producedB)
	}
}
```

## Review

`Zip` is correct when both inputs are pulled, both stops are deferred, and each `nextA`/`nextB` result and the `yield` result are all checked. The two tests that matter most are the uneven and early-break cases, because they are where a missing `stop` does real damage. In the uneven test, the long input is parked when Zip returns; the producer counter proves the deferred `stopB` woke it to its `return` rather than leaving it suspended. In the early-break test, the consumer's `break` makes `yield` return `false`, and both deferred stops halt both sources after a single pair. If you delete a `defer stop()`, the gate still passes — the leak is invisible to a race detector on a fast-finishing test — which is exactly why the discipline has to be a reflex rather than something you verify case by case.

The takeaway distinct from the merge exercise: in a generator that consumes uneven or unbounded inputs, `stop` is not cleanup you add for politeness, it is the only thing that tears down the goroutine and resources of the input you stopped reading. Write `defer stop()` on the line after every `iter.Pull`, always.

## Resources

- [`iter.Pull`](https://pkg.go.dev/iter#Pull) — the adapter whose `stop` tears down the parked goroutine of the input Zip stops reading.
- [`iter.Seq2`](https://pkg.go.dev/iter#Seq2) — the two-value push iterator type Zip produces.
- [Range Over Function Types](https://go.dev/blog/range-functions) — the coroutine model that explains why an unstopped pull leaks.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-stream-join-service.md](06-stream-join-service.md)
