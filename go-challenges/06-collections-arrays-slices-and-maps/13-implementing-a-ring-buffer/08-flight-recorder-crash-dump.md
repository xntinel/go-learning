# Exercise 8: Flight-Recorder Ring — Dump Recent Events on Panic

A flight recorder keeps a cheap, always-on ring of the last N breadcrumbs and only
materializes them when something goes wrong. This module builds a `Recorder` that
records structured events into a ring at O(1) per event and exposes `Dump()` — called
from a deferred `recover()` — to emit the recent history plus the stack to a crash
log for postmortem. It costs almost nothing in the happy path and pays off exactly
when you need it.

Self-contained: its own module, the recorder, a demo, and tests that trigger and
recover a panic.

## What you'll build

```text
flightrecorder/            independent module: example.com/flightrecorder
  go.mod                   go 1.24
  recorder.go              Event, Recorder (Record, Snapshot, Dump)
  cmd/
    demo/
      main.go              record breadcrumbs, panic, recover, dump to stderr
  recorder_test.go         last-N captured in order, dump includes stack, no panic
```

Files: `recorder.go`, `cmd/demo/main.go`, `recorder_test.go`.
Implement: `Recorder` over a ring of `Event`; `Record(msg, kv...)`, `Snapshot()`, `Dump(w, cause)`.
Test: record more than capacity then panic under a deferred recover; assert `Dump` captured exactly the last N in order plus the stack; recording on the panic path does not itself panic.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/flightrecorder/cmd/demo
cd ~/go-exercises/flightrecorder
go mod init example.com/flightrecorder
go mod edit -go=1.24
```

### The always-on cheap path, the rare expensive path

The design split is the point. `Record` runs on every meaningful step of a request:
it takes the mutex, pushes one `Event` (a timestamp, a message, and a few key/value
attributes) into the ring, and returns. That is O(1) and allocation-bounded — the
ring never grows, so a process can call `Record` millions of times without leaking.
Because it is so cheap, you can leave it on in production permanently, unlike verbose
logging.

`Dump` runs only on failure. From a deferred function, after `recover()` catches a
panic, `Dump` takes a `Snapshot` of the recent events, captures the current stack
with `runtime/debug.Stack()`, and writes both to a crash sink (stderr, a file, a
structured logger). The postmortem then has the last N breadcrumbs that led to the
crash plus the goroutine stack at the point of panic — far more useful than a bare
stack trace with no context.

### Snapshot on the failure path must not itself fail

A subtle requirement: the code that runs while handling a panic must be robust. If
`Dump` allocated unboundedly, or `Record` on the recovery path could itself panic
(nil map, out-of-range), you would lose the crash report to a second failure. So the
ring's `Snapshot` allocates exactly `size` events and no more, `Record` guards its
map access, and `Dump` writes to an `io.Writer` the caller supplies (so the test can
pass a `bytes.Buffer` and the production path can pass `os.Stderr`). The recover
handler catches the original panic, dumps, and — depending on policy — either
re-panics to crash the process with the report already emitted, or swallows the panic
to keep a server goroutine alive. This module demonstrates recover-dump-continue; a
process-level recorder would recover-dump-repanic.

### Using log/slog for structured output

`Dump` formats each event as a line and can route through `log/slog` for structured
crash logs. Here `Dump` writes plain lines to the provided writer to keep the output
deterministic and testable, and the demo shows the `slog` variant. The events carry
key/value attributes so a breadcrumb like `Record("db query", "table", "orders",
"rows", 3)` is self-describing in the dump.

Create `recorder.go`:

```go
package flightrecorder

