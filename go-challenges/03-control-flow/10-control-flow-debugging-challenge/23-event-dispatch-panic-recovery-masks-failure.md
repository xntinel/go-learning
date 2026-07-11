# Exercise 23: Event Dispatcher Panic Recovery Silently Masks Type Assertion Errors Instead of Returning Them

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An event dispatcher that routes on concrete type with a `switch v :=
e.(type)` is only as safe as the assertions each arm performs on the
fields it pulls out. A field typed `any` because the wire format cannot
guarantee its shape — a payment reference that is *usually* a string —
tempts an unchecked `v.Ref.(string)`, which panics the moment a producer
sends something else. A `recover` at the dispatcher's boundary is the
right instinct, but a `recover` that discards what it caught instead of
converting it into a returned error turns "one malformed event" into "the
caller believes every event succeeded, including the one that never
reached its handler." This module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
eventdispatch/               independent module: example.com/event-dispatch-panic-recovery-masks-failure
  go.mod
  eventdispatch.go            Event types, Sink, Dispatch
  cmd/
    demo/
      main.go                 runnable demo: a well-formed and a malformed PaymentReceived
  eventdispatch_test.go        table over every event shape, plus an edge case for post-panic reuse
```

- Files: `eventdispatch.go`, `cmd/demo/main.go`, `eventdispatch_test.go`.
- Implement: `Dispatch(e Event, sink *Sink) error` that routes via type switch, recovers from any panic during handling, and returns it as an error instead of `nil`.
- Test: a table over every known event type, an unknown type, a nil event, and a `PaymentReceived` whose `Ref` is not a string; a further test asserts a recovered panic does not corrupt the sink or break the next dispatch.
- Verify: `go test -count=1 -race ./...`.

```bash
mkdir -p ~/go-exercises/event-dispatch-panic-recovery-masks-failure/cmd/demo
cd ~/go-exercises/event-dispatch-panic-recovery-masks-failure
go mod init example.com/event-dispatch-panic-recovery-masks-failure
```

### Why recover has to produce an error, not just stop the crash

The buggy `Dispatch` recovers correctly — the process does not crash — but
throws away exactly the information a caller needs:

```go
defer func() {
	recover() // BUG: discards the panic value; err stays nil either way
}()
```

`recover()`'s return value is the panic's payload; calling it and ignoring
the result still stops the unwind, which is why this version "works" in
the sense that the program keeps running. But `err` is a named return that
was never assigned anywhere on this path — the function panicked partway
through the `PaymentReceived` case, before reaching any `return`
statement — so it keeps its zero value, `nil`. The caller's
`if err != nil { ... }` never fires. The event that panicked was never
recorded to the sink, and nothing about the return value says so; from the
outside, a dropped event and a successfully processed one are
indistinguishable. This is worse than a crash: a crash pages someone, a
silent `nil` does not. The fix assigns `err` from inside the recover
closure:

```go
defer func() {
	if r := recover(); r != nil {
		err = fmt.Errorf("event dispatch panicked handling %T: %v", e, r)
	}
}()
```

Now `recover` still contains the panic — the process still does not
crash — but it *fully owns what happens next*, converting the panic into
the same kind of error every other failure path in `Dispatch` already
returns, so a caller cannot tell the difference between "the type switch's
`default` rejected this" and "a case panicked partway through": both come
back as a non-nil `error`.

Create `eventdispatch.go`:

```go
package eventdispatch

import "fmt"

// Event is any domain event carried through the dispatcher.
type Event interface{}

// OrderCreated, OrderCancelled, and PaymentReceived are the known event
// types the dispatcher routes. PaymentReceived.Ref is deliberately typed
// as any: most producers send a string reference, but the field's static
// type does not guarantee that at compile time.
type OrderCreated struct {
	ID     string
	Amount float64
}

type OrderCancelled struct {
	ID string
}

