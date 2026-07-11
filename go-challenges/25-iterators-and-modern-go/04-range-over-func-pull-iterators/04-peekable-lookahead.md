# Exercise 4: A Peekable One-Token Lookahead

Parsers and run-grouping passes need to inspect the next value without consuming it — to decide whether to take it based on what it is. Push iteration has nowhere to hold a value back, so lookahead is a pull-only construct. This exercise builds `Peekable`, a wrapper over `iter.Pull` that buffers exactly one value, exposing `Peek`, `Next`, and a `Close` that you must always defer.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
peek.go              FromSlice, Peekable[T] (Peek/Next/Close), TakeWhile
cmd/
  demo/
    main.go          take a leading run, peek the breakpoint, drain the rest
peek_test.go         peek is non-destructive, end-of-input, TakeWhile, Close stops
```

- Files: `peek.go`, `cmd/demo/main.go`, `peek_test.go`.
- Implement: `FromSlice[T]`, `Peekable[T]` with `New`, `Peek`, `Next`, `Close`, and the `TakeWhile` helper.
- Test: `peek_test.go` checks that `Peek` does not advance, end-of-input behavior, `TakeWhile` stopping at the predicate, and that `Close` unwinds the producer.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p peekable/cmd/demo && cd peekable
go mod init example.com/peekable
```

### Why peek is pull-only, and the one-slot buffer

Lookahead means answering "what is next?" without committing to consuming it. A push iterator cannot do this: its loop pushes each value into your callback and moves on; there is no point at which you hold a value, look at it, and put it back. Pull inverts that — `next` hands you one value when you ask — but `iter.Pull` on its own has no "un-pull." So `Peekable` adds exactly one slot of buffer on top of `iter.Pull`: `Peek` pulls a value into the slot and leaves it there; `Next` returns the slot's value and empties it; a `Peek` while the slot is full returns the buffered value without pulling again.

Three fields carry the state: `buf` and `bufOK` hold the looked-ahead value and whether it exists, and `primed` records whether the slot is currently full. `Peek` fills the slot only when `primed` is false, so repeated `Peek`s are idempotent — they all see the same value. `Next` calls `Peek` to guarantee the slot is full, captures the result, then clears `primed` so the following `Peek` pulls the value after it. End of input rides along naturally: once `next` returns `ok == false`, `bufOK` is false, and both `Peek` and `Next` report it.

`Close` is the lookahead version of the `stop` discipline from the earlier exercises. `New` runs `iter.Pull` and stores the `stop`; `Close` calls it. Because `Peekable` is a value the caller holds, the `stop` is not behind a `defer` inside one function — the *caller* owns the lifetime, so the caller must `defer p.Close()` right after `New`. A peekable that is abandoned without `Close` leaks its producer goroutine exactly as a forgotten bare `stop` would.

Create `peek.go`:

```go
package peek

import "iter"

// FromSlice returns a push iterator that yields each element of values in order.
func FromSlice[T any](values []T) iter.Seq[T] {
	return func(yield func(T) bool) {
		for _, v := range values {
			if !yield(v) {
				return
			}
		}
	}
}

// Peekable is a one-value-lookahead view over a push iterator. It must be closed
// with Close to release the underlying pull iterator.
type Peekable[T any] struct {
	next   func() (T, bool)
	stop   func()
	buf    T
	bufOK  bool
	primed bool
}

// New converts seq to a pull iterator and wraps it with one slot of lookahead.
// The caller must defer Close to release the producer.
func New[T any](seq iter.Seq[T]) *Peekable[T] {
	next, stop := iter.Pull(seq)
	return &Peekable[T]{next: next, stop: stop}
}

// Peek returns the next value without consuming it. ok is false at end of input.
// Repeated Peek calls return the same value until Next consumes it.
func (p *Peekable[T]) Peek() (T, bool) {
	if !p.primed {
		p.buf, p.bufOK = p.next()
		p.primed = true
	}
	return p.buf, p.bufOK
}

// Next returns and consumes the next value. ok is false at end of input.
func (p *Peekable[T]) Next() (T, bool) {
	v, ok := p.Peek()
	var zero T
	p.buf, p.bufOK, p.primed = zero, false, false
	return v, ok
}

// Close releases the underlying pull iterator. It is safe to call more than once.
func (p *Peekable[T]) Close() { p.stop() }

// TakeWhile consumes and returns the leading run of values for which keep is
// true, leaving the first non-matching value (if any) available for the next
// Peek or Next.
func TakeWhile[T any](p *Peekable[T], keep func(T) bool) []T {
	out := []T{}
	for {
		v, ok := p.Peek()
		if !ok || !keep(v) {
			return out
		}
		p.Next()
		out = append(out, v)
	}
}
```

`TakeWhile` is the payoff and shows why peek exists: it must look at the next value to decide whether to take it, and if the predicate fails it must *not* consume that value, so the caller can keep reading from it. `Peek` followed by a conditional `Next` is exactly that. A pure push iterator could not implement `TakeWhile` as a reusable step, because once a value is pushed past the predicate it is gone.

