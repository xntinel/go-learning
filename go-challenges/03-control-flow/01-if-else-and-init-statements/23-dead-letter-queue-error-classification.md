# Exercise 23: Dead Letter Queue Router: Permanent vs Transient Error Triage

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A message consumer that retries every failure identically eventually
retries a permanently malformed message forever, wasting consumer
throughput on work that will never succeed; one that never retries turns
every transient blip into a lost message. The fix is a router that
classifies the error first — permanent failures go straight to a
dead-letter queue, transient ones get a bounded number of retries — and the
retry budget itself has to be tracked safely, because the same message ID
can arrive again before the first delivery's outcome is recorded. This
module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
dlqrouter/                  independent module: example.com/dead-letter-queue-error-classification
  go.mod                    go 1.24
  router.go                 Classify(err), Router (mutex-protected), Route(msgID, err)
  cmd/
    demo/
      main.go               a permanent error routes immediately; a transient one exhausts its budget
  router_test.go            table: permanent, under budget, exhausted; concurrent same-ID -race
```

- Files: `router.go`, `cmd/demo/main.go`, `router_test.go`.
- Implement: `Classify(err error) string` using `errors.Is` against `ErrPermanent`, and a `Router` guarded by a `sync.Mutex` with `Route(msgID string, err error) string`, where a permanent classification returns `DestinationDeadLetter` immediately and a transient one checks and increments a per-message retry counter inside one critical section.
- Test: a table over a permanent error, a transient error under its retry budget, and one that exhausts it; a concurrency test that fires 100 concurrent `Route` calls for the same message ID and asserts exactly `maxRetries` come back `"retry"` and the rest `"dead-letter"`, under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why classification runs before the lock, and the budget check runs inside it

`Classify` needs no shared state — it is a pure function of the error
value — so `Route` checks it first, before ever touching the mutex: a
permanent error never needs to consult or update the retry counter at all.
The retry-budget guard is different: `r.retries[msgID]` and its increment
must happen inside the same lock acquisition, because two concurrent
deliveries of the same message can both call `Route` at nearly the same
instant. If the read of the current count and the increment were two
separate operations — read outside the lock, increment inside, or even two
separate lock acquisitions — both concurrent callers could read the same
count, both see it under budget, and both increment, letting the message
retry more times than `maxRetries` allows. Keeping "read the count, decide,
increment" as one atomic block is what makes the budget an actual ceiling
rather than a race-prone approximation.

Create `router.go`:

```go
// Package dlqrouter decides, for a failed message delivery, whether to retry
// it or send it to a dead-letter queue, tracking a per-message retry budget
// safely under concurrent deliveries.
package dlqrouter

import (
	"errors"
	"sync"
)

// ErrPermanent marks an error class that must never be retried.
var ErrPermanent = errors.New("permanent failure")

const (
	DestinationRetry      = "retry"
	DestinationDeadLetter = "dead-letter"
)

// Classify reports whether err is permanent (never retry) based on the
// sentinel wrap chain, or transient otherwise.
func Classify(err error) string {
	if errors.Is(err, ErrPermanent) {
		return "permanent"
	}
	return "transient"
}

// Router tracks retry counts per message ID and is safe for concurrent use.
type Router struct {
	mu         sync.Mutex
	retries    map[string]int
	maxRetries int
}

// NewRouter returns a Router that allows up to maxRetries retries per
// message ID before routing to the dead-letter queue.
func NewRouter(maxRetries int) *Router {
	return &Router{retries: make(map[string]int), maxRetries: maxRetries}
}

