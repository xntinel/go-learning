# Exercise 10: Flush-and-reset — swap out an accumulator via its pointer

A buffered event batch fills up, gets flushed downstream, and must reset to empty
so the collector keeps filling a fresh buffer — all without re-pointing the
caller's variable. The mechanism is a whole-value write through a pointer: `*dst =
Batch{}`. This module builds that flush loop and proves the reset lands on the
caller's own batch.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
eventbatch/                independent module: example.com/eventbatch
  go.mod                   module example.com/eventbatch
  batch.go                 Event; Batch{events []Event}; Add; Flush(dst *Batch) []Event
  cmd/
    demo/
      main.go              adds events, flushes, adds more, flushes again
  batch_test.go            flush returns accumulated and empties source; empty flush is safe; caller reset
```

- Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
- Implement: `Add(e Event)` and `Flush(dst *Batch) []Event` that returns the accumulated events and resets `*dst` to its zero value in place.
- Test: after `Flush` the returned slice holds the events and the source batch is empty (`Len() == 0`); flushing an empty batch returns empty without panicking; the caller's batch (passed by `&`) is the one reset.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/01-pointer-basics/10-reset-on-flush-swap/cmd/demo
cd go-solutions/09-pointers/01-pointer-basics/10-reset-on-flush-swap
```

### Reset in place with *dst = Batch{}

`Batch` accumulates `Event`s in a slice. A flush loop needs two things at once:
hand the accumulated events downstream, and leave the batch empty so the next `Add`
starts a fresh buffer. The idiomatic move is a whole-value write through the
pointer:

```go
func Flush(dst *Batch) []Event {
	out := dst.events   // grab the current buffer's header
	*dst = Batch{}      // overwrite the whole Batch in place: events becomes nil
	return out
}
```

`*dst = Batch{}` overwrites the entire pointed-to `Batch` with a zero value. Because
the caller passed `&batch`, this resets the caller's own variable — no reassignment
at the call site, no re-pointing. The slice-header detail matters: `out :=
dst.events` copies the *header* (pointer, len, cap) that references the current
backing array; `*dst = Batch{}` then zeroes `dst`'s header (so `dst.events` is now
`nil`) but does not touch the backing array, so the returned `out` remains a valid
slice over the flushed events. The collector can keep `Add`-ing into the now-empty
`dst` while downstream consumes `out` — the classic swap-old-for-new pattern used in
real flush loops (log buffers, span exporters, write batchers).

Create `batch.go`:

```go
package eventbatch

// Event is one buffered item.
type Event struct {
	Name string
}

// Batch accumulates events until it is flushed.
type Batch struct {
	events []Event
}

// Add appends an event to the batch.
func (b *Batch) Add(e Event) {
	b.events = append(b.events, e)
}

// Len reports how many events are buffered.
func (b *Batch) Len() int {
	return len(b.events)
}

// Flush returns the accumulated events and resets *dst to its zero value in
// place, so the caller's batch is empty and ready to fill again. The returned
// slice references the flushed events; the reset does not disturb it.
func Flush(dst *Batch) []Event {
	out := dst.events
	*dst = Batch{}
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/eventbatch"
)

func main() {
	var b eventbatch.Batch
	b.Add(eventbatch.Event{Name: "login"})
	b.Add(eventbatch.Event{Name: "click"})

	flushed := eventbatch.Flush(&b)
	fmt.Printf("flushed %d events; batch now has %d\n", len(flushed), b.Len())

	b.Add(eventbatch.Event{Name: "logout"})
	fmt.Printf("after new Add: batch has %d\n", b.Len())

	empty := eventbatch.Flush(&b)
	empty = eventbatch.Flush(&b) // flushing empty is safe
	fmt.Printf("double flush is safe; last returned %d\n", len(empty))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flushed 2 events; batch now has 0
after new Add: batch has 1
double flush is safe; last returned 0
```

### Tests

`TestFlushReturnsAndEmpties` adds events, flushes, and asserts the returned slice
holds them while the source batch is now empty. `TestEmptyFlushIsSafe` flushes an
untouched batch and asserts it returns an empty slice without panicking.
`TestFlushResetsCaller` proves the reset lands on the caller's own variable: it
passes `&b`, flushes, and reads `b.Len() == 0` afterward. `TestRefillAfterFlush`
confirms the batch is reusable — a fresh `Add` after a flush starts a new buffer.

Create `batch_test.go`:

```go
package eventbatch

import (
	"fmt"
	"testing"
)

func TestFlushReturnsAndEmpties(t *testing.T) {
	t.Parallel()

	var b Batch
	b.Add(Event{Name: "a"})
	b.Add(Event{Name: "b"})

	out := Flush(&b)
	if len(out) != 2 || out[0].Name != "a" || out[1].Name != "b" {
		t.Fatalf("flushed = %+v, want [{a} {b}]", out)
	}
	if b.Len() != 0 {
		t.Fatalf("b.Len() = %d after flush, want 0 (source must be reset)", b.Len())
	}
}

func TestEmptyFlushIsSafe(t *testing.T) {
	t.Parallel()

	var b Batch
	out := Flush(&b)
	if len(out) != 0 {
		t.Fatalf("empty flush returned %d events, want 0", len(out))
	}
	// A second flush must also be safe.
	if out := Flush(&b); len(out) != 0 {
		t.Fatalf("second empty flush returned %d, want 0", len(out))
	}
}

func TestFlushResetsCaller(t *testing.T) {
	t.Parallel()

	var b Batch
	b.Add(Event{Name: "x"})
	_ = Flush(&b)
	if b.Len() != 0 {
		t.Fatalf("caller's b.Len() = %d, want 0 (reset through pointer must land)", b.Len())
	}
}

func TestRefillAfterFlush(t *testing.T) {
	t.Parallel()

	var b Batch
	b.Add(Event{Name: "old"})
	old := Flush(&b)

	b.Add(Event{Name: "new"})
	if b.Len() != 1 {
		t.Fatalf("b.Len() = %d after refill, want 1", b.Len())
	}
	// The previously flushed slice is undisturbed by the reset and refill.
	if len(old) != 1 || old[0].Name != "old" {
		t.Fatalf("old flush = %+v, want [{old}]", old)
	}
}

func Example() {
	var b Batch
	b.Add(Event{Name: "login"})
	out := Flush(&b)
	fmt.Println(len(out), b.Len())
	// Output: 1 0
}
```

## Review

The pattern is correct when one call both drains and resets: `Flush` returns the
accumulated events and, via `*dst = Batch{}`, leaves the caller's batch empty and
reusable. Passing `dst *Batch` is required — a value parameter would reset a copy
and the caller's batch would stay full, the "my flush did not clear the buffer"
bug. The slice-header semantics are the subtle part: grabbing `out := dst.events`
before the reset keeps the flushed events valid because `*dst = Batch{}` only zeroes
`dst`'s header, not the backing array `out` still references. `TestRefillAfterFlush`
pins both halves — the batch refills cleanly and the earlier flush is undisturbed.
Flushing an empty batch is a no-op that returns an empty slice, never a panic. Run
`go test -race`.

## Resources

- [Go Language Specification: Assignments](https://go.dev/ref/spec#Assignments) — a whole-value assignment through `*p`.
- [Go Blog: Slices (usage and internals)](https://go.dev/blog/slices-intro) — the slice header and backing array.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-nil-pointer-deref-defense.md](09-nil-pointer-deref-defense.md) | Next: [../02-pointers-and-function-parameters/00-concepts.md](../02-pointers-and-function-parameters/00-concepts.md)
