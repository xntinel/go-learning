# Exercise 18: Message Queue Batch Consumer with Per-Message Recovery and Classification

**Nivel: Intermedio** — validacion rapida (un test corto).

A consumer that pulls a batch off a queue and hands each message to a
handler cannot let one poison message stall the whole batch — but "isolate
the panic" alone is not enough for an operator to act on. Knowing *what kind*
of failure a message triggered (a genuine code bug versus a modeled
application error versus something unclassifiable) is what decides whether
it goes to a dead-letter queue for manual inspection or gets automatically
retried. This module builds `ConsumeBatch`, which processes every message,
isolates and classifies each panic, and returns both the full per-message
outcome and the list of message IDs that need to be routed to a dead
letter queue instead of committed normally. It is fully self-contained: its
own module, demo, and tests.

## What you'll build

```text
msgbatch/                    independent module: example.com/msgbatch
  go.mod                     go 1.24
  msgbatch.go                 PanicKind, Outcome, ConsumeBatch, consumeOne, classify
  cmd/
    demo/
      main.go                runnable demo: 5 messages, three different failure kinds
  msgbatch_test.go             classification table, all-clean batch, empty batch
```

Files: `msgbatch.go`, `cmd/demo/main.go`, `msgbatch_test.go`.
Implement: `ConsumeBatch(msgs []Message, handle func(Message) error) (outcomes []Outcome, deadLetter []string)` that isolates each message's panic in `consumeOne` and classifies it via `classify` into `KindRuntimeBug`, `KindAppError`, or `KindUnknown`.
Test: five messages where one triggers an index-out-of-range, one panics with an application error, and one panics with a bare string; assert all five are processed (the queue never stalls), each is classified correctly, `errors.Is` reaches the application error's sentinel, and the dead-letter list contains exactly the three panicking IDs in order; an all-clean batch and an empty batch are also covered.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why classification is a separate step from recovery, and what each kind means operationally

`consumeOne` is the recover boundary — one message wide, re-armed by the
surrounding loop on every iteration, exactly like isolating one batch item
elsewhere in this chapter. What is specific to a *queue* consumer is what
happens after recovery: `classify` turns the raw recovered value into a
`PanicKind` an operator or an automated pipeline can act on without reading
a stack trace. A `KindRuntimeBug` (detected via `errors.As` against the
`runtime.Error` interface, never by comparing concrete types) means the
handler itself has a bug — an index or nil-map access that will happen
again on redelivery, so this message belongs on a dead-letter queue for a
human, not an automatic retry. A `KindAppError` is a `panic(error)` the
handler chose deliberately, which may be transient (a downstream timeout)
and worth an automatic retry with backoff. `KindUnknown` — a bare string or
non-error value — is treated conservatively, the same as a runtime bug,
because there is no structured information to reason about a retry with.

The consumer never stalls on any of this: `ConsumeBatch` collects a full
`[]Outcome` in message order regardless of classification, and a separate
`deadLetter` slice of just the IDs that need special handling — so the
caller can commit the queue offset for the whole batch (nothing was lost or
skipped) while routing exactly the poison messages elsewhere, rather than
nacking the entire batch because of one bad record.

Create `msgbatch.go`:

```go
package msgbatch

import (
	"errors"
	"fmt"
	"runtime"
)

// Message is one record pulled off a queue.
type Message struct {
	ID      string
	Payload string
}

// PanicKind classifies what a recovered panic value turned out to be, so an
// operator reading the outcome knows at a glance whether a handler has a
// real bug (KindRuntimeBug) or hit a modeled failure (KindAppError), versus
// something that slipped past both (KindUnknown).
type PanicKind string

const (
	KindNone       PanicKind = ""
	KindRuntimeBug PanicKind = "runtime-bug"
	KindAppError   PanicKind = "app-error"
	KindUnknown    PanicKind = "unknown"
)

// Outcome is the per-message result, in batch order.
type Outcome struct {
	ID   string
	OK   bool
	Kind PanicKind
	Err  error
}

// ConsumeBatch runs handle over every message, isolating each in its own
// recover so one poison message cannot stall the queue: the batch is
// processed to the end regardless of how many messages panic, and every
// panicking message's ID is collected into deadLetter for separate
// re-delivery or manual inspection, rather than blocking the messages that
// come after it.
func ConsumeBatch(msgs []Message, handle func(Message) error) (outcomes []Outcome, deadLetter []string) {
	outcomes = make([]Outcome, 0, len(msgs))
	for _, m := range msgs {
		o := consumeOne(m, handle)
		outcomes = append(outcomes, o)
		if o.Kind != KindNone {
			deadLetter = append(deadLetter, o.ID)
		}
	}
	return outcomes, deadLetter
}

// consumeOne is the recover boundary: exactly one message's worth of
// untrusted handler logic.
func consumeOne(m Message, handle func(Message) error) (o Outcome) {
	o = Outcome{ID: m.ID, OK: true}
	defer func() {
		if r := recover(); r != nil {
			o.OK = false
			o.Kind, o.Err = classify(m.ID, r)
		}
	}()

	if err := handle(m); err != nil {
		o.OK = false
		o.Err = err
	}
	return o
}

// classify turns a recovered panic value into a PanicKind and a wrapped
// error that still lets errors.Is/errors.As reach the original cause.
func classify(id string, r any) (PanicKind, error) {
	e, ok := r.(error)
	if !ok {
		return KindUnknown, fmt.Errorf("message %s panicked: %v", id, r)
	}
	var rerr runtime.Error
	if errors.As(e, &rerr) {
		return KindRuntimeBug, fmt.Errorf("message %s panicked with a runtime bug: %w", id, rerr)
	}
	return KindAppError, fmt.Errorf("message %s panicked: %w", id, e)
}
```

### The runnable demo

Five messages: two clean, one triggers an index-out-of-range, one panics
with an application error, and one panics with a bare string.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/msgbatch"
)

