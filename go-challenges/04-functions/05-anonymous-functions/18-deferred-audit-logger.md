# Exercise 18: Deferred Closure that Instruments Handler Observability via Named Return

**Nivel: Intermedio** — validacion rapida (un test corto).

Instrumenting a handler — logging its duration and outcome, converting a panic into
an ordinary error instead of a crash — reads best as one deferred closure wrapped
around the call, reading the named return value after the real work has run. This
module builds that instrumentation once, generically, so any operation can be
wrapped in a single line.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
audit/                        module example.com/audit
  go.mod
  audit.go                     Entry, Logger, Instrument (named return + deferred closure)
  audit_test.go                success, error, panic-to-error, ordering
  cmd/demo/main.go             three instrumented calls: ok, error, panic
```

- Files: `audit.go`, `audit_test.go`, `cmd/demo/main.go`.
- Implement: `Logger` collecting `Entry` values behind a mutex; `Instrument(l, op, now, fn) (err error)` with a named return read by a deferred closure that recovers a panic into `err`, then logs the operation, its duration, and `err`.
- Test: a successful call logs a nil error and the measured duration; a returned error is logged as-is; a panic is converted to a non-nil error and logged; multiple calls are logged in order.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/audit/cmd/demo
cd ~/go-exercises/audit
go mod init example.com/audit
go mod edit -go=1.24
```

### Reading the named return from inside the defer

`Instrument` declares `err` as a named return so the deferred closure can both read
and rewrite it after `fn` has already run. The closure does two things, in order.
First, `recover()` — if `fn` panicked, the panic value becomes the new `err`,
converting a crash into an ordinary returned error the caller can inspect. Second, it
logs an `Entry` built from `op`, the elapsed time, and — critically — the *current*
value of `err`, which by then reflects either `fn`'s real return value or the
recovered panic. If the closure logged `err` by copying it into a local before `fn`
ran, it would always log `nil`; reading the named return itself, after the deferred
closure's own recover has had a chance to run, is what makes the log accurate in all
three cases.

The clock is injected (`now func() time.Time`) so tests can measure a deterministic
duration instead of depending on real elapsed time.

Create `audit.go`:

```go
package audit

import (
	"fmt"
	"sync"
	"time"
)

// Entry is one instrumented call's outcome.
type Entry struct {
	Op       string
	Duration time.Duration
	Err      error
}

// Logger collects Entries from concurrent instrumented calls.
type Logger struct {
	mu      sync.Mutex
	entries []Entry
}

func (l *Logger) record(e Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
}

// Entries returns a copy of every recorded Entry, in the order it was
// recorded.
func (l *Logger) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

// Instrument runs fn and logs its outcome to l: the operation name, its
// duration measured with now, and its final error. The deferred closure
// reads the named return err *after* fn has run (or after a recover), which
// is the only way it can see the real outcome — and it uses recover to turn
// a panicking fn into an ordinary error instead of crashing the caller.
func Instrument(l *Logger, op string, now func() time.Time, fn func() error) (err error) {
	start := now()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in %s: %v", op, r)
		}
		l.record(Entry{Op: op, Duration: now().Sub(start), Err: err})
	}()
	err = fn()
	return err
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/audit"
)

func main() {
	l := &audit.Logger{}

	_ = audit.Instrument(l, "create-order", time.Now, func() error {
		return nil
	})
	_ = audit.Instrument(l, "charge-card", time.Now, func() error {
		return errors.New("card declined")
	})
	_ = audit.Instrument(l, "send-email", time.Now, func() error {
		panic("smtp connection lost")
	})

	for _, e := range l.Entries() {
		fmt.Printf("op=%s err=%v\n", e.Op, e.Err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
op=create-order err=<nil>
op=charge-card err=card declined
op=send-email err=panic in send-email: smtp connection lost
```

### Tests

`TestInstrumentLogsSuccess` uses a fake, fixed-step clock and checks a nil error and
the exact measured duration. `TestInstrumentLogsError` checks a returned error is
logged unchanged. `TestInstrumentConvertsPanicToError` checks a panicking `fn`
produces both a non-nil returned error and a matching logged entry.
`TestInstrumentLogsEachCallInOrder` checks two calls appear in call order.

Create `audit_test.go`:

```go
package audit

import (
	"errors"
	"testing"
	"time"
)

// fakeClock advances by one fixed step per call, so Duration is
// deterministic without sleeping or reading the real wall clock.
func fakeClock(step time.Duration) func() time.Time {
	t := time.Unix(0, 0)
	return func() time.Time {
		cur := t
		t = t.Add(step)
		return cur
	}
}

func TestInstrumentLogsSuccess(t *testing.T) {
	t.Parallel()
	l := &Logger{}
	clock := fakeClock(10 * time.Millisecond)

	err := Instrument(l, "op-ok", clock, func() error { return nil })
	if err != nil {
		t.Fatalf("Instrument() = %v, want nil", err)
	}

	entries := l.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Op != "op-ok" || entries[0].Err != nil {
		t.Fatalf("entries[0] = %+v, want op-ok with nil err", entries[0])
	}
	if entries[0].Duration != 10*time.Millisecond {
		t.Fatalf("entries[0].Duration = %v, want 10ms", entries[0].Duration)
	}
}

func TestInstrumentLogsError(t *testing.T) {
	t.Parallel()
	l := &Logger{}
	want := errors.New("boom")

	err := Instrument(l, "op-fail", fakeClock(time.Millisecond), func() error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("Instrument() = %v, want %v", err, want)
	}
	if entries := l.Entries(); !errors.Is(entries[0].Err, want) {
		t.Fatalf("logged err = %v, want %v", entries[0].Err, want)
	}
}

func TestInstrumentConvertsPanicToError(t *testing.T) {
	t.Parallel()
	l := &Logger{}

	err := Instrument(l, "op-panic", fakeClock(time.Millisecond), func() error {
		panic("kaboom")
	})
	if err == nil {
		t.Fatal("Instrument() = nil, want a converted panic error")
	}
	if got := l.Entries()[0].Err; got == nil {
		t.Fatal("logged entry has nil Err after a panic, want the converted error")
	}
}

func TestInstrumentLogsEachCallInOrder(t *testing.T) {
	t.Parallel()
	l := &Logger{}
	clock := fakeClock(time.Millisecond)

	_ = Instrument(l, "first", clock, func() error { return nil })
	_ = Instrument(l, "second", clock, func() error { return nil })

	entries := l.Entries()
	if len(entries) != 2 || entries[0].Op != "first" || entries[1].Op != "second" {
		t.Fatalf("entries = %+v, want [first second] in order", entries)
	}
}
```

## Review

The instrumentation is correct because the deferred closure reads `err` — the named
return — *after* both `fn`'s own assignment and its own `recover` have had a chance
to run, so whatever it logs is the value the caller actually receives. Getting the
order backwards (logging before `fn` runs, or logging a local copy taken before the
`defer`) is the classic bug this pattern guards against: the log would always show
success even when the real call failed or panicked. The panic-to-error conversion is
what lets a caller treat every call through `Instrument` uniformly, as a function
that returns an error, never one that can bring down the process.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Effective Go: Recover](https://go.dev/doc/effective_go#recover)
- [fmt.Errorf](https://pkg.go.dev/fmt#Errorf)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-iife-quota-inline.md](17-iife-quota-inline.md) | Next: [19-stream-sampler-goroutine-fanout.md](19-stream-sampler-goroutine-fanout.md)