import (
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Event is one recorded breadcrumb.
type Event struct {
	Time    time.Time
	Message string
	Attrs   []any // alternating key, value pairs
}

type ring struct {
	data []Event
	head int
	tail int
	size int
}

func newRing(capacity int) *ring {
	if capacity <= 0 {
		capacity = 1
	}
	return &ring{data: make([]Event, capacity)}
}

func (r *ring) push(e Event) {
	r.data[r.head] = e
	r.head = (r.head + 1) % len(r.data)
	if r.size < len(r.data) {
		r.size++
	} else {
		r.tail = (r.tail + 1) % len(r.data)
	}
}

func (r *ring) snapshot() []Event {
	out := make([]Event, r.size)
	for i := range r.size {
		out[i] = r.data[(r.tail+i)%len(r.data)]
	}
	return out
}

// Recorder keeps the most recent N events for crash-time postmortem. It is safe
// for concurrent Record and Dump.
type Recorder struct {
	mu  sync.Mutex
	r   *ring
	now func() time.Time // injectable for deterministic tests
}

// NewRecorder returns a Recorder holding at most capacity events.
func NewRecorder(capacity int) *Recorder {
	return &Recorder{r: newRing(capacity), now: time.Now}
}

// Record appends a breadcrumb with optional alternating key/value attributes.
func (rec *Recorder) Record(msg string, kv ...any) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	attrs := make([]any, len(kv))
	copy(attrs, kv)
	rec.r.push(Event{Time: rec.now(), Message: msg, Attrs: attrs})
}

// Snapshot returns an independent copy of the recorded events, oldest first.
func (rec *Recorder) Snapshot() []Event {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.r.snapshot()
}

// Dump writes the recent events and the current stack to w, prefixed by cause.
// It is intended to be called from a deferred recover handler.
func (rec *Recorder) Dump(w io.Writer, cause any) {
	events := rec.Snapshot()
	fmt.Fprintf(w, "PANIC: %v\n", cause)
	fmt.Fprintf(w, "recent events (%d):\n", len(events))
	for _, e := range events {
		var b strings.Builder
		b.WriteString(e.Message)
		for i := 0; i+1 < len(e.Attrs); i += 2 {
			fmt.Fprintf(&b, " %v=%v", e.Attrs[i], e.Attrs[i+1])
		}
		fmt.Fprintf(w, "  - %s\n", b.String())
	}
	fmt.Fprintf(w, "stack:\n%s", debug.Stack())
}
```

### The runnable demo

The demo records three breadcrumbs, then a function panics; a deferred handler
recovers, dumps to a buffer, and the program continues to a clean exit. To keep the
output deterministic, the demo prints the deterministic cause+event lines to stdout
and routes the variable stack to stderr.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"example.com/flightrecorder"
)

func main() {
	rec := flightrecorder.NewRecorder(4)
	rec.Record("accept request", "path", "/checkout")
	rec.Record("load cart", "items", 3)
	rec.Record("charge card", "amount", 1999)

	risky(rec)

	fmt.Println("server still alive after recovered panic")
}

func risky(rec *flightrecorder.Recorder) {
	defer func() {
		cause := recover()
		if cause == nil {
			return
		}
		var buf bytes.Buffer
		rec.Dump(&buf, cause)
		out := buf.String()
		// Deterministic part (cause + events) to stdout; the stack to stderr.
		if i := strings.Index(out, "stack:\n"); i >= 0 {
			fmt.Print(out[:i])
			fmt.Fprint(os.Stderr, out[i:])
		} else {
			fmt.Print(out)
		}
	}()
	rec.Record("call payment gateway", "attempt", 1)
	panic("gateway timeout")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
PANIC: gateway timeout
recent events (4):
  - accept request path=/checkout
  - load cart items=3
  - charge card amount=1999
  - call payment gateway attempt=1
server still alive after recovered panic
```

Four events were recorded into a capacity-4 ring, so none were evicted; the dump
shows all four in insertion order, then the recovered program continues to its
final line. Increase the load past capacity 4 and the oldest breadcrumbs drop off
the front — the recorder always holds the freshest N.

### Tests

The tests trigger a panic inside a function with a deferred recover that dumps to a
`bytes.Buffer`, then assert the dump contains the last N events in order and the
stack marker, and that the whole path recovered cleanly. A fixed clock is injected so
timestamps do not appear in the asserted lines (the dump prints messages and attrs,
not times, keeping the assertion stable). A second test proves recording during the
recover handler does not panic.

