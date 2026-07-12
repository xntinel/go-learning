# Exercise 1: Seq and Seq2 Producers

A push iterator is a function that drives its own loop and pushes each value into a `yield` callback you give it. This exercise builds two producers — a single-value `iter.Seq[int]` of primes and a key/value `iter.Seq2[int, V]` of indexed elements — and pins down the one rule that makes them correct: honoring the boolean that `yield` returns.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
producers.go         PrimesUnder (iter.Seq[int]), Indexed (iter.Seq2[int, V]), isPrime
cmd/
  demo/
    main.go          range over a Seq and a Seq2, then break early to prove yield stops it
producers_test.go    full-range collection, early-break honored, validation error
```

- Files: `producers.go`, `cmd/demo/main.go`, `producers_test.go`.
- Implement: `PrimesUnder(limit int) (iter.Seq[int], error)` and `Indexed[V any](values []V) iter.Seq2[int, V]`.
- Test: collect a full range, break out of a range early and assert the iterator stopped, and assert `PrimesUnder(-1)` returns `ErrNegativeLimit`.
- Verify: `go test -run 'TestPrimes|TestIndexed|TestEarly|TestValidation' -race ./...`

### The yield protocol and why the boolean is not optional

An `iter.Seq[V]` is the named type `func(yield func(V) bool)`. A producer does not return values one at a time; it receives a `yield` callback and calls it once per element. When you write `for n := range PrimesUnder(30)`, the compiler turns the loop body into that `yield` function and hands it to the iterator, which then pushes primes into it. The inversion is the whole point: the iterator owns the loop, the consumer owns the body.

The single rule that separates a correct producer from a buggy one is the value `yield` returns. `yield(v)` returns `true` to mean "keep going" and `false` to mean "the consumer is finished — stop now." A consumer signals `false` by breaking out of its range loop, returning from the enclosing function, or panicking. The producer must check that result on every call and return immediately when it sees `false`. The idiom `if isPrime(n) && !yield(n) { return }` reads as "if this is a prime and the consumer rejected it, stop." Ignoring the result is not a harmless inefficiency: continuing to call `yield` after it returned `false` makes the runtime panic with "range function continued iteration after function for loop body returned false." Honoring the boolean is what makes `break` inside a `range` actually break.

`Indexed` is the `Seq2` version: its `yield` takes two arguments, the index and the value, mirroring what `slices.All` returns. The same boolean rule applies — `if !yield(i, v) { return }` — because a consumer can break out of a two-variable range just as easily as a one-variable one.

`PrimesUnder` returns an error as well as an iterator so the caller can reject a nonsensical limit before any iteration begins. Validation that belongs to constructing the iterator happens eagerly, at call time; only the actual enumeration is deferred into the returned closure.

Create `producers.go`:

```go
// Create `producers.go`
package producers

import (
	"errors"
	"fmt"
	"iter"
)

// ErrNegativeLimit is returned when a limit argument is negative.
var ErrNegativeLimit = errors.New("limit must not be negative")

// PrimesUnder returns a push iterator over every prime strictly less than limit,
// in increasing order. A negative limit is rejected with ErrNegativeLimit.
func PrimesUnder(limit int) (iter.Seq[int], error) {
	if limit < 0 {
		return nil, fmt.Errorf("primes under %d: %w", limit, ErrNegativeLimit)
	}
	return func(yield func(int) bool) {
		for n := 2; n < limit; n++ {
			if isPrime(n) && !yield(n) {
				return
			}
		}
	}, nil
}

// Indexed returns a push iterator that yields each element of values together
// with its index, mirroring the shape of slices.All.
func Indexed[V any](values []V) iter.Seq2[int, V] {
	return func(yield func(int, V) bool) {
		for i, v := range values {
			if !yield(i, v) {
				return
			}
		}
	}
}

func isPrime(n int) bool {
	if n < 2 {
		return false
	}
	for d := 2; d*d <= n; d++ {
		if n%d == 0 {
			return false
		}
	}
	return true
}
```

### The runnable demo

The demo ranges over `PrimesUnder` to show ordinary consumption, ranges over `Indexed` to show the key/value shape, then breaks out of a prime loop after three values to prove the early-stop path works end to end.

Create `cmd/demo/main.go`:

```go
// Create `cmd/demo/main.go`
package main

