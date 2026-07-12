# Exercise 30: Stream Batching System with Timeout Callback and Deferred Flush

**Nivel: Intermedio** — validacion rapida (un test corto).

A batcher that only flushes once it hits its size threshold will happily
hold a handful of items forever if traffic dries up — nothing ever pushes
that last partial batch out. This module builds a `Batcher` whose `Add` and
`Tick` both defer a closure that checks two independent conditions after
the main logic runs — size threshold or max-wait deadline — and flushes on
whichever fires first.

This module is fully self-contained. Nothing here imports another
exercise.

## What you'll build

```text
batch/                        module example.com/batch
  go.mod
  batch.go                     Batcher, New, Add (deferred flush check), Tick, Len
  batch_test.go                  size-triggered, wait-triggered, empty Tick, wait-window reset
  cmd/demo/main.go              size flush then a timeout flush via Tick
```

- Files: `batch.go`, `batch_test.go`, `cmd/demo/main.go`.
- Implement: `Batcher{maxSize, maxWait, items, firstAt, flush}`; `Add(now, item)` appending then deferring a flush check; `Tick(now)` re-checking the deadline without adding an item.
- Test: `maxSize` flushes immediately; `Tick` before `maxWait` does not flush; `Tick` after `maxWait` flushes a partial batch; the wait window restarts after a flush.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A deferred closure checks both flush conditions after the fact

`Add` appends `item` to the buffer, then returns `false` — but the real
decision happens in a closure deferred *before* that append runs. By the
time the deferred closure executes, the buffer already has the new item in
it, so `full := len(b.items) >= b.maxSize` sees the post-append length.
`expired` checks `now.Sub(b.firstAt) >= b.maxWait` against `firstAt`,
which `Add` only sets when the buffer was empty — the timestamp of the
*oldest* buffered item, not the newest. If either condition holds, the
closure calls `flush`, resets `b.items` and `b.firstAt`, and sets the named
return `flushed` to `true` as its last act — the same named-return pattern
this chapter uses elsewhere, here reporting a decision made entirely inside
the defer rather than in `Add`'s own body. `Tick` reuses the identical
check with no append, so a periodic scheduler can push out a partial batch
that size alone would never trigger. Every timestamp here is a `time.Time`
parameter, never `time.Now()`, so tests control elapsed time exactly instead
of racing a real clock.

Create `batch.go`:

```go
package batch

import "time"

// Batcher accumulates items and flushes them either once maxSize is
// reached or once maxWait has elapsed since the oldest buffered item,
// whichever comes first.
type Batcher struct {
	maxSize int
	maxWait time.Duration
	items   []any
	firstAt time.Time
	flush   func([]any)
}

// New returns a Batcher that calls flush with every buffered item once a
// threshold is crossed. flush is the caller-supplied callback -- typically
// a closure over a sink (a channel, an HTTP client, a log) -- run inline by
// Add or Tick.
func New(maxSize int, maxWait time.Duration, flush func([]any)) *Batcher {
	return &Batcher{maxSize: maxSize, maxWait: maxWait, flush: flush}
}

// Add appends item to the buffer at time now (an injected timestamp, never
// time.Now, so callers -- and tests -- control elapsed time exactly). A
// deferred closure inspects the buffer afterward: if it has reached
// maxSize, or if maxWait has elapsed since the first item currently
// buffered, it calls flush and resets the buffer. flushed reports whether
// this call triggered that flush.
func (b *Batcher) Add(now time.Time, item any) (flushed bool) {
	defer func() {
		full := len(b.items) >= b.maxSize
		expired := !b.firstAt.IsZero() && now.Sub(b.firstAt) >= b.maxWait
		if full || expired {
			b.flush(b.items)
			b.items = nil
			b.firstAt = time.Time{}
			flushed = true
		}
	}()

	if len(b.items) == 0 {
		b.firstAt = now
	}
	b.items = append(b.items, item)
	return false
}

// Tick re-evaluates the max-wait deadline at time now without adding a new
// item -- as a periodic scheduler would call it -- flushing whatever
// partial batch is buffered if its oldest item has waited too long.
func (b *Batcher) Tick(now time.Time) (flushed bool) {
	defer func() {
		if len(b.items) > 0 && now.Sub(b.firstAt) >= b.maxWait {
			b.flush(b.items)
			b.items = nil
			b.firstAt = time.Time{}
			flushed = true
		}
	}()
	return false
}

// Len reports how many items are currently buffered.
func (b *Batcher) Len() int { return len(b.items) }
```

### The runnable demo

The demo fills a batch to `maxSize`, triggering an immediate flush, then
adds one more item and lets `Tick` flush it once `maxWait` has elapsed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/batch"
)

