# Exercise 1: Bridge a Push Iterator to Pull

A push iterator drives its own loop; sometimes you need the opposite — to ask for the next value yourself, take a bounded number, and stop. This exercise builds `PullN`, a helper that consumes at most `n` values from any `iter.Seq` using `iter.Pull`, and proves the one rule that makes pull safe: `stop` must run even when you take fewer values than the sequence holds.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
pulliter.go          FromSlice, PullN, ErrNegativeLimit (iter.Pull + defer stop)
cmd/
  demo/
    main.go          pull 4 values from an infinite producer, show it was released
pulliter_test.go     bounded pull, negative-limit rejection, stop unwinds producer
```

- Files: `pulliter.go`, `cmd/demo/main.go`, `pulliter_test.go`.
- Implement: `FromSlice[T]`, `PullN[T](seq iter.Seq[T], n int) ([]T, error)`, and `ErrNegativeLimit`.
- Test: `pulliter_test.go` checks bounded pull against a short sequence, rejects a negative limit, and asserts `stop` runs the producer's deferred cleanup.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/04-range-over-func-pull-iterators/01-bridge-push-to-pull/cmd/demo && cd go-solutions/25-iterators-and-modern-go/04-range-over-func-pull-iterators/01-bridge-push-to-pull
```

### Why `PullN` is a pull iterator, and why `defer stop()` comes first

`iter.Pull(seq)` returns `(next func() (T, bool), stop func())`. The whole point of `PullN` is that it does not drain `seq`; it takes at most `n` values and then walks away. That is precisely the case where forgetting `stop` bites. Under the hood, `iter.Pull` is running `seq`'s push loop on a separate goroutine, parked inside `yield`. If `PullN` returns after pulling three of a thousand values, that goroutine is still parked, holding whatever the producer deferred. Calling `stop` resumes it one last time with `yield` returning `false`, so the producer's loop exits and its `defer`s run. Writing `defer stop()` on the line right after `iter.Pull` makes that fire on every exit path — the normal return, the early `break` inside the loop when the sequence ends, and any panic.

The bound itself is a plain `for range n` (Go's integer range), pulling one value per iteration and breaking the moment `next` reports `ok == false`, so asking for more than the sequence holds returns everything and stops cleanly. A negative `n` is a programming error rather than an empty result, so `PullN` rejects it with a sentinel error you can match with `errors.Is`, instead of silently returning nothing.

Create `pulliter.go`:

```go
package pulliter

import (
	"errors"
	"fmt"
	"iter"
)

// ErrNegativeLimit is returned by PullN when the requested count is negative.
var ErrNegativeLimit = errors.New("pulliter: limit must not be negative")

// FromSlice returns a push iterator (iter.Seq) that yields each element of
// values in order.
func FromSlice[T any](values []T) iter.Seq[T] {
	return func(yield func(T) bool) {
		for _, v := range values {
			if !yield(v) {
				return
			}
		}
	}
}

// PullN consumes at most n values from seq and returns them. It converts the
// push iterator to a pull iterator with iter.Pull and always releases it with
// stop, even when it takes fewer than n values. A negative n is rejected with
// ErrNegativeLimit.
func PullN[T any](seq iter.Seq[T], n int) ([]T, error) {
	if n < 0 {
		return nil, fmt.Errorf("pull %d: %w", n, ErrNegativeLimit)
	}

	next, stop := iter.Pull(seq)
	defer stop()

	out := make([]T, 0, n)
	for range n {
		v, ok := next()
		if !ok {
			break
		}
		out = append(out, v)
	}
	return out, nil
}
```

`next` and `stop` are the two halves of one pull iterator: `next` advances it, `stop` tears it down. They share single-goroutine state, so `PullN` only ever touches them from its own goroutine. The returned slice is freshly allocated with `make`, so a caller that asked for zero values gets a non-nil empty slice rather than `nil`, and the result never aliases the producer's storage.

### The runnable demo

The demo makes the `stop` guarantee visible. It builds an *infinite* push iterator — a loop with no upper bound — that increments a counter in a `defer` when its loop finally exits. `PullN` takes four values and returns. Because `PullN` deferred `stop`, the infinite producer is unwound and the counter reaches one; without `stop` it would be parked forever and the counter would stay zero. The demo then shows the negative-limit error path.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	pulliter "example.com/pull-bridge"
)