Create `recorder_test.go`:

```go
package flightrecorder

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestDumpCapturesLastNInOrder(t *testing.T) {
	t.Parallel()
	rec := NewRecorder(3)
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }

	var buf bytes.Buffer
	func() {
		defer func() {
			if cause := recover(); cause != nil {
				rec.Dump(&buf, cause)
			}
		}()
		rec.Record("step1")
		rec.Record("step2")
		rec.Record("step3")
		rec.Record("step4") // evicts step1
		panic("boom")
	}()

	out := buf.String()
	if !strings.Contains(out, "PANIC: boom") {
		t.Fatalf("dump missing panic cause:\n%s", out)
	}
	// Capacity 3: step1 evicted, last three remain in order.
	for _, want := range []string{"step2", "step3", "step4"} {
		if !strings.Contains(out, "- "+want+"\n") {
			t.Fatalf("dump missing event %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "- step1\n") {
		t.Fatalf("dump contains evicted event step1:\n%s", out)
	}
	if !strings.Contains(out, "stack:\n") {
		t.Fatalf("dump missing stack section:\n%s", out)
	}
	// Order: step2 before step3 before step4.
	i2 := strings.Index(out, "step2")
	i3 := strings.Index(out, "step3")
	i4 := strings.Index(out, "step4")
	if !(i2 < i3 && i3 < i4) {
		t.Fatalf("events out of order in dump:\n%s", out)
	}
}

func TestAttrsAppearInDump(t *testing.T) {
	t.Parallel()
	rec := NewRecorder(4)
	rec.Record("db query", "table", "orders", "rows", 3)

	var buf bytes.Buffer
	rec.Dump(&buf, "manual")
	if got := buf.String(); !strings.Contains(got, "db query table=orders rows=3") {
		t.Fatalf("attrs not rendered: %q", got)
	}
}

func TestRecordOnRecoverPathDoesNotPanic(t *testing.T) {
	t.Parallel()
	rec := NewRecorder(2)
	// A defensive recover handler that records and dumps must not itself panic.
	func() {
		defer func() {
			cause := recover()
			rec.Record("recovering", "cause", cause) // must be safe
			var buf bytes.Buffer
			rec.Dump(&buf, cause)
		}()
		panic("original")
	}()
	if got := len(rec.Snapshot()); got != 1 {
		t.Fatalf("recorded events after recovery = %d, want 1", got)
	}
}
```

## Review

The recorder is correct when `Record` is O(1) and bounded, `Dump` emits exactly the
surviving last-N events in FIFO order plus the stack, and nothing on the recovery
path can fail a second time. `TestDumpCapturesLastNInOrder` proves eviction and order
together — the evicted `step1` must be absent and `step2..4` present in sequence —
and checks the stack section is present. The trap is doing anything unbounded or
panic-prone in the failure path: `Snapshot` allocates exactly `size`, `Record`
copies its variadic attrs into a fresh slice so a caller cannot alias and mutate the
stored event, and `Dump` writes to a caller-supplied `io.Writer` so the crash sink is
the caller's choice. In production, decide the policy consciously: dump-and-repanic to
crash with a report, or dump-and-continue to keep a server goroutine alive.

## Resources

- [`runtime/debug`](https://pkg.go.dev/runtime/debug#Stack) — `debug.Stack` for capturing the goroutine stack at panic time.
- [Effective Go: recover](https://go.dev/doc/effective_go#recover) — the defer/recover pattern this module builds on.
- [`log/slog`](https://pkg.go.dev/log/slog) — structured logging for routing crash dumps.

---

Back to [07-blocking-bounded-queue-cond.md](07-blocking-bounded-queue-cond.md) | Next: [09-drop-counter-backpressure-metrics.md](09-drop-counter-backpressure-metrics.md)