// Route decides the destination for a failed delivery of msgID. A permanent
// error always goes straight to the dead-letter queue. A transient error is
// allowed to retry until the message's retry count reaches maxRetries; the
// read of the current count and its increment happen inside one critical
// section, so concurrent deliveries for the same msgID can never together
// authorize more than maxRetries retries.
func (r *Router) Route(msgID string, err error) string {
	if Classify(err) == "permanent" {
		return DestinationDeadLetter
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if n := r.retries[msgID]; n >= r.maxRetries {
		return DestinationDeadLetter
	}

	r.retries[msgID]++
	return DestinationRetry
}
```

### The runnable demo

The demo routes a permanent error once, then routes the same transient
error five times against a budget of three, showing the switch from
`retry` to `dead-letter` exactly at the budget boundary.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	dlqrouter "example.com/dead-letter-queue-error-classification"
)

func main() {
	router := dlqrouter.NewRouter(3)
	transient := errors.New("connection reset")
	permanent := fmt.Errorf("malformed payload: %w", dlqrouter.ErrPermanent)

	fmt.Println("permanent error:", router.Route("msg-perm", permanent))

	for i := 1; i <= 5; i++ {
		dest := router.Route("msg-transient", transient)
		fmt.Printf("transient attempt %d: %s\n", i, dest)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
permanent error: dead-letter
transient attempt 1: retry
transient attempt 2: retry
transient attempt 3: retry
transient attempt 4: dead-letter
transient attempt 5: dead-letter
```

### Tests

The table checks classification-driven routing across three shapes:
permanent, transient-under-budget, and transient-exhausted. The
concurrency test fires 100 goroutines at one message ID with a budget of
10 and asserts the count of `"retry"` outcomes is exactly 10 — not
approximately 10 — proving the budget holds under `-race`.

Create `router_test.go`:

```go
package dlqrouter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestRoute(t *testing.T) {
	t.Parallel()

	transient := errors.New("connection reset")
	permanent := fmt.Errorf("malformed payload: %w", ErrPermanent)

	tests := []struct {
		name       string
		msgID      string
		err        error
		maxRetries int
		attempts   int
		wantLast   string
	}{
		{
			name:       "permanent error goes straight to dead-letter",
			msgID:      "m1",
			err:        permanent,
			maxRetries: 5,
			attempts:   1,
			wantLast:   DestinationDeadLetter,
		},
		{
			name:       "transient error retries under the budget",
			msgID:      "m2",
			err:        transient,
			maxRetries: 3,
			attempts:   2,
			wantLast:   DestinationRetry,
		},
		{
			name:       "transient error exhausts the budget and dead-letters",
			msgID:      "m3",
			err:        transient,
			maxRetries: 3,
			attempts:   4,
			wantLast:   DestinationDeadLetter,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			router := NewRouter(tc.maxRetries)
			var got string
			for i := 0; i < tc.attempts; i++ {
				got = router.Route(tc.msgID, tc.err)
			}
			if got != tc.wantLast {
				t.Errorf("last destination = %q, want %q", got, tc.wantLast)
			}
		})
	}
}

func TestConcurrentRouteNeverExceedsBudget(t *testing.T) {
	t.Parallel()

	const maxRetries = 10
	router := NewRouter(maxRetries)
	transient := errors.New("connection reset")

	const n = 100
	results := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = router.Route("same-message", transient)
		}(i)
	}
	wg.Wait()

	retries := 0
	deadLettered := 0
	for _, r := range results {
		if r == DestinationRetry {
			retries++
			continue
		}
		if r == DestinationDeadLetter {
			deadLettered++
			continue
		}
		t.Fatalf("unexpected destination %q", r)
	}

	if retries != maxRetries {
		t.Errorf("retries = %d, want exactly %d", retries, maxRetries)
	}
	if deadLettered != n-maxRetries {
		t.Errorf("dead-lettered = %d, want %d", deadLettered, n-maxRetries)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

The concurrency test is the one that actually proves the router is
correct: a single-threaded test could pass even if the read-then-increment
were split across two lock acquisitions, because nothing would ever
interleave. Only firing genuinely concurrent callers at the same message
ID and asserting the exact count exposes that bug. Carry this forward: a
shared budget or counter guarded by a mutex needs a concurrent test with an
exact expected count, not just a sequential one — sequential tests cannot
distinguish "correct" from "correct by accident because nothing raced."

## Resources

- [AWS SQS: Amazon SQS dead-letter queues](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-dead-letter-queues.html) — the production pattern this router models.
- [Google Cloud Pub/Sub: Dead-letter topics](https://cloud.google.com/pubsub/docs/handling-failures) — another implementation of retry-budget-then-dead-letter.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the primitive guarding the shared retry counter.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-outbox-pattern-transactional-publish.md](22-outbox-pattern-transactional-publish.md) | Next: [24-graceful-shutdown-drain-and-timeout.md](24-graceful-shutdown-drain-and-timeout.md)