func main() {
	var flushed [][]any
	b := batch.New(3, 100*time.Millisecond, func(items []any) {
		flushed = append(flushed, append([]any(nil), items...))
	})

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.Add(base, "a")
	b.Add(base.Add(10*time.Millisecond), "b")
	b.Add(base.Add(20*time.Millisecond), "c") // hits maxSize=3, flushes [a b c]

	b.Add(base.Add(30*time.Millisecond), "d")
	b.Tick(base.Add(50 * time.Millisecond))  // 20ms since "d", no flush yet
	b.Tick(base.Add(140 * time.Millisecond)) // 110ms since "d" >= maxWait, flushes [d]

	for i, items := range flushed {
		fmt.Printf("flush %d: %v\n", i, items)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flush 0: [a b c]
flush 1: [d]
```

### Tests

`TestAddFlushesOnMaxSize` checks the third `Add` flushes and resets `Len`.
`TestTickDoesNotFlushBeforeMaxWait` checks a `Tick` before the deadline
leaves the item buffered. `TestTickFlushesPartialBatchAfterMaxWait` checks
a `Tick` past the deadline flushes a two-item partial batch.
`TestTickOnEmptyBatcherNeverFlushes` checks an empty buffer never calls
`flush`. `TestAddResetsWaitWindowAfterFlush` checks the wait window
restarts from the next item after a size-triggered flush, rather than
still counting from the original (already-flushed) first item.

Create `batch_test.go`:

```go
package batch

import (
	"testing"
	"time"
)

func TestAddFlushesOnMaxSize(t *testing.T) {
	t.Parallel()
	var flushes [][]any
	b := New(3, time.Hour, func(items []any) {
		flushes = append(flushes, append([]any(nil), items...))
	})
	base := time.Unix(0, 0)

	if b.Add(base, "a") {
		t.Fatal("Add 1/3 flushed early")
	}
	if b.Add(base, "b") {
		t.Fatal("Add 2/3 flushed early")
	}
	if !b.Add(base, "c") {
		t.Fatal("Add 3/3 did not flush at maxSize")
	}
	if len(flushes) != 1 || len(flushes[0]) != 3 {
		t.Fatalf("flushes = %v, want one flush of 3 items", flushes)
	}
	if b.Len() != 0 {
		t.Fatalf("Len() after flush = %d, want 0", b.Len())
	}
}

func TestTickDoesNotFlushBeforeMaxWait(t *testing.T) {
	t.Parallel()
	var flushes int
	b := New(10, 100*time.Millisecond, func([]any) { flushes++ })
	base := time.Unix(0, 0)

	b.Add(base, "x")
	if flushed := b.Tick(base.Add(50 * time.Millisecond)); flushed {
		t.Fatal("Tick flushed before maxWait elapsed")
	}
	if flushes != 0 || b.Len() != 1 {
		t.Fatalf("flushes=%d Len=%d, want 0 and 1 (still buffered)", flushes, b.Len())
	}
}

func TestTickFlushesPartialBatchAfterMaxWait(t *testing.T) {
	t.Parallel()
	var flushed []any
	b := New(10, 100*time.Millisecond, func(items []any) {
		flushed = append([]any(nil), items...)
	})
	base := time.Unix(0, 0)

	b.Add(base, "x")
	b.Add(base.Add(10*time.Millisecond), "y")
	if !b.Tick(base.Add(101 * time.Millisecond)) {
		t.Fatal("Tick did not flush once maxWait elapsed since the first item")
	}
	if len(flushed) != 2 {
		t.Fatalf("flushed = %v, want [x y]", flushed)
	}
	if b.Len() != 0 {
		t.Fatalf("Len() after Tick flush = %d, want 0", b.Len())
	}
}

func TestTickOnEmptyBatcherNeverFlushes(t *testing.T) {
	t.Parallel()
	flushed := false
	b := New(10, time.Millisecond, func([]any) { flushed = true })

	if b.Tick(time.Unix(0, 0).Add(time.Hour)) {
		t.Fatal("Tick on an empty batcher reported a flush")
	}
	if flushed {
		t.Fatal("flush callback ran on an empty batcher")
	}
}

func TestAddResetsWaitWindowAfterFlush(t *testing.T) {
	t.Parallel()
	var flushes int
	b := New(2, 100*time.Millisecond, func([]any) { flushes++ })
	base := time.Unix(0, 0)

	b.Add(base, "a")
	b.Add(base, "b") // flush 1 (size)
	b.Add(base.Add(200*time.Millisecond), "c")
	if flushed := b.Tick(base.Add(210 * time.Millisecond)); flushed {
		t.Fatal("Tick flushed too early: wait window must restart after the size-triggered flush")
	}
	if flushes != 1 {
		t.Fatalf("flushes = %d, want 1 so far", flushes)
	}
}
```

## Review

`Batcher` is correct when a flush happens on exactly one of two conditions
— `maxSize` reached, or `maxWait` elapsed since the oldest currently
buffered item — and never on a stale condition left over from before the
last flush. The wait-window-reset test is the one that catches the subtle
bug: if `firstAt` weren't cleared alongside `items` on every flush, a
size-triggered flush would leave the old timestamp behind, and the next
`Tick` would see an already-expired window through no fault of the new
batch's own age. Deferring the flush check in `Add` (rather than checking
it inline before `return false`) is what lets `Tick` share the exact same
condition logic without duplicating it — both entry points defer a
closure that reads the same two fields.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [time.Duration](https://pkg.go.dev/time#Duration)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-request-logging-deferred.md](29-request-logging-deferred.md) | Next: [31-permission-closure-factory.md](31-permission-closure-factory.md)
