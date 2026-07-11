# Exercise 2: Pull Adapters

Sometimes you need values on demand rather than pushed at you — to take only the first few, or to advance an iterator from outside a `range` loop. `iter.Pull` and `iter.Pull2` convert a push `Seq` into a manual `next`/`stop` pair. This exercise builds two pull-based helpers and drills the one discipline that pull demands: always `defer stop()`.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
pull.go              PullFirst (iter.Pull), PullPairs (iter.Pull2), ErrNegativeLimit
cmd/
  demo/
    main.go          pull the first few values from an infinite counter
pull_test.go         take N, take fewer than available, take zero, validation error
```

- Files: `pull.go`, `cmd/demo/main.go`, `pull_test.go`.
- Implement: `PullFirst[V any](seq iter.Seq[V], n int) ([]V, error)` and `PullPairs[K, V any](seq iter.Seq2[K, V], n int) ([]K, []V, error)`.
- Test: take exactly N from a long iterator, take more than the iterator holds, take zero, and reject a negative count with `ErrNegativeLimit`.
- Verify: `go test -run 'TestPullFirst|TestPullPairs|TestValidation' -race ./...`

Set up the module:

```bash
mkdir -p pull-adapters/cmd/demo && cd pull-adapters
go mod init example.com/pull-adapters
```

### Why Pull exists, and why stop is mandatory

A push `Seq` is perfect for `range`, but it owns its loop, which makes "give me just the first five" awkward and "advance two iterators in lockstep" impossible. `iter.Pull` inverts the inversion. Given a `Seq[V]`, it returns `next func() (V, bool)` and `stop func()`. Each call to `next()` produces one value and the `ok` flag, returning `(zero, false)` once the source is exhausted. `Pull2` is identical for `Seq2`, with `next` returning `(K, V, bool)`.

The implementation runs the source `Seq` on a separate goroutine parked at each `yield`; `next()` unparks it for exactly one value. That goroutine is the reason `stop` is not optional. If you pull an iterator, take what you need, and return without calling `stop`, the goroutine stays parked forever and anything the source holds open — a file, a cursor, a socket — is never released. The rule is one line and unconditional: `defer stop()` on the line immediately after `iter.Pull`, before the first `next()`. Putting it there means every exit path — the natural end of the data, an early return after collecting `n`, a panic — releases the goroutine. `stop` is idempotent, so calling it via `defer` even when the iterator already drained naturally is harmless. Calling `stop` also resumes the parked source one last time with `yield` returning `false`, so the source's own deferred cleanup runs; skipping `stop` skips that cleanup too.

`PullFirst` shows the pattern at its smallest: pull, defer stop, then call `next()` up to `n` times, breaking as soon as `ok` is false so a short source does not over-collect. `PullPairs` is the `Pull2` twin, accumulating keys and values in parallel. Both validate `n` eagerly so a negative count fails before any goroutine is spawned.

Create `pull.go`:

```go
// Create `pull.go`
package pull

import (
	"errors"
	"fmt"
	"iter"
)

// ErrNegativeLimit is returned when a count argument is negative.
var ErrNegativeLimit = errors.New("count must not be negative")

// PullFirst drains at most n values from seq using iter.Pull and returns them.
// If seq yields fewer than n values, the shorter slice is returned.
func PullFirst[V any](seq iter.Seq[V], n int) ([]V, error) {
	if n < 0 {
		return nil, fmt.Errorf("pull first %d: %w", n, ErrNegativeLimit)
	}

	next, stop := iter.Pull(seq)
	defer stop()

	out := make([]V, 0, n)
	for range n {
		v, ok := next()
		if !ok {
			break
		}
		out = append(out, v)
	}
	return out, nil
}

// PullPairs drains at most n key/value pairs from seq using iter.Pull2 and
// returns them as parallel slices.
func PullPairs[K, V any](seq iter.Seq2[K, V], n int) ([]K, []V, error) {
	if n < 0 {
		return nil, nil, fmt.Errorf("pull pairs %d: %w", n, ErrNegativeLimit)
	}

	next, stop := iter.Pull2(seq)
	defer stop()

	keys := make([]K, 0, n)
	vals := make([]V, 0, n)
	for range n {
		k, v, ok := next()
		if !ok {
			break
		}
		keys = append(keys, k)
		vals = append(vals, v)
	}
	return keys, vals, nil
}
```

### The runnable demo

The demo defines an unbounded counter as an `iter.Seq[int]` and uses `PullFirst` to take just the first five values from it. Ranging over an unbounded iterator would never terminate; pulling a fixed count is exactly the case `iter.Pull` is for. Because `PullFirst` defers `stop`, the counter's goroutine is released even though the source itself never ends.

Create `cmd/demo/main.go`:

```go
// Create `cmd/demo/main.go`
package main