import (
	"fmt"

	"example.com/seq-and-seq2-producers"
)

func main() {
	seq, err := producers.PrimesUnder(20)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	var all []int
	for p := range seq {
		all = append(all, p)
	}
	fmt.Println("primes under 20:", all)

	for i, name := range producers.Indexed([]string{"alice", "bob", "carol"}) {
		fmt.Printf("  [%d] %s\n", i, name)
	}

	var first []int
	for p := range seq {
		first = append(first, p)
		if len(first) == 3 {
			break
		}
	}
	fmt.Println("first three primes:", first)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
primes under 20: [2 3 5 7 11 13 17 19]
  [0] alice
  [1] bob
  [2] carol
first three primes: [2 3 5]
```

### Tests

`TestPrimesUnder` collects the full range and checks the exact prime sequence. `TestIndexed` checks both the indices and the values. `TestEarlyBreak` ranges over the prime iterator, breaks after two values, and asserts the loop stopped — the iterator honored the `false` return. `TestValidation` asserts `PrimesUnder(-1)` reports `ErrNegativeLimit`.

Create `producers_test.go`:

```go
// Create `producers_test.go`
package producers

import (
	"errors"
	"slices"
	"testing"
)

func TestPrimesUnder(t *testing.T) {
	t.Parallel()

	seq, err := PrimesUnder(30)
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Collect(seq)
	want := []int{2, 3, 5, 7, 11, 13, 17, 19, 23, 29}
	if !slices.Equal(got, want) {
		t.Fatalf("PrimesUnder(30) = %v, want %v", got, want)
	}
}

func TestIndexed(t *testing.T) {
	t.Parallel()

	var keys []int
	var vals []string
	for i, v := range Indexed([]string{"a", "b", "c"}) {
		keys = append(keys, i)
		vals = append(vals, v)
	}
	if !slices.Equal(keys, []int{0, 1, 2}) {
		t.Fatalf("keys = %v", keys)
	}
	if !slices.Equal(vals, []string{"a", "b", "c"}) {
		t.Fatalf("vals = %v", vals)
	}
}

func TestEarlyBreak(t *testing.T) {
	t.Parallel()

	seq, err := PrimesUnder(1000)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for p := range seq {
		got = append(got, p)
		if len(got) == 2 {
			break
		}
	}
	if !slices.Equal(got, []int{2, 3}) {
		t.Fatalf("early break collected %v, want [2 3]", got)
	}
}

func TestValidation(t *testing.T) {
	t.Parallel()

	if _, err := PrimesUnder(-1); !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("PrimesUnder(-1) error = %v, want ErrNegativeLimit", err)
	}
}
```

## Review

The producer is correct when every loop in its body returns the instant `yield` reports `false`. Read `PrimesUnder` and `Indexed` as the same shape: enumerate, and on each element either keep going because `yield` returned `true` or stop because it returned `false`. `TestEarlyBreak` is the test that matters most here — it proves the `break` in the consumer reaches the iterator. If you drop the `&& !yield(n)` check and instead call `yield(n)` while discarding its result, the full-range tests still pass but a consumer that breaks early will eventually trigger the runtime's "continued iteration after false" panic; the early-break test is what catches that.

Two further points. Validation is eager: `PrimesUnder` checks its argument and returns an error before building the closure, so a bad limit fails at the call site, not midway through a range. And the named types earn their keep at the boundary — returning `iter.Seq[int]` and `iter.Seq2[int, V]` rather than the raw `func(func(int) bool)` is what lets `slices.Collect` accept the result directly in the test.

## Resources

- [`iter` package](https://pkg.go.dev/iter) — the definitions of `Seq` and `Seq2` and the contract their `yield` callbacks follow.
- [Range Over Function Types](https://go.dev/blog/range-functions) — the Go blog post introducing push iterators and the meaning of the boolean `yield` returns.
- [`slices.Collect`](https://pkg.go.dev/slices#Collect) — drains an `iter.Seq` into a fresh slice, used throughout the tests.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-pull-adapters.md](02-pull-adapters.md)