func main() {
	msgs := []msgbatch.Message{
		{ID: "msg-1", Payload: "ok"},
		{ID: "msg-2", Payload: "index"},
		{ID: "msg-3", Payload: "ok"},
		{ID: "msg-4", Payload: "app"},
		{ID: "msg-5", Payload: "string"},
	}

	handle := func(m msgbatch.Message) error {
		switch m.Payload {
		case "index":
			bad := []int{1, 2}
			_ = bad[5]
		case "app":
			panic(errors.New("malformed record: missing field"))
		case "string":
			panic("unexpected shape")
		}
		return nil
	}

	outcomes, deadLetter := msgbatch.ConsumeBatch(msgs, handle)

	for _, o := range outcomes {
		status := "ok"
		if !o.OK {
			status = string(o.Kind)
		}
		fmt.Printf("%s: %s\n", o.ID, status)
	}
	fmt.Printf("dead letter: %v\n", deadLetter)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
msg-1: ok
msg-2: runtime-bug
msg-3: ok
msg-4: app-error
msg-5: unknown
dead letter: [msg-2 msg-4 msg-5]
```

### Tests

`TestConsumeBatchClassifiesAndContinues` drives all three panic kinds
through one batch, asserting every message is processed (the batch never
stalls), each is classified correctly, `errors.Is` reaches the application
error's sentinel, and the dead-letter list is exactly the three panicking
IDs, in order. `TestConsumeBatchAllClean` and `TestConsumeBatchEmpty` cover
the boundary cases.

Create `msgbatch_test.go`:

```go
package msgbatch

import (
	"errors"
	"testing"
)

func TestConsumeBatchClassifiesAndContinues(t *testing.T) {
	var processed []string
	sentinel := errors.New("bad json")

	msgs := []Message{
		{ID: "m1", Payload: "ok"},
		{ID: "m2", Payload: "index"},
		{ID: "m3", Payload: "ok"},
		{ID: "m4", Payload: "app"},
		{ID: "m5", Payload: "string"},
	}

	handle := func(m Message) error {
		processed = append(processed, m.ID)
		switch m.Payload {
		case "index":
			bad := []int{1, 2}
			_ = bad[5] // index out of range: a genuine runtime.Error
		case "app":
			panic(sentinel)
		case "string":
			panic("weird failure")
		}
		return nil
	}

	outcomes, deadLetter := ConsumeBatch(msgs, handle)

	if len(outcomes) != len(msgs) {
		t.Fatalf("len(outcomes) = %d, want %d", len(outcomes), len(msgs))
	}
	if len(processed) != len(msgs) {
		t.Fatalf("processed %d/%d messages; a panic must not stall the batch", len(processed), len(msgs))
	}

	byID := make(map[string]Outcome, len(outcomes))
	for _, o := range outcomes {
		byID[o.ID] = o
	}

	if byID["m1"].Kind != KindNone || !byID["m1"].OK {
		t.Fatalf("m1 = %+v, want a clean success", byID["m1"])
	}
	if byID["m2"].Kind != KindRuntimeBug {
		t.Fatalf("m2 kind = %v, want KindRuntimeBug", byID["m2"].Kind)
	}
	if byID["m4"].Kind != KindAppError {
		t.Fatalf("m4 kind = %v, want KindAppError", byID["m4"].Kind)
	}
	if !errors.Is(byID["m4"].Err, sentinel) {
		t.Fatalf("m4 error %v does not wrap the sentinel", byID["m4"].Err)
	}
	if byID["m5"].Kind != KindUnknown {
		t.Fatalf("m5 kind = %v, want KindUnknown", byID["m5"].Kind)
	}

	wantDeadLetter := []string{"m2", "m4", "m5"}
	if len(deadLetter) != len(wantDeadLetter) {
		t.Fatalf("deadLetter = %v, want %v", deadLetter, wantDeadLetter)
	}
	for i, id := range wantDeadLetter {
		if deadLetter[i] != id {
			t.Fatalf("deadLetter[%d] = %q, want %q", i, deadLetter[i], id)
		}
	}
}

func TestConsumeBatchAllClean(t *testing.T) {
	msgs := []Message{{ID: "a"}, {ID: "b"}}
	outcomes, deadLetter := ConsumeBatch(msgs, func(Message) error { return nil })
	if len(outcomes) != 2 || len(deadLetter) != 0 {
		t.Fatalf("outcomes=%v deadLetter=%v, want 2 clean outcomes and no dead letters", outcomes, deadLetter)
	}
}

func TestConsumeBatchEmpty(t *testing.T) {
	outcomes, deadLetter := ConsumeBatch(nil, func(Message) error { return nil })
	if len(outcomes) != 0 || len(deadLetter) != 0 {
		t.Fatalf("outcomes=%v deadLetter=%v, want both empty", outcomes, deadLetter)
	}
}
```

## Review

`ConsumeBatch` is correct when every message in the batch is attempted
regardless of how many earlier ones panicked, and when the returned
classification is accurate enough that a caller could wire it directly to
"retry `KindAppError`, dead-letter everything else" without further
inspection. The recover lives in `consumeOne`, one message wide, re-armed by
the loop each iteration — the same discipline this chapter uses for one job
or one plugin call. `classify` is the piece unique to a queue consumer:
detecting `runtime.Error` via `errors.As` rather than a type switch on
concrete panic types is what makes the classification correct even when the
underlying runtime panic type changes across Go versions, since
`runtime.Error` is a stable interface, not a specific struct.

## Resources

- [runtime.Error](https://pkg.go.dev/runtime#Error) — the interface used to detect a genuine runtime bug versus a modeled application panic.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the per-message recover pattern this batch consumer reuses.
- [fmt.Errorf and %w](https://pkg.go.dev/fmt#Errorf) — preserving a classified panic's original error so errors.Is still reaches it.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-config-loader-fallback-chain.md](17-config-loader-fallback-chain.md) | Next: [19-request-trace-boundary.md](19-request-trace-boundary.md)