import (
	"fmt"
	"iter"

	"example.com/pull-adapters"
)

// countFrom is an unbounded push iterator: it never stops on its own.
func countFrom(start int) iter.Seq[int] {
	return func(yield func(int) bool) {
		for n := start; ; n++ {
			if !yield(n) {
				return
			}
		}
	}
}

func main() {
	first, err := pull.PullFirst(countFrom(10), 5)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("first five from 10:", first)

	short, _ := pull.PullFirst(countFrom(0), 0)
	fmt.Println("take zero:", short, "len", len(short))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first five from 10: [10 11 12 13 14]
take zero: [] len 0
```

### Tests

`TestPullFirst` takes exactly N from a long iterator, then takes more than a short iterator holds and asserts it stops at the real end rather than padding. `TestPullPairs` does the same for the `Pull2` path. A take-zero case proves the loop body never runs when `n == 0`. `TestValidation` asserts both helpers reject a negative count with `ErrNegativeLimit`.

Create `pull_test.go`:

```go
// Create `pull_test.go`
package pull

import (
	"errors"
	"iter"
	"slices"
	"testing"
)

func upTo(limit int) iter.Seq[int] {
	return func(yield func(int) bool) {
		for n := 0; n < limit; n++ {
			if !yield(n) {
				return
			}
		}
	}
}

func enumerate(values []string) iter.Seq2[int, string] {
	return func(yield func(int, string) bool) {
		for i, v := range values {
			if !yield(i, v) {
				return
			}
		}
	}
}

func TestPullFirst(t *testing.T) {
	t.Parallel()

	got, err := PullFirst(upTo(100), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []int{0, 1, 2}) {
		t.Fatalf("take 3 = %v", got)
	}

	short, err := PullFirst(upTo(2), 5)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(short, []int{0, 1}) {
		t.Fatalf("take 5 of 2 = %v, want [0 1]", short)
	}

	zero, err := PullFirst(upTo(100), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(zero) != 0 {
		t.Fatalf("take 0 = %v, want empty", zero)
	}
}

func TestPullPairs(t *testing.T) {
	t.Parallel()

	keys, vals, err := PullPairs(enumerate([]string{"a", "b", "c"}), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys, []int{0, 1}) || !slices.Equal(vals, []string{"a", "b"}) {
		t.Fatalf("keys=%v vals=%v", keys, vals)
	}
}

func TestValidation(t *testing.T) {
	t.Parallel()

	if _, err := PullFirst(upTo(3), -1); !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("PullFirst(-1) error = %v", err)
	}
	if _, _, err := PullPairs(enumerate(nil), -1); !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("PullPairs(-1) error = %v", err)
	}
}
```

## Review

The pull helpers are correct when `stop` is deferred unconditionally on the line after `iter.Pull` and the collection loop breaks the moment `next` reports `ok == false`. The most consequential test is the "take more than available" case: it proves the loop stops at the source's real end instead of appending zero values, which is the bug you get if you trust `n` over `ok`. The take-zero case confirms the `for range n` loop body simply does not execute when `n` is zero, so no goroutine work happens beyond the pull setup that `defer stop()` cleans up.

The mistake to internalize is leaking the goroutine. Writing `next, stop := iter.Pull(seq)` and then returning on an error path, or inside a conditional, without having already `defer`red `stop` strands the source goroutine and its resources. Deferring immediately makes every path safe and costs nothing on the success path because `stop` is idempotent. Validating `n` before the pull also matters: a negative count returns an error before any goroutine is spawned, so the bad-input path never has anything to clean up.

## Resources

- [`iter.Pull`](https://pkg.go.dev/iter#Pull) — converts a `Seq` into `next`/`stop`, with the documented requirement to call `stop`.
- [`iter.Pull2`](https://pkg.go.dev/iter#Pull2) — the `Seq2` twin returning a three-value `next`.
- [Range Over Function Types](https://go.dev/blog/range-functions) — explains the coroutine that backs `Pull` and why `stop` releases it.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-stdlib-iterator-integration.md](03-stdlib-iterator-integration.md)