type PaymentReceived struct {
	ID  string
	Ref any
}

// Sink records the side effect of a successfully dispatched event.
type Sink struct {
	Records []string
}

func (s *Sink) Record(msg string) { s.Records = append(s.Records, msg) }

// Dispatch routes e to its handler and recovers from any panic raised
// while handling it -- such as an unchecked type assertion on data that
// turned out not to be what the handler assumed -- converting the panic
// into a returned error instead of letting it crash the caller, and
// instead of silently discarding the event as a false success.
func Dispatch(e Event, sink *Sink) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("event dispatch panicked handling %T: %v", e, r)
		}
	}()

	switch v := e.(type) {
	case OrderCreated:
		sink.Record(fmt.Sprintf("order created: %s $%.2f", v.ID, v.Amount))
	case OrderCancelled:
		sink.Record(fmt.Sprintf("order cancelled: %s", v.ID))
	case PaymentReceived:
		ref := v.Ref.(string) // may panic: Ref is not statically guaranteed to be a string
		sink.Record(fmt.Sprintf("payment received: %s ref=%s", v.ID, ref))
	default:
		return fmt.Errorf("unhandled event type %T", e)
	}
	return nil
}
```

### The runnable demo

The demo dispatches a well-formed `PaymentReceived` and a malformed one
whose `Ref` is an `int`, showing the malformed one is reported as an error
and never recorded.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/event-dispatch-panic-recovery-masks-failure"
)

func main() {
	sink := &eventdispatch.Sink{}
	stream := []eventdispatch.Event{
		eventdispatch.OrderCreated{ID: "o-1", Amount: 42.00},
		eventdispatch.PaymentReceived{ID: "p-1", Ref: "txn-abc"},
		eventdispatch.PaymentReceived{ID: "p-2", Ref: 12345}, // malformed: Ref is an int, not a string
		eventdispatch.OrderCancelled{ID: "o-2"},
	}

	for _, e := range stream {
		if err := eventdispatch.Dispatch(e, sink); err != nil {
			fmt.Printf("DROP %T: %v\n", e, err)
			continue
		}
		fmt.Printf("OK   %T\n", e)
	}

	fmt.Println("recorded:", sink.Records)
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
OK   eventdispatch.OrderCreated
OK   eventdispatch.PaymentReceived
DROP eventdispatch.PaymentReceived: event dispatch panicked handling eventdispatch.PaymentReceived: interface conversion: interface {} is int, not string
OK   eventdispatch.OrderCancelled
recorded: [order created: o-1 $42.00 payment received: p-1 ref=txn-abc order cancelled: o-2]
```

### Tests

`TestDispatchTable` covers every known event type, an unknown type, a nil
event, and the malformed `PaymentReceived` that triggers the panic — every
error row asserts the sink recorded nothing, which is what the buggy
version gets wrong: it also records nothing, but reports `nil`.
`TestDispatchRecoversWithoutPoisoningLaterCalls` is the edge case: after a
recovered panic, the very next `Dispatch` call against the same `Sink`
must behave completely normally.

Create `eventdispatch_test.go`:

```go
package eventdispatch

import (
	"strings"
	"testing"
)

func TestDispatchTable(t *testing.T) {
	tests := []struct {
		name        string
		event       Event
		wantErr     bool
		wantErrText string // substring, checked when wantErr is true
		wantRecords []string
	}{
		{
			name:        "order created records and succeeds",
			event:       OrderCreated{ID: "o-1", Amount: 42},
			wantRecords: []string{"order created: o-1 $42.00"},
		},
		{
			name:        "order cancelled records and succeeds",
			event:       OrderCancelled{ID: "o-2"},
			wantRecords: []string{"order cancelled: o-2"},
		},
		{
			name:        "payment received with a string ref succeeds",
			event:       PaymentReceived{ID: "p-1", Ref: "txn-abc"},
			wantRecords: []string{"payment received: p-1 ref=txn-abc"},
		},
		{
			name:        "payment received with a non-string ref returns an error, not nil",
			event:       PaymentReceived{ID: "p-2", Ref: 12345},
			wantErr:     true,
			wantErrText: "PaymentReceived",
			wantRecords: nil,
		},
		{
			name:        "unknown event type is rejected",
			event:       "not an event",
			wantErr:     true,
			wantErrText: "unhandled event type",
			wantRecords: nil,
		},
		{
			name:        "nil event is rejected",
			event:       nil,
			wantErr:     true,
			wantErrText: "unhandled event type",
			wantRecords: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := &Sink{}
			err := Dispatch(tc.event, sink)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("Dispatch = nil error, want an error containing %q (the event must not be silently dropped)", tc.wantErrText)
				}
				if !strings.Contains(err.Error(), tc.wantErrText) {
					t.Fatalf("Dispatch error = %q, want it to contain %q", err.Error(), tc.wantErrText)
				}
			} else if err != nil {
				t.Fatalf("Dispatch = %v, want nil", err)
			}

			if len(sink.Records) != len(tc.wantRecords) {
				t.Fatalf("records = %v, want %v", sink.Records, tc.wantRecords)
			}
			for i, want := range tc.wantRecords {
				if sink.Records[i] != want {
					t.Fatalf("records[%d] = %q, want %q", i, sink.Records[i], want)
				}
			}
		})
	}
}

// TestDispatchRecoversWithoutPoisoningLaterCalls is the edge case: a
// recovered panic on one event must not corrupt the sink or break the very
// next Dispatch call against the same sink.
func TestDispatchRecoversWithoutPoisoningLaterCalls(t *testing.T) {
	sink := &Sink{}

	err := Dispatch(PaymentReceived{ID: "p-bad", Ref: 999}, sink)
	if err == nil {
		t.Fatal("Dispatch = nil error for a malformed payment, want an error")
	}
	if len(sink.Records) != 0 {
		t.Fatalf("records after the panic = %v, want none", sink.Records)
	}

	err = Dispatch(OrderCreated{ID: "o-after", Amount: 10}, sink)
	if err != nil {
		t.Fatalf("Dispatch after a recovered panic = %v, want nil", err)
	}
	if len(sink.Records) != 1 || sink.Records[0] != "order created: o-after $10.00" {
		t.Fatalf("records after a recovered panic = %v, want the next event recorded normally", sink.Records)
	}
}
```

Run: `go test -count=1 -race ./...`.

## Review

`Dispatch` is correct when every panic during handling comes back as a
non-nil `error` that mentions the event type that caused it — not when
the process merely avoids crashing. `recover()` stopping the unwind and
`recover()` reporting the failure are two different responsibilities, and
a `recover` that only does the first one is worse than no `recover` at
all: an unhandled panic at least crashes loudly, while a `recover` that
swallows the value converts a loud, page-worthy failure into a quiet
`nil` that looks exactly like success. Once a boundary decides to recover
from a panic, it takes on full ownership of what the caller sees next —
which here means assigning the named return `err`, not merely calling
`recover()` for its side effect of stopping the crash.

## Resources

- [Go Specification: Handling panics](https://go.dev/ref/spec#Handling_panics) — `recover` only has an effect when called directly by a deferred function; its return value is the panic's argument.
- [Effective Go: Recover](https://go.dev/doc/effective_go#recover) — the canonical recover-to-error boundary pattern.
- [Go Specification: Type assertions](https://go.dev/ref/spec#Type_assertions) — a single-value type assertion `x.(T)` panics on failure; the two-value form `v, ok := x.(T)` does not.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-worker-pool-goroutine-leak-on-shutdown-race.md](22-worker-pool-goroutine-leak-on-shutdown-race.md) | Next: [24-saga-rollback-loop-scope-error.md](24-saga-rollback-loop-scope-error.md)
