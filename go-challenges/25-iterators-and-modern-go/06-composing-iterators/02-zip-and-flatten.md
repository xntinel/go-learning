# Exercise 2: Zip And Flatten

The combinators in exercise 1 each took one sequence. This exercise builds the two that combine *several*: `Zip`, which advances two sequences in lockstep and yields pairs, and `Flatten`, which collapses a sequence of sequences into one flat stream. `Zip` is the combinator that forces you to confront a limitation of push iterators — a single `for range` drives only one sequence — and to reach for `iter.Pull` to drive the second by hand. Both stay lazy: `Zip` stops at the shorter input, and `Flatten` never constructs a sub-sequence past an early break.

This module is fully self-contained. It begins with its own `go mod init`, defines both combinators, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
combine.go           Zip (via iter.Pull) and Flatten (nested range)
cmd/
  demo/
    main.go          zip names with ages, then flatten a slice of slices
combine_test.go      Zip stops at shorter, Flatten output, Flatten early-break laziness
```

- Files: `combine.go`, `cmd/demo/main.go`, `combine_test.go`.
- Implement: `Zip[A, B](a iter.Seq[A], b iter.Seq[B]) iter.Seq2[A, B]` and `Flatten[V](seqs iter.Seq[iter.Seq[V]]) iter.Seq[V]`.
- Test: `Zip` stops at the shorter sequence; `Flatten` concatenates in order; breaking out of a `Flatten` range stops upstream so later sub-sequences are never produced.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/06-composing-iterators/02-zip-and-flatten/cmd/demo && cd go-solutions/25-iterators-and-modern-go/06-composing-iterators/02-zip-and-flatten
```

### Why Zip needs iter.Pull

A push sequence delivers its values by calling `yield` in its own loop; the consumer never asks for "the next one," it just reacts inside the `for range` body. That is fine for one sequence, but `Zip` has to advance two at the same rate, and you cannot put two sequences in one `for range`, nor nest them — nesting would run the inner one to completion for every single element of the outer. The two sequences need to step together, which means at least one of them must become *pullable*: something you can call to get exactly one value on demand.

`iter.Pull` is the standard-library bridge. Given an `iter.Seq[V]`, it returns `next func() (V, bool)` — call it to get the next value, with `ok == false` when the sequence is exhausted — and `stop func()`, which you must call to release it. The machinery suspends the pushed sequence between `next` calls (it runs on a goroutine under the hood), so `stop` is not optional: forgetting it leaks that goroutine. The idiom is `next, stop := iter.Pull(b); defer stop()`, and the `defer` is what makes the cleanup correct on every exit path — whether `a` runs out, `b` runs out, or the consumer breaks.

With `b` pulled by hand, `Zip` drives `a` with an ordinary `for range` and calls `next()` once per step. It stops the instant either side is done: if `next()` reports `ok == false`, `b` is shorter and `Zip` returns; if `a`'s loop ends, `a` is shorter and the loop simply finishes; and `if !yield(av, bv) { return }` handles a consumer that breaks early. Because each step produces two values, `Zip` returns an `iter.Seq2[A, B]`, and a consumer ranges over it with two variables.

`Flatten` is the easy one by comparison: two nested `for range` loops, with the stop check on the inner `yield`. The single subtlety is that a `return` inside the inner loop exits the whole `Flatten` function, and the runtime tears down *both* the inner and the outer range. So a consumer that breaks during the second sub-sequence causes the third to never be constructed at all — that is precisely the laziness the test pins down.

Create `combine.go`:

```go
package combine

import "iter"

// Zip advances a and b in lockstep, yielding pairs until either is exhausted.
// It drives a with a for range loop and pulls b by hand with iter.Pull, since a
// single for range can drive only one sequence.
func Zip[A, B any](a iter.Seq[A], b iter.Seq[B]) iter.Seq2[A, B] {
	return func(yield func(A, B) bool) {
		next, stop := iter.Pull(b)
		defer stop()
		for av := range a {
			bv, ok := next()
			if !ok {
				return // b is the shorter sequence
			}
			if !yield(av, bv) {
				return // the consumer stopped
			}
		}
	}
}

// Flatten concatenates a sequence of sequences into one flat sequence. A return
// inside the inner loop stops both ranges, so an early break upstream means
// later sub-sequences are never produced.
func Flatten[V any](seqs iter.Seq[iter.Seq[V]]) iter.Seq[V] {
	return func(yield func(V) bool) {
		for seq := range seqs {
			for v := range seq {
				if !yield(v) {
					return
				}
			}
		}
	}
}
```

### The runnable demo

The demo zips three names against two ages — `Zip` stops at the two ages, so the third name is dropped — then flattens a slice of three small slices into one stream. `slices.Values` turns a slice into an `iter.Seq`, and `slices.Collect` drives the flattened sequence back into a slice.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"iter"
	"slices"

	"example.com/combine"
)

