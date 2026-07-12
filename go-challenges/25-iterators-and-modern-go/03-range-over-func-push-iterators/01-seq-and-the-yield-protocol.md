# Exercise 1: iter.Seq and the Yield Protocol

A push iterator is an ordinary function that hands each value to a `yield`
callback, and `for v := range seq` walks it. This exercise builds two
single-value generators -- a finite `Countdown` and a `Fibonacci` prefix -- and
nails the one rule that makes the whole mechanism work: when `yield` returns
false, the iterator must stop.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
seq.go               iter.Seq generators: Countdown, Fibonacci, ErrNegative
cmd/
  demo/
    main.go          run a countdown, break out of an infinite-style stream
seq_test.go          full-range collection, early-break prefix, negative-input errors
```

- Files: `seq.go`, `cmd/demo/main.go`, `seq_test.go`.
- Implement: `Countdown(n int) (iter.Seq[int], error)` and `Fibonacci(limit int)
  (iter.Seq[int], error)`, both validating their input and both honoring the
  yield protocol; a sentinel `ErrNegative`.
- Test: `seq_test.go` collects full sequences, breaks early and asserts the exact
  prefix, and asserts a negative argument returns `ErrNegative`.
- Verify: `go test -run TestSeq -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/03-range-over-func-push-iterators/01-seq-and-the-yield-protocol/cmd/demo && cd go-solutions/25-iterators-and-modern-go/03-range-over-func-push-iterators/01-seq-and-the-yield-protocol
```

### What iter.Seq actually is, and what yield's bool means

`iter.Seq[V]` is nothing more than a named generic function type:
`func(yield func(V) bool)`. A value of that type is an iterator. To produce a
sequence you write a function that, when called with some `yield`, loops over
your data and calls `yield(v)` once per value. The compiler does the inverse:
when you write `for v := range seq`, it synthesizes a `yield` whose body is your
loop body and calls `seq(yield)`. So the producer's loop drives the consumer's
loop body -- the iterator pushes.

The contract that ties the two together is `yield`'s return value. `yield(v)`
returns `true` when the consumer wants to keep going and `false` when it does
not -- because the loop body ran a `break`, a `return`, or otherwise left the
loop. The iterator must check that bool on every single call and return as soon
as it sees `false`:

```go
return func(yield func(int) bool) {
	for i := n; i >= 1; i-- {
		if !yield(i) {
			return
		}
	}
}
```

That `if !yield(i) { return }` is the entire protocol. Drop it and two things go
wrong: a caller's `break` no longer stops the producer, and if the iterator
keeps calling `yield` after it returned `false`, the loop machinery the compiler
generates panics at run time. The same rule is what lets `Fibonacci` be written
as an unbounded generator bounded only by `limit` and by the consumer -- the
consumer can `break` after the value it wants and the generator stops on the next
yield.

### Validating eagerly, returning an error before the loop

Both generators take an `int` that must not be negative. An iterator is lazy --
its body does not run until `for range` calls it -- so a negative argument would
otherwise surface in the middle of someone's loop, where a push iterator has no
way to report an error. The idiom is to validate in the constructor and return
`(iter.Seq[int], error)`: the caller handles the error up front, and once the
loop starts the inputs are already known good. `errors.Is(err, ErrNegative)`
gives callers a stable sentinel to test against, and `%w` wraps it with the
offending value for a useful message.

Create `seq.go`:

```go
// Package seqyield builds single-value push iterators (iter.Seq[int]) and
// demonstrates the yield-returns-false termination protocol.
package seqyield

import (
	"errors"
	"fmt"
	"iter"
)

// ErrNegative is returned by a generator given a negative bound.
var ErrNegative = errors.New("bound must not be negative")

// Countdown returns an iterator over n, n-1, ..., 1. A bound of 0 yields the
// empty sequence; a negative bound is a validation error.
func Countdown(n int) (iter.Seq[int], error) {
	if n < 0 {
		return nil, fmt.Errorf("countdown %d: %w", n, ErrNegative)
	}
	return func(yield func(int) bool) {
		for i := n; i >= 1; i-- {
			if !yield(i) {
				return
			}
		}
	}, nil
}

