# Exercise 9: Queue Consumer — Ack/Nack/DLQ Disposition Using Is and As Together

Every message-queue consumer must decide, for each failed message, whether to
acknowledge it (already handled), negatively-acknowledge it for redelivery
(transient failure, try again), or route it to a dead-letter queue (poison message
or retries exhausted). This exercise builds that decision by combining both verbs:
`errors.Is(err, ErrDuplicate)` for the idempotent-ack case, and `errors.As` into a
transient error's `Retryable()` for the nack-vs-DLQ case.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
queuedisp/                      independent module: example.com/queuedisp
  go.mod                        go 1.25
  consumer.go                   Disposition, ErrDuplicate, TransientError, Decide, Consumer
  consumer_test.go              table over (err, attempt) -> Ack/Nack/DLQ; fake broker
  cmd/demo/main.go              runnable demo of each disposition
```

Files: `consumer.go`, `consumer_test.go`, `cmd/demo/main.go`.
Implement: `Decide(err, attempt, maxAttempts)` returning `Ack`/`Nack`/`DLQ` using `errors.Is` for `ErrDuplicate` and `errors.As` for a `Retryable()` transient error; a `Consumer` that calls the matching broker method.
Test: table over (handler error, attempt) to expected disposition; duplicate to Ack; transient with attempts<max to Nack; transient with attempts==max to DLQ; unknown fatal to DLQ immediately; fake broker captures the call.
Verify: `go test -count=1 -race ./... && go vet ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/queuedisp/cmd/demo
cd ~/go-exercises/queuedisp
go mod init example.com/queuedisp
go mod edit -go=1.25
```

### Both verbs, in the right order

A consumer's disposition logic is where `Is` and `As` work together, each doing
the job it is suited for. The first question is one of *identity*: has this message
already been processed? A well-designed handler that detects a duplicate (an
idempotency key it has seen) returns `ErrDuplicate`, and the consumer answers with
`errors.Is(err, ErrDuplicate)` — a sentinel match, because "duplicate" is a kind,
not a payload. A duplicate must be **Ack**'d: the work is already done, so
acknowledge and drop the message; redelivering or dead-lettering it would be wrong.

The second question is one of *payload*: is this failure transient, and if so, have
we retried too many times? A transient failure carries a `Retryable()` verdict, so
the consumer uses `errors.As` into a behavioral interface to recover it. A
transient error whose attempt count is still below `maxAttempts` gets **Nack**'d —
negatively acknowledged so the broker redelivers it later. But a transient error
that has already used all its attempts is a message that keeps failing; it goes to
the **DLQ** so it stops consuming redelivery budget and a human can inspect it.
Anything that is neither a duplicate nor transient — an unknown, non-retryable fault
— is a poison message and goes straight to the **DLQ** on the first attempt; there
is no point redelivering an error that will never succeed.

Order matters: check `ErrDuplicate` first (an idempotent success masquerading as an
error), then the transient/attempts logic, then default to DLQ. The `Consumer`
wraps `Decide` and dispatches to a `Broker` interface — `Ack`, `Nack`, or `DLQ` —
so the effect is observable and the broker is fakeable in tests. When routing to
the DLQ after exhausting retries, the consumer attaches attempt context with
`errors.Join`, so the dead-lettered error records both the original cause and how
many attempts were spent.

Create `consumer.go`:

```go
package queuedisp

import (
	"errors"
	"fmt"
)

type Disposition int

const (
	Ack Disposition = iota
	Nack
	DLQ
)

func (d Disposition) String() string {
	switch d {
	case Ack:
		return "ack"
	case Nack:
		return "nack"
	default:
		return "dlq"
	}
}

// ErrDuplicate marks a message the handler has already processed (idempotency).
var ErrDuplicate = errors.New("duplicate message")

// TransientError marks a redeliverable failure.
type TransientError struct {
	Cause error
}

func (e *TransientError) Error() string   { return "transient: " + e.Cause.Error() }
func (e *TransientError) Unwrap() error   { return e.Cause }
func (e *TransientError) Retryable() bool { return true }

// Decide chooses a disposition from the handler error and attempt count. It uses
// errors.Is for the duplicate identity and errors.As for the transient payload.
func Decide(err error, attempt, maxAttempts int) Disposition {
	if err == nil || errors.Is(err, ErrDuplicate) {
		return Ack // already processed: acknowledge and drop
	}
	var r interface{ Retryable() bool }
	if errors.As(err, &r) && r.Retryable() {
		if attempt < maxAttempts {
			return Nack // redeliver
		}
		return DLQ // retries exhausted
	}
	return DLQ // unknown fatal: poison message
}

// Broker is the seam a Consumer dispatches to. Tests inject a fake.
type Broker interface {
	Ack(id string)
	Nack(id string)
	DLQ(id string, cause error)
}

type Consumer struct {
	Broker      Broker
	MaxAttempts int
}