func main() {
	names := slices.Values([]string{"alice", "bob", "carol"})
	ages := slices.Values([]int{30, 25})

	fmt.Println("zip (stops at the shorter sequence):")
	for name, age := range combine.Zip(names, ages) {
		fmt.Printf("  %s=%d\n", name, age)
	}

	groups := []iter.Seq[int]{
		slices.Values([]int{1, 2}),
		slices.Values([]int{3, 4}),
		slices.Values([]int{5, 6}),
	}
	flat := combine.Flatten(slices.Values(groups))
	fmt.Println("flatten:", slices.Collect(flat))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
zip (stops at the shorter sequence):
  alice=30
  bob=25
flatten: [1 2 3 4 5 6]
```

### Tests

`TestZipStopsAtShorter` confirms `Zip` ends with the shorter input and drops the extra. `TestFlatten` checks order is preserved across sub-sequences. `TestFlattenEarlyBreakIsLazy` is the laziness proof: each sub-sequence records every value it produces into a shared slice; the consumer breaks after the first value of the second sub-sequence, and the test asserts the recorded values stop right there — the third sub-sequence is never produced.

Create `combine_test.go`:

```go
package combine

import (
	"iter"
	"slices"
	"testing"
)

func TestZipStopsAtShorter(t *testing.T) {
	t.Parallel()

	letters := slices.Values([]string{"a", "b", "c", "d"})
	numbers := slices.Values([]int{1, 2})

	var keys []string
	var vals []int
	for k, v := range Zip(letters, numbers) {
		keys = append(keys, k)
		vals = append(vals, v)
	}
	if want := []string{"a", "b"}; !slices.Equal(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	if want := []int{1, 2}; !slices.Equal(vals, want) {
		t.Fatalf("vals = %v, want %v", vals, want)
	}
}

func TestFlatten(t *testing.T) {
	t.Parallel()

	groups := []iter.Seq[int]{
		slices.Values([]int{1, 2}),
		slices.Values([]int{}),
		slices.Values([]int{3}),
		slices.Values([]int{4, 5, 6}),
	}
	got := slices.Collect(Flatten(slices.Values(groups)))
	if want := []int{1, 2, 3, 4, 5, 6}; !slices.Equal(got, want) {
		t.Fatalf("Flatten = %v, want %v", got, want)
	}
}

func TestFlattenEarlyBreakIsLazy(t *testing.T) {
	t.Parallel()

	var produced []int
	gen := func(vals ...int) iter.Seq[int] {
		return func(yield func(int) bool) {
			for _, v := range vals {
				produced = append(produced, v)
				if !yield(v) {
					return
				}
			}
		}
	}

	groups := []iter.Seq[int]{gen(1, 2), gen(3, 4), gen(5, 6)}

	var got []int
	for v := range Flatten(slices.Values(groups)) {
		got = append(got, v)
		if v == 3 {
			break
		}
	}

	if want := []int{1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	// The break after 3 must stop the second sub-sequence and never start the
	// third: 4, 5, and 6 are never produced.
	if want := []int{1, 2, 3}; !slices.Equal(produced, want) {
		t.Fatalf("produced = %v, want %v (no work past the break)", produced, want)
	}
}
```

## Review

`Zip` is correct when it advances both sequences once per step and stops at the first to run dry. The mechanism that makes that possible is `iter.Pull`: `a` is driven by `for range`, `b` is pulled by `next()`, and `defer stop()` releases `b`'s suspended goroutine on every exit — `a` ending, `next()` returning `ok == false`, or the consumer breaking. `Flatten` is correct when its nested ranges preserve order and a `return` in the inner loop tears down both, which `TestFlattenEarlyBreakIsLazy` verifies by asserting that breaking after the value 3 leaves 4, 5, and 6 unproduced.

Common mistakes for this feature. The first is forgetting `defer stop()` after `iter.Pull`, which leaks the goroutine that holds the suspended sequence — always pair the pull with a deferred stop. The second is trying to write `Zip` with two nested `for range` loops, which runs the inner sequence to completion for each element of the outer instead of stepping them together; lockstep requires pulling one side. The third is a bare `yield(v)` in `Flatten`'s inner loop, which would keep producing from later sub-sequences after a consumer break and trip the runtime's "continued iteration" panic.

## Resources

- [`iter.Pull`](https://pkg.go.dev/iter#Pull) — converts a push sequence into a `next`/`stop` pull pair, the bridge `Zip` is built on, with the requirement to call `stop`.
- [`iter.Seq2`](https://pkg.go.dev/iter#Seq2) — the two-value sequence type `Zip` returns and that `for k, v := range` consumes.
- [Go Blog: Range Over Function Types](https://go.dev/blog/range-functions) — covers both push (`Seq`) and pull (`iter.Pull`) iteration and how stop propagates through nested ranges.
- [`slices.Values`](https://pkg.go.dev/slices#Values) — the source that turns a slice into an `iter.Seq`, used to feed both combinators in the demo and tests.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-core-combinators.md](01-core-combinators.md) | Next: [03-take-while-and-drop-while.md](03-take-while-and-drop-while.md)