// Fibonacci returns an iterator over the first `limit` Fibonacci numbers
// starting at 0, 1, 1, 2, 3, 5, .... It is written as an unbounded generator
// capped by `limit` and by the consumer: a caller that breaks early stops the
// generator on the next yield.
func Fibonacci(limit int) (iter.Seq[int], error) {
	if limit < 0 {
		return nil, fmt.Errorf("fibonacci %d: %w", limit, ErrNegative)
	}
	return func(yield func(int) bool) {
		a, b := 0, 1
		for range limit {
			if !yield(a) {
				return
			}
			a, b = b, a+b
		}
	}, nil
}
```

Read each generator as "produce a value, then ask permission to continue".
`Countdown` walks `i` from `n` down to `1`; `Fibonacci` keeps the rolling pair
`(a, b)` and advances it after each accepted value. Neither ever calls `yield`
again once it returned `false`, which is what makes them safe to `break` out of.

### The runnable demo

The demo shows both halves of the protocol. First it runs a `Countdown` to
completion, where the iterator decides when to stop. Then it ranges a long
`Fibonacci` but `break`s as soon as a value reaches 100, where the consumer
decides -- and the generator stops cleanly on the next yield rather than running
all the way to `limit`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/seq-yield"
)

func main() {
	down, err := seqyield.Countdown(5)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print("countdown:")
	for v := range down {
		fmt.Printf(" %d", v)
	}
	fmt.Println()

	fib, err := seqyield.Fibonacci(100)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print("fib < 100:")
	for v := range fib {
		if v >= 100 {
			break
		}
		fmt.Printf(" %d", v)
	}
	fmt.Println()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
countdown: 5 4 3 2 1
fib < 100: 0 1 1 2 3 5 8 13 21 34 55 89
```

### Tests

The tests pin the three properties that matter. `TestSeqFullRange` collects an
entire `Countdown` and an entire `Fibonacci` and checks every value. `TestSeqEarlyBreak`
breaks out of a long `Countdown` after three values and asserts the collected
prefix is exactly `[10 9 8]` -- this is the test that fails if the iterator
ignores the `false` from `yield`. `TestSeqNegative` asserts both constructors
reject a negative bound with `ErrNegative`.

Create `seq_test.go`:

```go
package seqyield

import (
	"errors"
	"slices"
	"testing"
)

func collect(seq func(func(int) bool)) []int {
	var out []int
	for v := range seq {
		out = append(out, v)
	}
	return out
}

func TestSeqFullRange(t *testing.T) {
	t.Parallel()

	down, err := Countdown(5)
	if err != nil {
		t.Fatalf("Countdown: %v", err)
	}
	if got, want := collect(down), []int{5, 4, 3, 2, 1}; !slices.Equal(got, want) {
		t.Fatalf("countdown = %v, want %v", got, want)
	}

	fib, err := Fibonacci(8)
	if err != nil {
		t.Fatalf("Fibonacci: %v", err)
	}
	if got, want := collect(fib), []int{0, 1, 1, 2, 3, 5, 8, 13}; !slices.Equal(got, want) {
		t.Fatalf("fibonacci = %v, want %v", got, want)
	}
}

func TestSeqEarlyBreak(t *testing.T) {
	t.Parallel()

	down, err := Countdown(10)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for v := range down {
		if len(got) == 3 {
			break
		}
		got = append(got, v)
	}
	if want := []int{10, 9, 8}; !slices.Equal(got, want) {
		t.Fatalf("early break = %v, want %v", got, want)
	}
}

func TestSeqNegative(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		call func() error
	}{
		{name: "countdown", call: func() error { _, err := Countdown(-1); return err }},
		{name: "fibonacci", call: func() error { _, err := Fibonacci(-3); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.call(); !errors.Is(err, ErrNegative) {
				t.Fatalf("%s err = %v, want ErrNegative", tc.name, err)
			}
		})
	}
}
```

## Review

The iterators are correct when `break` in the caller actually stops the
producer. That property lives entirely in `if !yield(v) { return }`: drop the
`return` and `TestSeqEarlyBreak` fails (the iterator keeps producing) or the run
panics (the iterator calls `yield` after it returned `false`). Confirm both
generators validate their bound before returning the iterator, not inside it, so
a negative argument is an ordinary error the caller handles before the loop
rather than a silent early stop. The full-range, early-break, and negative-input
cases passing together under `go test -race ./...` establish the protocol is
honored.

Common mistakes for this feature. The first is treating `yield` as fire-and-
forget -- calling it for its side effect and looping on regardless of the bool;
this is the bug `TestSeqEarlyBreak` exists to catch. The second is validating
lazily: putting the negative-bound check inside the returned closure, where the
error has nowhere to go and the loop just ends early with no explanation; validate
in the constructor and return `(iter.Seq[int], error)`. The third is reaching
for a goroutine and channel to make `Fibonacci` "infinite"; the bool protocol
already makes an unbounded generator safe to break out of, with no goroutine to
leak.

## Resources

- [`iter` package](https://pkg.go.dev/iter) -- the standard definitions of
  `Seq[V]` and `Seq2[K, V]` and the documented yield contract.
- [Go Blog: Range Over Function Types](https://go.dev/blog/range-functions) --
  the design walkthrough of push iterators and why `yield` returns a bool.
- [Go Spec: For statements with range clause](https://go.dev/ref/spec#For_range)
  -- the exact rules for ranging over a function value.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-seq2-key-value-iterators.md](02-seq2-key-value-iterators.md)