// Process decides the disposition for a message and dispatches it to the broker,
// returning the chosen disposition.
func (c *Consumer) Process(id string, attempt int, handlerErr error) Disposition {
	d := Decide(handlerErr, attempt, c.MaxAttempts)
	switch d {
	case Ack:
		c.Broker.Ack(id)
	case Nack:
		c.Broker.Nack(id)
	default:
		cause := errors.Join(handlerErr, fmt.Errorf("dead-lettered after attempt %d", attempt))
		c.Broker.DLQ(id, cause)
	}
	return d
}
```

### The runnable demo

The demo drives a fake broker through each disposition: a duplicate, a transient
with attempts remaining, a transient with attempts exhausted, and an unknown fatal.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/queuedisp"
)

type printBroker struct{}

func (printBroker) Ack(id string)              { fmt.Printf("  broker.Ack(%s)\n", id) }
func (printBroker) Nack(id string)             { fmt.Printf("  broker.Nack(%s)\n", id) }
func (printBroker) DLQ(id string, cause error) { fmt.Printf("  broker.DLQ(%s)\n", id) }

func main() {
	c := &queuedisp.Consumer{Broker: printBroker{}, MaxAttempts: 3}

	cases := []struct {
		id      string
		attempt int
		err     error
	}{
		{"m1", 1, fmt.Errorf("handle: %w", queuedisp.ErrDuplicate)},
		{"m2", 1, &queuedisp.TransientError{Cause: errors.New("timeout")}},
		{"m3", 3, &queuedisp.TransientError{Cause: errors.New("timeout")}},
		{"m4", 1, errors.New("malformed payload")},
	}
	for _, tc := range cases {
		d := c.Process(tc.id, tc.attempt, tc.err)
		fmt.Printf("%s attempt=%d -> %s\n", tc.id, tc.attempt, d)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
  broker.Ack(m1)
m1 attempt=1 -> ack
  broker.Nack(m2)
m2 attempt=1 -> nack
  broker.DLQ(m3)
m3 attempt=3 -> dlq
  broker.DLQ(m4)
m4 attempt=1 -> dlq
```

### Tests

The table drives `Decide` over `(error, attempt)` and asserts the disposition, and
a separate test drives the `Consumer` against a fake broker to assert the right
method was called. The DLQ-after-exhaustion case also asserts the dead-lettered
cause still carries the original error via `errors.Is`.

Create `consumer_test.go`:

```go
package queuedisp

import (
	"errors"
	"fmt"
	"testing"
)

func TestDecide(t *testing.T) {
	t.Parallel()
	const maxAttempts = 3
	transient := &TransientError{Cause: errors.New("timeout")}
	tests := []struct {
		name    string
		err     error
		attempt int
		want    Disposition
	}{
		{"duplicate acked", fmt.Errorf("h: %w", ErrDuplicate), 1, Ack},
		{"nil acked", nil, 1, Ack},
		{"transient redelivered", transient, 1, Nack},
		{"transient at max dlq", transient, maxAttempts, DLQ},
		{"unknown fatal dlq", errors.New("malformed"), 1, DLQ},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Decide(tc.err, tc.attempt, maxAttempts); got != tc.want {
				t.Fatalf("Decide(%v, %d) = %s, want %s", tc.err, tc.attempt, got, tc.want)
			}
		})
	}
}

type fakeBroker struct {
	acked  []string
	nacked []string
	dlqd   []string
	causes []error
}

func (b *fakeBroker) Ack(id string)  { b.acked = append(b.acked, id) }
func (b *fakeBroker) Nack(id string) { b.nacked = append(b.nacked, id) }
func (b *fakeBroker) DLQ(id string, cause error) {
	b.dlqd = append(b.dlqd, id)
	b.causes = append(b.causes, cause)
}

func TestConsumerDispatches(t *testing.T) {
	t.Parallel()
	b := &fakeBroker{}
	c := &Consumer{Broker: b, MaxAttempts: 3}

	c.Process("dup", 1, fmt.Errorf("h: %w", ErrDuplicate))
	c.Process("try", 1, &TransientError{Cause: errors.New("timeout")})
	c.Process("dead", 3, &TransientError{Cause: errors.New("timeout")})

	if len(b.acked) != 1 || b.acked[0] != "dup" {
		t.Fatalf("acked = %v, want [dup]", b.acked)
	}
	if len(b.nacked) != 1 || b.nacked[0] != "try" {
		t.Fatalf("nacked = %v, want [try]", b.nacked)
	}
	if len(b.dlqd) != 1 || b.dlqd[0] != "dead" {
		t.Fatalf("dlqd = %v, want [dead]", b.dlqd)
	}
	// The dead-lettered cause still carries the transient failure.
	var tr *TransientError
	if !errors.As(b.causes[0], &tr) {
		t.Fatalf("DLQ cause lost the original transient error: %v", b.causes[0])
	}
}

func ExampleDecide() {
	fmt.Println(Decide(fmt.Errorf("h: %w", ErrDuplicate), 1, 3))
	// Output: ack
}
```

## Review

The consumer is correct when each message reaches the right terminal state: a
duplicate is Ack'd (the work is done), a transient with budget left is Nack'd for
redelivery, a transient out of budget is DLQ'd, and an unknown fatal is DLQ'd on
the first attempt. The two verbs divide the work cleanly: `errors.Is` for the
duplicate *identity*, `errors.As` for the transient *payload* and its `Retryable()`
verdict. Order the checks duplicate-first, because a duplicate is really a success
wearing an error's clothes and must not be redelivered or dead-lettered. The DLQ
path attaches attempt context with `errors.Join` while keeping the original cause
inspectable — the test proves `errors.As` still finds the transient error inside the
joined DLQ cause. Run `go test -race`.

## Resources

- [errors.Is](https://pkg.go.dev/errors#Is) — the duplicate identity check.
- [errors.As](https://pkg.go.dev/errors#As) — the transient `Retryable()` extraction.
- [errors.Join](https://pkg.go.dev/errors#Join) — attaching attempt context to the DLQ cause.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-observability-extract-fields.md](08-observability-extract-fields.md) | Next: [10-astype-generic-matching.md](10-astype-generic-matching.md)
