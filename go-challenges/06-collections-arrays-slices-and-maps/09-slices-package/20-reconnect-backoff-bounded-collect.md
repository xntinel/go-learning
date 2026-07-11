# Exercise 20: Bound An Infinite Sequence Before slices.Collect

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A reconnect loop's backoff schedule is naturally unbounded: it doubles the
delay after every failed attempt, caps at some maximum, and keeps producing
the next interval for as long as the connection keeps failing -- which could
be forever. Modeled as a Go 1.23 `iter.Seq[time.Duration]`, that generator
never returns from the loop inside its own function; it only stops when the
caller's `yield` callback returns `false`. That is precisely how a
range-over-func iterator is supposed to work, and it is also a loaded gun:
`slices.Collect` takes any `iter.Seq[E]` and drains it completely into a
slice, with no concept of "enough" built in. Hand it a sequence that never
signals it is done, and `Collect` never returns either.

The trap is reaching for `slices.Collect` the moment you have an iterator and
want a slice, without first asking whether the iterator is bounded. It is an
easy trap because most of the sequences a codebase produces *are* bounded --
a slice's own elements, a map's keys, a file's lines -- so `Collect` behaves
exactly as expected on all of them, right up until someone points it at the
one sequence that models an open-ended process instead of a finite collection.
Logging "the first five backoff intervals" for a support ticket, or asserting
on them in a test, are the moments this actually gets tried, and `Collect`
hangs the goroutine that tries it -- not with an error, not with a panic, just
silence, because from the sequence's point of view it is doing exactly what
it was asked: producing the next interval, forever.

This module builds `NewSequence`, the unbounded backoff generator, and `Take`,
the combinator that turns any sequence into one that stops after `n` elements
-- the one piece of code standing between an infinite generator and a
`slices.Collect` call that is actually safe to make.

The failure mode is worth naming precisely, because it is not the failure
mode most Go code is written to guard against. A nil pointer panics. A
missing map key returns a zero value you can check for. An out-of-range slice
index panics too, loudly, at the exact line that got it wrong. A goroutine
blocked forever inside `slices.Collect` does none of that: it does not crash,
it does not log anything, it does not show up in a stack trace unless someone
thinks to ask for one. It just stops making progress, and whatever else was
waiting on its result -- an HTTP handler, a test, a CLI command -- stops
making progress with it. In a request-scoped goroutine this shows up as a
request that never completes; in a background worker it shows up as a leaked
goroutine that quietly grows the process's goroutine count on every call site
that made the same mistake. Neither looks anything like "this specific line of
code is wrong" from the outside.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
retrybackoff/                module example.com/retrybackoff
  go.mod                     go 1.24
  retrybackoff.go            NewSequence, Take, First; ErrInvalidBase, ErrInvalidMax
  retrybackoff_test.go       validation table, doubling/cap behavior, edge cases,
                             the direct-Collect contrast, concurrency, ExampleFirst
```

- Files: `retrybackoff.go`, `retrybackoff_test.go`.
- Implement: `NewSequence(base, max time.Duration) (iter.Seq[time.Duration], error)` returning an unbounded, doubling-then-capped backoff generator, rejecting a non-positive `base` with `ErrInvalidBase` or a `max` below `base` with `ErrInvalidMax`; `Take[T any](seq iter.Seq[T], n int) iter.Seq[T]` bounding any sequence to at most `n` elements, panicking on a negative `n`; `First(seq iter.Seq[time.Duration], n int) []time.Duration` as `slices.Collect(Take(seq, n))`.
- Test: `NewSequence` validation table including `base == max`; the doubling-then-capped interval sequence; `n == 0` and `n == 1` edge cases; `Take` panicking on a negative `n`; a direct-`slices.Collect` contrast proving it drains an entire large source regardless of how many elements the caller wanted, next to `First` bounding the same source correctly; the same sequence value called concurrently by many goroutines; and `ExampleFirst` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/retrybackoff
cd ~/go-exercises/retrybackoff
go mod init example.com/retrybackoff
go mod edit -go=1.24
```

### Collect drains; it never asks how much you actually wanted

`slices.Collect(seq)` is defined, in effect, as `for v := range seq { out =
append(out, v) }` -- it ranges over the whole sequence and stops precisely
when the sequence itself stops calling `yield`. For a bounded sequence -- the
keys of a map, the lines already read from a closed file -- that is exactly
the right behavior, and it is why `Collect` is the standard way to bridge an
`iter.Seq[E]` back into a concrete slice. For an unbounded sequence, "stops
precisely when the sequence stops" means "never", because the sequence itself
never stops:

```go
seq, _ := retrybackoff.NewSequence(100*time.Millisecond, time.Second)
all := slices.Collect(seq)   // blocks the calling goroutine forever
```

`NewSequence`'s own `for { ... }` loop only exits through its `yield`
callback returning `false` -- that is the range-over-func protocol's way of
saying "the consumer is done asking for more" -- and `slices.Collect`'s
`yield` implementation never returns `false`; it always wants the next
element. Nothing here is a bug in `Collect`, and nothing is a bug in
`NewSequence`: each does exactly what its contract promises. The bug is
composing them without anything in between.

`Take` is that something. It wraps any `iter.Seq[T]` in a new sequence that
forwards up to `n` elements and then returns from its own `yield` loop with
`false` sent upstream implicitly -- range-over-func stops the wrapped
sequence as soon as `Take`'s inner function returns, which is exactly the
"the consumer is done" signal `NewSequence`'s loop is waiting for. `First`
composes `Take` and `slices.Collect` in the one order that is always safe:
bound first, drain second. `slices.Collect(Take(seq, n))` terminates after at
most `n` elements no matter what `seq` is; `slices.Collect(seq)` terminates
only if `seq` was already bounded on its own, and the type system gives no
signal either way.

Create `retrybackoff.go`:

```go
// Package retrybackoff generates exponential backoff intervals as an
// unbounded iter.Seq[time.Duration] -- the shape a reconnect loop's delay
// schedule naturally has, since it runs until the connection succeeds, not
// for a predetermined number of attempts. Materializing "the first N
// intervals" for logging, testing, or display requires stopping the
// sequence explicitly; slices.Collect never stops one on its own.
package retrybackoff

import (
	"errors"
	"iter"
	"slices"
	"time"
)

// Sentinel errors returned by NewSequence. Callers should test for them
// with errors.Is.
var (
	// ErrInvalidBase means base was not positive.
	ErrInvalidBase = errors.New("retrybackoff: base must be positive")
	// ErrInvalidMax means max was smaller than base.
	ErrInvalidMax = errors.New("retrybackoff: max must be >= base")
)

// NewSequence returns an unbounded iterator of exponential backoff
// intervals: base, 2*base, 4*base, ..., capped at max and repeated forever
// once the cap is reached. It never terminates on its own -- ranging over
// it directly, or passing it straight to slices.Collect, blocks the
// calling goroutine forever. Use Take (or First) to bound it first.
//
// The returned iterator is stateless per call: each range over it starts
// again from base, so the same iter.Seq value is safe to range over from
// multiple goroutines concurrently.
func NewSequence(base, max time.Duration) (iter.Seq[time.Duration], error) {
	if base <= 0 {
		return nil, ErrInvalidBase
	}
	if max < base {
		return nil, ErrInvalidMax
	}
	return func(yield func(time.Duration) bool) {
		d := base
		for {
			if !yield(d) {
				return
			}
			if d < max {
				d *= 2
				if d > max {
					d = max
				}
			}
		}
	}, nil
}

// Take returns an iterator over at most n elements of seq, stopping seq's
// production as soon as n elements have been yielded (or seq itself ends,
// whichever comes first). It is the combinator that makes an unbounded
// sequence like the one NewSequence returns safe to pass to
// slices.Collect.
//
// Take panics if n is negative, matching slices.Grow's convention for a
// negative size argument.
func Take[T any](seq iter.Seq[T], n int) iter.Seq[T] {
	if n < 0 {
		panic("retrybackoff: n must not be negative")
	}
	return func(yield func(T) bool) {
		count := 0
		for v := range seq {
			if count == n || !yield(v) {
				return
			}
			count++
		}
	}
}

// First materializes the first n intervals of seq into a freshly allocated
// slice. It is the safe way to turn an unbounded sequence like the one
// NewSequence returns into a slice: Take bounds it first, and
// slices.Collect only then drains it, so First always returns regardless
// of how long seq itself would run unbounded.
func First(seq iter.Seq[time.Duration], n int) []time.Duration {
	return slices.Collect(Take(seq, n))
}
```

### Using it