func main() {
	releases := 0
	infinite := func(yield func(int) bool) {
		defer func() { releases++ }()
		for i := 0; ; i++ {
			if !yield(i) {
				return
			}
		}
	}

	got, _ := pulliter.PullN(infinite, 4)
	fmt.Println("pulled:", got)
	fmt.Println("producer released:", releases == 1)

	_, err := pulliter.PullN(pulliter.FromSlice([]int{1}), -1)
	fmt.Println("negative limit error:", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pulled: [0 1 2 3]
producer released: true
negative limit error: true
```

### Tests

The tests pin three properties. `TestPullN` runs a table against a four-element sequence: zero values, a partial take, and a take past the end, confirming the bound and the clean stop at exhaustion. `TestPullNRejectsNegativeLimit` checks the sentinel is returned and matchable with `errors.Is`. `TestPullNStopsProducer` is the important one: it pulls three values from an infinite producer whose loop sets a flag in a `defer`, then asserts the flag is set — which can only happen if `PullN`'s `defer stop()` unwound the producer.

Create `pulliter_test.go`:

```go
package pulliter

import (
	"errors"
	"reflect"
	"testing"
)

func TestPullN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		want []int
	}{
		{name: "none", n: 0, want: []int{}},
		{name: "some", n: 3, want: []int{1, 2, 3}},
		{name: "past end", n: 10, want: []int{1, 2, 3, 4}},
	}

	for _, tt := range tests {
		got, err := PullN(FromSlice([]int{1, 2, 3, 4}), tt.n)
		if err != nil {
			t.Fatalf("%s: PullN error = %v", tt.name, err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestPullNRejectsNegativeLimit(t *testing.T) {
	t.Parallel()

	_, err := PullN(FromSlice([]int{1}), -1)
	if !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("err = %v, want ErrNegativeLimit", err)
	}
}

func TestPullNStopsProducer(t *testing.T) {
	t.Parallel()

	cleaned := false
	seq := func(yield func(int) bool) {
		defer func() { cleaned = true }()
		for i := 0; ; i++ {
			if !yield(i) {
				return
			}
		}
	}

	got, err := PullN(seq, 3)
	if err != nil {
		t.Fatalf("PullN error = %v", err)
	}
	if want := []int{0, 1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if !cleaned {
		t.Fatal("producer cleanup did not run: stop() was not called")
	}
}
```

## Review

`PullN` is correct when it never drains more than it must and always releases what it opened. The bound is the `for range n` loop that breaks on `ok == false`, so a request larger than the sequence returns everything; the release is the `defer stop()` placed before the first `next`, so the producer goroutine is torn down on every exit. The infinite-producer test is the real proof: a flag set in the producer's own `defer` can only flip if `stop` resumed and unwound it, which is exactly the guarantee an early-terminating pull consumer depends on.

The common trap is treating `stop` as optional because a quick test that consumes the whole sequence passes without it — the producer there reaches its own end and cleans up regardless. The leak only appears under early termination, which is the case pull is for, so the test deliberately uses an infinite producer that can *only* be cleaned up by `stop`. The second trap is returning `nil` for a zero-count pull; `make([]T, 0, n)` keeps the empty result non-nil and distinct from an error.

## Resources

- [`iter.Pull`](https://pkg.go.dev/iter#Pull) — the standard-library function, its `(next, stop)` return, and the rule that `stop` must be called.
- [`iter` package overview](https://pkg.go.dev/iter) — push vs pull iterators, `Seq`, and how pull iteration is implemented.
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions) — the design of range-over-func and where pull conversion fits.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-lockstep-zip-and-merge.md](02-lockstep-zip-and-merge.md)
