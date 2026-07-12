# Exercise 34: Multi-Destination Event Publisher with Dead-Letter Queue Fallback

**Nivel: Intermedio** — validacion rapida (un test corto).

An event needs to reach several independent destinations — a webhook, a
message queue, an audit log — and one of them being temporarily unreachable
should never stop the others from getting the event, nor should it silently
drop the failed delivery. `NewPublisher` closes over an ordered list of
destinations and a dead-letter callback; `Publish` sends the event to every
destination in order, routing any failing destination's delivery to the DLQ
while the rest continue unaffected.

## What you'll build

```text
multi-destination-publisher/ independent module: example.com/multi-destination-publisher
  go.mod                       go 1.24
  multipub.go                  Destination, Report, NewPublisher
  cmd/
    demo/
      main.go                   webhook fails, queue and log succeed, DLQ logged
  multipub_test.go              table test: partial failure, all succeed, all fail
```

- Files: `multipub.go`, `cmd/demo/main.go`, `multipub_test.go`.
- Implement: `NewPublisher(destinations []Destination, dlq func(destination, event string, err error)) func(event string) Report`, iterating destinations in order and routing failures to `dlq` without stopping.
- Test: one failing destination is routed to the DLQ while the remaining destinations still receive the event; all destinations succeeding never touches the DLQ; all destinations failing routes every one to the DLQ and reports nothing delivered.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/04-first-class-functions-and-closures/34-multi-destination-event-publisher-dlq/cmd/demo
cd go-solutions/04-functions/04-first-class-functions-and-closures/34-multi-destination-event-publisher-dlq
go mod edit -go=1.24
```

### One bad destination never blocks the rest

`NewPublisher` closes over `destinations`, a slice of `{Name, Send}` pairs,
and `dlq`, a callback invoked with the destination name, the event, and the
error whenever a `Send` fails. The returned closure loops over
`destinations` in order — order matters here, unlike the fan-out
broadcaster elsewhere in this lesson, since a webhook, a queue, and a log
are different kinds of destinations with a natural priority, not
interchangeable subscribers — and for each one either appends its name to
`report.Delivered` on success or calls `dlq` and appends to
`report.DeadLettered` on failure, then `continue`s to the next destination
regardless of what just happened. Nothing about one destination's outcome
ever short-circuits the loop; the `Report` returned at the end is a
complete account of what happened to every destination, not just the first
failure.

This is deliberately synchronous and ordered, not fanned out into
goroutines: a caller that wants webhook delivery to happen before the audit
log write, say, depends on that ordering, and a slow destination should
naturally push later destinations later rather than racing them.

Create `multipub.go`:

```go
// Package multipub publishes one event to an ordered list of destinations,
// routing any destination that fails to a dead-letter queue instead of
// aborting the rest of the publish.
package multipub

// Destination is one delivery target (a webhook, a queue, a log sink...).
type Destination struct {
	Name string
	Send func(event string) error
}

// Report records what happened to one Publish call: which destinations
// accepted the event, and which were routed to the dead-letter queue.
type Report struct {
	Delivered    []string
	DeadLettered []string
}