### The runnable demo

The demo reads a leading run of even numbers with `TakeWhile`, then peeks at the value that broke the run (the first odd number) to show it was not consumed, then drains the rest with `Next` to show everything from the breakpoint onward is still there.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/peekable"
)

func main() {
	p := peek.New(peek.FromSlice([]int{2, 4, 6, 7, 8, 10}))
	defer p.Close()

	evens := peek.TakeWhile(p, func(n int) bool { return n%2 == 0 })
	fmt.Println("leading evens:", evens)

	v, ok := p.Peek()
	fmt.Printf("next value waiting: %d (ok=%v)\n", v, ok)

	rest := []int{}
	for {
		v, ok := p.Next()
		if !ok {
			break
		}
		rest = append(rest, v)
	}
	fmt.Println("rest:", rest)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
leading evens: [2 4 6]
next value waiting: 7 (ok=true)
rest: [7 8 10]
```

### Tests

`TestPeekDoesNotConsume` peeks three times and asserts the same value each time, then `Next`s once and peeks again to confirm the cursor advanced by exactly one. `TestPeekAtEnd` drains a single-element sequence and checks that `Peek` and `Next` both report `ok == false` past the end. `TestTakeWhile` takes the leading even run and asserts the breakpoint value is still peekable afterward. `TestCloseStopsEarly` pulls two values from an infinite producer, calls `Close`, and asserts the producer's deferred cleanup ran.

Create `peek_test.go`:

```go
package peek

import (
	"reflect"
	"testing"
)

func TestPeekDoesNotConsume(t *testing.T) {
	t.Parallel()

	p := New(FromSlice([]int{10, 20, 30}))
	defer p.Close()

	for i := 0; i < 3; i++ {
		v, ok := p.Peek()
		if !ok || v != 10 {
			t.Fatalf("Peek %d = (%d,%v), want (10,true)", i, v, ok)
		}
	}
	v, ok := p.Next()
	if !ok || v != 10 {
		t.Fatalf("Next = (%d,%v), want (10,true)", v, ok)
	}
	v, ok = p.Peek()
	if !ok || v != 20 {
		t.Fatalf("Peek after Next = (%d,%v), want (20,true)", v, ok)
	}
}

func TestPeekAtEnd(t *testing.T) {
	t.Parallel()

	p := New(FromSlice([]int{1}))
	defer p.Close()

	if _, ok := p.Next(); !ok {
		t.Fatal("first Next should succeed")
	}
	if _, ok := p.Peek(); ok {
		t.Fatal("Peek past end should report ok=false")
	}
	if _, ok := p.Next(); ok {
		t.Fatal("Next past end should report ok=false")
	}
}

func TestTakeWhile(t *testing.T) {
	t.Parallel()

	p := New(FromSlice([]int{2, 4, 6, 7, 8}))
	defer p.Close()

	got := TakeWhile(p, func(n int) bool { return n%2 == 0 })
	if want := []int{2, 4, 6}; !reflect.DeepEqual(got, want) {
		t.Fatalf("TakeWhile even = %v, want %v", got, want)
	}
	v, ok := p.Peek()
	if !ok || v != 7 {
		t.Fatalf("after TakeWhile Peek = (%d,%v), want (7,true)", v, ok)
	}
}

func TestCloseStopsEarly(t *testing.T) {
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

	p := New(seq)
	p.Next()
	p.Next()
	p.Close()
	if !cleaned {
		t.Fatal("Close did not stop the producer")
	}
}
```

## Review

`Peekable` is correct when `Peek` is non-destructive and `Next` advances by exactly one. The `primed` flag is the hinge: `Peek` fills the slot only when it is empty, so repeated peeks are stable, and `Next` clears `primed` so the slot refills on the following access. End-of-input is uniform because `bufOK` carries the `next` result through both methods. `TakeWhile` demonstrates the use the design exists for — decide on a value, then choose whether to consume it — which push iteration cannot express as a composable step.

The traps are about the buffer and the lifetime. Pulling a fresh value inside `Peek` every call, instead of caching, would make two peeks see two different values and silently drop the first — the triple-peek test catches that. Forgetting to clear `primed` in `Next` would make the iterator stick on one value forever. And because the caller owns a `Peekable` rather than a scoped `stop`, the `defer p.Close()` is the caller's responsibility; omit it and the producer goroutine leaks, which the infinite-producer `Close` test pins by requiring the producer's deferred flag to flip.

## Resources

- [`iter.Pull`](https://pkg.go.dev/iter#Pull) — the conversion `New` wraps, and the `stop` that `Close` calls.
- [`iter` package overview](https://pkg.go.dev/iter) — push vs pull and why lookahead belongs on the pull side.
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions) — the iterator model and where buffered pull adapters like this one fit.

---

Back to [03-merge-join-with-pull2.md](03-merge-join-with-pull2.md) | Next: [05-k-way-merge-with-heap.md](05-k-way-merge-with-heap.md)
