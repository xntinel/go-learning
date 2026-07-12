# Exercise 2: Zero-Value-Ready Types: an EventBatch Usable Without a Constructor

The most ergonomic types in the standard library — `bytes.Buffer`, `sync.Mutex`,
`sync.WaitGroup` — need no constructor: `var b bytes.Buffer` just works. This
exercise builds an `EventBatch` (a request-scoped buffer of analytics events)
with the same property, so `var b EventBatch`, `b := EventBatch{}`, and
`new(EventBatch)` are all immediately usable. The trick is lazy initialization:
the backing store is created on first write, and read methods tolerate the nil
backing store.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
eventbatch/                   independent module: example.com/eventbatch
  go.mod                      go 1.26
  batch.go                    EventBatch (lazy map+slice init), Add, Len, Count, Flush; Counter
  cmd/
    demo/
      main.go                 runnable demo across all three construction paths
  batch_test.go               table test over the three paths + zero-value read safety + once-init
```

Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
Implement: an `EventBatch` whose `Add` lazily initializes the backing slice and
map on first use, whose `Len`/`Count`/`Flush` work on the zero value, plus a
small `Counter` demonstrating lazy map init.
Test: the same `Add`/`Flush` sequence through `var b EventBatch`,
`b := EventBatch{}`, and `b := new(EventBatch)` yields identical results; reading
`Len`/`Count` and ranging on a never-written zero batch returns 0 and does not
panic; `Add` on the zero value initializes the backing store exactly once.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/03-new-vs-composite-literal/02-zero-value-ready-batch/cmd/demo
cd go-solutions/09-pointers/03-new-vs-composite-literal/02-zero-value-ready-batch
```

### Why the zero value must be valid

An `EventBatch` collects events during one request and flushes them at the end.
The natural way to declare one at the top of a handler is `var b EventBatch` — no
constructor call, no error to handle, no import to remember. For that to be
correct, the zero value must be a valid empty batch. But the zero value of a slice
field is `nil` and the zero value of a map field is `nil`, and while ranging over
a nil slice or nil map is legal and `len` of them is 0, *writing* to a nil map
panics. So the batch cannot eagerly assume its backing store exists; it must
create it lazily on the first `Add`.

That is the whole design. `Add` checks whether the backing store is nil and
allocates it (with `make`) only the first time. `Len`, `Count`, and `Flush` are
written to tolerate a nil backing store: `len(nil)` is 0, ranging over nil yields
nothing, and `Flush` returns whatever is there (possibly nothing). The result is a
type where `var b EventBatch`, `EventBatch{}`, and `new(EventBatch)` are
interchangeable and all immediately usable — exactly the `bytes.Buffer` ergonomic.
An exported `NewEventBatch` constructor becomes optional; you can offer one for
callers who prefer it, but nothing breaks without it.

The `Counter` is the same idea at minimum size: a map-valued counter whose `Inc`
lazily creates the map, so `var c Counter; c.Inc("x")` does not panic on a nil
map write. Reading a missing key from a nil map returns the zero value (`0`)
without panicking, which is why `Get` needs no lazy init at all.

Create `batch.go`:

```go
package eventbatch

// Event is one analytics event collected during a request.
type Event struct {
	Name string
	User string
}

// EventBatch buffers events for one request. Its zero value is a valid empty
// batch: var b EventBatch, EventBatch{}, and new(EventBatch) are all usable with
// no constructor. The backing store is created lazily on the first Add.
type EventBatch struct {
	events []Event
	counts map[string]int // per-name count, built lazily
}

// Add appends an event, lazily initializing the backing store on first use.
func (b *EventBatch) Add(e Event) {
	if b.counts == nil {
		// First write: allocate both backing structures. A nil map write would
		// panic, so this lazy init is mandatory, not optional.
		b.counts = make(map[string]int)
		b.events = make([]Event, 0, 8)
	}
	b.events = append(b.events, e)
	b.counts[e.Name]++
}

// Len reports the number of buffered events. Safe on the zero value: len(nil)==0.
func (b *EventBatch) Len() int {
	return len(b.events)
}

// Count reports how many events with the given name were added. Safe on the zero
// value: reading a nil map returns the zero value without panicking.
func (b *EventBatch) Count(name string) int {
	return b.counts[name]
}

// Flush returns the buffered events and resets the batch to empty. Safe on the
// zero value: it returns a nil slice and leaves the batch empty.
func (b *EventBatch) Flush() []Event {
	out := b.events
	b.events = nil
	b.counts = nil
	return out
}

// Counter is a zero-value-ready per-key counter. var c Counter is usable.
type Counter struct {
	m map[string]int
}

// Inc increments the count for key, lazily creating the map on first use.
func (c *Counter) Inc(key string) {
	if c.m == nil {
		c.m = make(map[string]int)
	}
	c.m[key]++
}

// Get returns the count for key. Reading a nil map yields 0 without panicking.
func (c *Counter) Get(key string) int {
	return c.m[key]
}
```

### The runnable demo

The demo drives all three construction paths through the same sequence and prints
their results so you can see they are identical.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/eventbatch"
)

func drive(b *eventbatch.EventBatch) (int, int) {
	b.Add(eventbatch.Event{Name: "login", User: "alice"})
	b.Add(eventbatch.Event{Name: "login", User: "bob"})
	b.Add(eventbatch.Event{Name: "logout", User: "alice"})
	return b.Len(), b.Count("login")
}