Call `NewSequence` once with the base delay and the cap, keep the returned
`iter.Seq[time.Duration]` value, and never call `slices.Collect` on it
directly. Anywhere a bounded number of intervals is needed -- seeding a retry
policy's first few delays into a config struct, asserting on them in a test,
printing them in a debug log -- go through `First`, which threads the sequence
through `Take` before it ever reaches `Collect`. Because `NewSequence`'s
generator restarts from `base` on every fresh `range`, the same sequence value
can be hedged to `First` from as many call sites, and as many goroutines, as
needed: each call gets its own independent walk, and none of them observe or
disturb another's progress.

`Take` is a general-purpose combinator, not specific to backoff schedules --
it bounds any `iter.Seq[T]`, which is why it is a generic function rather than
a method. That generality is also why it panics on a negative `n` instead of
returning an error: `n` is a caller-supplied constant almost everywhere it
is used, the same category of argument `slices.Grow` treats the same way, and
a construction-time panic surfaces the mistake immediately rather than
letting a silently-empty sequence through.

The same shape of problem shows up outside backoff schedules wherever a
range-over-func sequence models a live or open-ended source rather than a
fixed collection: a channel drained via `iter.Seq`, a file being tailed, a
paginated API whose next-page token never runs out on its own. `Take` and
`First` generalize directly to any of them, because neither one knows or
cares what `seq` actually produces -- only that it stops asking once `n`
elements have come through. The discipline this module is really teaching is
narrower than "know how backoff schedules work": it is "before collecting an
iterator you did not write yourself, know whether it is bounded, and if you
cannot be sure, bound it yourself."

`ExampleFirst` is the runnable demonstration of this module: `go test`
executes it and compares its stdout against the `// Output:` comment below.

```go
func ExampleFirst() {
	seq, err := NewSequence(100*time.Millisecond, 800*time.Millisecond)
	if err != nil {
		panic(err)
	}
	fmt.Println(First(seq, 5))

	// Output:
	// [100ms 200ms 400ms 800ms 800ms]
}
```

The five intervals double from 100ms to 800ms and then repeat 800ms, showing
both the doubling behavior and the cap in one call -- and the call returns
immediately, because `First` bounded the sequence before it ever reached
`slices.Collect`.

### Tests

`TestNewSequenceValidation` is the constructor table: a valid range, `base`
equal to `max` (a degenerate but legal one-value sequence), a non-positive
`base`, and a `max` below `base`. `TestFirstDoublesThenCaps` pins the doubling
arithmetic and the cap together in one sequence. `TestFirstEdgeCases` checks
`n == 0` (returns `nil`, matching `slices.Collect`'s own contract for an empty
result) and `n == 1`. `TestTakeRejectsNegativeN` confirms the documented
panic. `TestCollectingDirectlyDrainsEverything` is the heart of the module:
`unboundedStandIn` is a large-but-finite iterator standing in for a source
that in production has no bound at all -- purely so this test itself finishes
in milliseconds instead of hanging the suite -- and it shows `slices.Collect`
called on it directly returns every one of its fifty thousand elements, while
`First` on the identical source returns exactly the five the caller asked
for. `TestNewSequenceConcurrent` calls `First` on the same sequence value from
twenty goroutines at once, holding `NewSequence`'s stateless-per-call
concurrency claim to account.

Create `retrybackoff_test.go`:

```go
package retrybackoff

import (
	"errors"
	"fmt"
	"iter"
	"slices"
	"sync"
	"testing"
	"time"
)

// unboundedStandIn simulates what a genuinely unbounded live source looks
// like: a reconnect loop's real backoff sequence in production has no
// upper bound at all, so slices.Collect on it never returns. This
// stand-in is bounded at a large but finite count purely so the test
// suite itself terminates quickly; the property it proves -- Collect
// drains everything, ignoring how many elements the caller actually
// wanted -- is the same one that hangs a real unbounded source forever.
func unboundedStandIn(yield func(time.Duration) bool) {
	for i := range 50_000 {
		if !yield(time.Duration(i)) {
			return
		}
	}
}

func TestNewSequenceValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		base, max time.Duration
		wantErr   error
	}{
		{name: "valid", base: 100 * time.Millisecond, max: time.Second},
		{name: "base equals max", base: time.Second, max: time.Second},
		{name: "zero base", base: 0, max: time.Second, wantErr: ErrInvalidBase},
		{name: "negative base", base: -1, max: time.Second, wantErr: ErrInvalidBase},
		{name: "max below base", base: time.Second, max: 100 * time.Millisecond, wantErr: ErrInvalidMax},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewSequence(tc.base, tc.max)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("NewSequence error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestFirstDoublesThenCaps(t *testing.T) {
	t.Parallel()

	seq, err := NewSequence(100*time.Millisecond, 800*time.Millisecond)
	if err != nil {
		t.Fatalf("NewSequence: %v", err)
	}
	want := []time.Duration{100, 200, 400, 800, 800}
	for i := range want {
		want[i] *= time.Millisecond
	}
	if got := First(seq, 5); !slices.Equal(got, want) {
		t.Fatalf("First(seq, 5) = %v, want %v", got, want)
	}
}

func TestFirstEdgeCases(t *testing.T) {
	t.Parallel()

	seq, err := NewSequence(time.Second, time.Second)
	if err != nil {
		t.Fatalf("NewSequence: %v", err)
	}
	if got := First(seq, 0); got != nil {
		t.Fatalf("First(seq, 0) = %v, want nil", got)
	}
	if got := First(seq, 1); !slices.Equal(got, []time.Duration{time.Second}) {
		t.Fatalf("First(seq, 1) = %v, want [1s]", got)
	}
}

func TestTakeRejectsNegativeN(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("Take(seq, -1) should have panicked")
		}
	}()
	Take(unboundedStandIn, -1)
}

// TestCollectingDirectlyDrainsEverything is the heart of the module: a
// caller who passes an effectively unbounded sequence straight to
// slices.Collect, instead of bounding it with Take first, gets every
// element the source ever produces -- and a real reconnect loop's backoff
// sequence never produces a last one.
func TestCollectingDirectlyDrainsEverything(t *testing.T) {
	t.Parallel()

	got := slices.Collect(iter.Seq[time.Duration](unboundedStandIn))
	if len(got) != 50_000 {
		t.Fatalf("len(got) = %d, want 50000; slices.Collect should drain the whole source", len(got))
	}

	bounded := First(unboundedStandIn, 5)
	if len(bounded) != 5 {
		t.Fatalf("len(First(..., 5)) = %d, want 5", len(bounded))
	}
}

func TestNewSequenceConcurrent(t *testing.T) {
	t.Parallel()

	seq, err := NewSequence(100*time.Millisecond, 400*time.Millisecond)
	if err != nil {
		t.Fatalf("NewSequence: %v", err)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := First(seq, 3); !slices.Equal(got, want) {
				t.Errorf("First(seq, 3) = %v, want %v", got, want)
			}
		}()
	}
	wg.Wait()
}

// ExampleFirst is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleFirst() {
	seq, err := NewSequence(100*time.Millisecond, 800*time.Millisecond)
	if err != nil {
		panic(err)
	}
	fmt.Println(First(seq, 5))

	// Output:
	// [100ms 200ms 400ms 800ms 800ms]
}
```

## Review

`First` is correct because it never lets `slices.Collect` see an unbounded
sequence: `Take` sits between them on every call, so the sequence is always
stopped at `n` elements before `Collect` ever starts draining it. The mistake
it avoids is calling `slices.Collect(seq)` directly on a generator that has no
natural end -- syntactically identical to collecting any other iterator,
correct for every bounded one, and a permanent hang for this one, with no
error and no panic to point at the cause. `NewSequence` rejects a
non-positive `base` with `ErrInvalidBase` and a `max` below `base` with
`ErrInvalidMax`, both checkable with `errors.Is`; `Take` panics on a negative
`n`, the same convention `slices.Grow` uses for the same kind of argument. The
returned sequence is stateless per call, so it is safe to hand to `First` from
as many goroutines as needed, each getting an independent walk from `base`.
Run `go test -count=1 -race ./...` to confirm the validation table, the
doubling-and-cap arithmetic, the edge cases, the direct-`Collect` contrast,
and the concurrent use.

## Resources

- [`slices.Collect`](https://pkg.go.dev/slices#Collect) — drains an `iter.Seq[E]` completely; the function this module makes safe to call on an unbounded source.
- [`iter.Seq`](https://pkg.go.dev/iter#Seq) — the range-over-func iterator type, and the `yield` protocol a bounded consumer uses to stop production early.
- [`slices.Grow`](https://pkg.go.dev/slices#Grow) — the panics-on-negative-size convention `Take` follows for its own `n` argument.
- [Go Blog: Range over Function Types](https://go.dev/blog/range-functions) — the range-over-func mechanics `Take` relies on to stop `seq`'s production early.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-etcd-range-scan-appendseq.md](19-etcd-range-scan-appendseq.md) | Next: [../10-maps-package/00-concepts.md](../10-maps-package/00-concepts.md)