// NewPublisher returns a closure over the ordered destinations and a dlq
// callback. Publish sends event to every destination in order; a
// destination whose Send returns an error is routed to dlq (with its name
// and the error) and recorded as dead-lettered, but delivery continues to
// every remaining destination -- one bad destination never blocks the
// others from receiving the event.
func NewPublisher(destinations []Destination, dlq func(destination, event string, err error)) func(event string) Report {
	return func(event string) Report {
		var report Report
		for _, d := range destinations {
			if err := d.Send(event); err != nil {
				dlq(d.Name, event, err)
				report.DeadLettered = append(report.DeadLettered, d.Name)
				continue
			}
			report.Delivered = append(report.Delivered, d.Name)
		}
		return report
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/multi-destination-publisher"
)

func main() {
	destinations := []multipub.Destination{
		{Name: "webhook", Send: func(event string) error {
			return errors.New("webhook: connection refused")
		}},
		{Name: "queue", Send: func(event string) error {
			fmt.Printf("queue: enqueued %q\n", event)
			return nil
		}},
		{Name: "log", Send: func(event string) error {
			fmt.Printf("log: recorded %q\n", event)
			return nil
		}},
	}

	dlq := func(destination, event string, err error) {
		fmt.Printf("dlq: %s failed for %q: %v\n", destination, event, err)
	}

	publish := multipub.NewPublisher(destinations, dlq)
	report := publish("order.created:42")

	fmt.Println("delivered:", report.Delivered)
	fmt.Println("dead-lettered:", report.DeadLettered)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
dlq: webhook failed for "order.created:42": webhook: connection refused
queue: enqueued "order.created:42"
log: recorded "order.created:42"
delivered: [queue log]
dead-lettered: [webhook]
```

### Tests

Create `multipub_test.go`:

```go
package multipub

import (
	"errors"
	"fmt"
	"testing"
)

func TestPublishRoutesFailuresToDLQButKeepsDelivering(t *testing.T) {
	var dlqCalls []string
	dlq := func(destination, event string, err error) {
		dlqCalls = append(dlqCalls, fmt.Sprintf("%s:%v", destination, err))
	}

	failWebhook := errors.New("webhook down")
	destinations := []Destination{
		{Name: "webhook", Send: func(string) error { return failWebhook }},
		{Name: "queue", Send: func(string) error { return nil }},
		{Name: "log", Send: func(string) error { return nil }},
	}

	publish := NewPublisher(destinations, dlq)
	report := publish("event-1")

	wantDelivered := "[queue log]"
	if got := fmt.Sprint(report.Delivered); got != wantDelivered {
		t.Fatalf("Delivered = %s, want %s", got, wantDelivered)
	}
	wantDeadLettered := "[webhook]"
	if got := fmt.Sprint(report.DeadLettered); got != wantDeadLettered {
		t.Fatalf("DeadLettered = %s, want %s", got, wantDeadLettered)
	}
	if len(dlqCalls) != 1 || dlqCalls[0] != "webhook:webhook down" {
		t.Fatalf("dlqCalls = %v, want exactly one call for webhook", dlqCalls)
	}
}

func TestPublishAllDestinationsSucceed(t *testing.T) {
	dlqCalled := false
	dlq := func(string, string, error) { dlqCalled = true }

	destinations := []Destination{
		{Name: "a", Send: func(string) error { return nil }},
		{Name: "b", Send: func(string) error { return nil }},
	}

	publish := NewPublisher(destinations, dlq)
	report := publish("event-2")

	if len(report.DeadLettered) != 0 {
		t.Fatalf("DeadLettered = %v, want empty", report.DeadLettered)
	}
	if dlqCalled {
		t.Fatal("dlq was called, want no destination to fail")
	}
	wantDelivered := "[a b]"
	if got := fmt.Sprint(report.Delivered); got != wantDelivered {
		t.Fatalf("Delivered = %s, want %s", got, wantDelivered)
	}
}

func TestPublishAllDestinationsFail(t *testing.T) {
	var dlqCalls []string
	dlq := func(destination, event string, err error) {
		dlqCalls = append(dlqCalls, destination)
	}

	destinations := []Destination{
		{Name: "a", Send: func(string) error { return errors.New("a down") }},
		{Name: "b", Send: func(string) error { return errors.New("b down") }},
	}

	publish := NewPublisher(destinations, dlq)
	report := publish("event-3")

	if len(report.Delivered) != 0 {
		t.Fatalf("Delivered = %v, want empty", report.Delivered)
	}
	wantDeadLettered := "[a b]"
	if got := fmt.Sprint(report.DeadLettered); got != wantDeadLettered {
		t.Fatalf("DeadLettered = %s, want %s", got, wantDeadLettered)
	}
	if len(dlqCalls) != 2 {
		t.Fatalf("dlqCalls = %v, want both destinations routed", dlqCalls)
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The partial-failure test is the exercise's core contract: a failing webhook
is routed to the DLQ exactly once, and the queue and log destinations still
receive the event afterward, in order. The all-succeed test guards against
ever calling the DLQ callback when nothing failed. The all-fail test proves
the loop never stops early — every destination is attempted and every
failure is routed, even when none of them succeed.

## Resources

- [AWS docs: Dead-letter queues](https://docs.aws.amazon.com/sqs/latest/dg/sqs-dead-letter-queues.html) — the dead-letter pattern this publisher routes failed destinations to.
- [pkg.go.dev: errors.New](https://pkg.go.dev/errors#New) — the error values destinations return that drive routing to the DLQ.
- [Go spec: Function literals](https://go.dev/ref/spec#Function_literals) — how each `Destination.Send` closes over whatever state a real webhook or queue client would need.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-schema-migration-barrier-reader-blocker.md](33-schema-migration-barrier-reader-blocker.md) | Next: [35-workload-classification-priority-router.md](35-workload-classification-priority-router.md)