func main() {
	var viaVar eventbatch.EventBatch
	l1, c1 := drive(&viaVar)
	fmt.Printf("var:  len=%d login=%d\n", l1, c1)

	viaLiteral := eventbatch.EventBatch{}
	l2, c2 := drive(&viaLiteral)
	fmt.Printf("lit:  len=%d login=%d\n", l2, c2)

	viaNew := new(eventbatch.EventBatch)
	l3, c3 := drive(viaNew)
	fmt.Printf("new:  len=%d login=%d\n", l3, c3)

	// A never-written zero batch is safe to read.
	var empty eventbatch.EventBatch
	fmt.Printf("empty: len=%d login=%d flushed=%d\n",
		empty.Len(), empty.Count("login"), len(empty.Flush()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
var:  len=3 login=2
lit:  len=3 login=2
new:  len=3 login=2
empty: len=0 login=0 flushed=0
```

### Tests

The table test drives the three construction paths through one sequence and
asserts identical results. `TestZeroValueReadsAreSafe` proves a never-written
batch reads as empty and does not panic when ranged over. `TestLazyInitOnce`
proves the backing store is created exactly once by capturing the map identity
after the first and second `Add` and asserting it does not change.

Create `batch_test.go`:

```go
package eventbatch

import (
	"fmt"
	"testing"
)

func TestConstructionPathsAreEquivalent(t *testing.T) {
	t.Parallel()

	seq := []Event{
		{Name: "login", User: "alice"},
		{Name: "login", User: "bob"},
		{Name: "logout", User: "alice"},
	}

	tests := []struct {
		name string
		make func() *EventBatch
	}{
		{"var", func() *EventBatch { var b EventBatch; return &b }},
		{"literal", func() *EventBatch { b := EventBatch{}; return &b }},
		{"new", func() *EventBatch { return new(EventBatch) }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := tc.make()
			for _, e := range seq {
				b.Add(e)
			}
			if got := b.Len(); got != 3 {
				t.Fatalf("Len() = %d, want 3", got)
			}
			if got := b.Count("login"); got != 2 {
				t.Fatalf("Count(login) = %d, want 2", got)
			}
			if got := len(b.Flush()); got != 3 {
				t.Fatalf("Flush() len = %d, want 3", got)
			}
			if got := b.Len(); got != 0 {
				t.Fatalf("Len() after Flush = %d, want 0", got)
			}
		})
	}
}

func TestZeroValueReadsAreSafe(t *testing.T) {
	t.Parallel()

	var b EventBatch
	if b.Len() != 0 {
		t.Fatalf("Len() = %d, want 0 on zero value", b.Len())
	}
	if b.Count("missing") != 0 {
		t.Fatalf("Count(missing) = %d, want 0 on zero value", b.Count("missing"))
	}
	// Ranging over the flushed nil slice must not panic.
	n := 0
	for range b.Flush() {
		n++
	}
	if n != 0 {
		t.Fatalf("ranged %d events, want 0", n)
	}

	var c Counter
	if c.Get("x") != 0 {
		t.Fatal("Counter.Get on zero value should be 0")
	}
}

func TestLazyInitOnce(t *testing.T) {
	t.Parallel()

	var b EventBatch
	b.Add(Event{Name: "a"})
	first := fmt.Sprintf("%p", b.counts)
	b.Add(Event{Name: "b"})
	second := fmt.Sprintf("%p", b.counts)

	if first != second {
		t.Fatalf("backing map re-created: %s then %s; want one init", first, second)
	}
	if first == "0x0" {
		t.Fatal("backing map was never initialized after Add")
	}
}

func ExampleEventBatch_zeroValue() {
	var b EventBatch // no constructor
	b.Add(Event{Name: "click", User: "alice"})
	b.Add(Event{Name: "click", User: "bob"})
	fmt.Printf("len=%d clicks=%d\n", b.Len(), b.Count("click"))
	// Output: len=2 clicks=2
}
```

## Review

The type is correct when the three construction paths are truly interchangeable —
the table test asserts identical `Len`, `Count`, and `Flush` results for `var`,
`EventBatch{}`, and `new` — and when a never-written zero value reads as empty
without panicking. The single most important line is the `if b.counts == nil`
guard in `Add`: without it, `var b EventBatch; b.Add(...)` panics on a nil-map
write, and the zero-value ergonomic is lost. `TestLazyInitOnce` guards the other
failure mode: re-allocating the backing map on every `Add` (dropping the guard's
`else` intent) would silently discard earlier counts. Reading a nil map is always
safe and returns the zero value, which is why `Count` and `Get` need no guard.
This is exactly how `bytes.Buffer` avoids forcing a constructor on its callers.

## Resources

- [Dave Cheney: What is the zero value, and why is it useful?](https://dave.cheney.net/2013/01/19/what-is-the-zero-value-and-why-is-it-useful) — the design case for zero-value-ready types.
- [Go Specification: The zero value](https://go.dev/ref/spec#The_zero_value) — what fields a freshly allocated struct starts with.
- [bytes.Buffer](https://pkg.go.dev/bytes#Buffer) — the canonical "usable zero value, no constructor" type.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-functional-options-server-config.md](03-functional-options-server-config.md)
