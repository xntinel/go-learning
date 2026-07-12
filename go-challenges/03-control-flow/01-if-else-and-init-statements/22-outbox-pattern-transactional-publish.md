# Exercise 22: Outbox Pattern: Transactional Event Publishing with Durability

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Publishing a domain event and writing the row that triggered it are two
separate operations against two separate systems — a database and a
message broker — and if you do them as two independent steps, a crash
between them either loses the event or publishes one for a transaction
that never actually committed. The transactional outbox pattern fixes this
by writing the event durably in the same transaction as the business
change, then publishing from that durable record; a guard clause keeps a
publish from ever running for an event that was never committed, and a
second guard keeps a publish from ever running twice for one that was.
This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
outbox/                     independent module: example.com/outbox-pattern-transactional-publish
  go.mod                    go 1.24
  outbox.go                 Outbox (mutex-protected), SaveWithTx, Publish
  cmd/
    demo/
      main.go               a failed commit blocks publish; a committed event publishes once
  outbox_test.go            commit-fails guard; publish-once guard; concurrent publish -race
```

- Files: `outbox.go`, `cmd/demo/main.go`, `outbox_test.go`.
- Implement: an `Outbox` guarded by a `sync.Mutex` with `SaveWithTx(id, payload string, commit func() error) error`, which only records the event if `commit` succeeds, and `Publish(id string, publish func(string) error) (bool, error)`, whose comma-ok lookup plus an `ev.published` guard keep an unknown or already-published event from invoking `publish` again.
- Test: a commit failure leaves the event unknown to `Publish`; a first publish call runs the side effect and marks it published; a second call for the same ID is a no-op; a concurrent-publish test fires many goroutines at once and asserts the side effect ran exactly once, under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/01-if-else-and-init-statements/22-outbox-pattern-transactional-publish/cmd/demo
cd go-solutions/03-control-flow/01-if-else-and-init-statements/22-outbox-pattern-transactional-publish
go mod edit -go=1.24
```

### Why durability is checked before publish is ever attempted

`Publish`'s first guard is a comma-ok lookup — `ev, ok := o.events[id]` —
and it returns `ErrUnknownEvent` on a miss before looking at anything
else. This is the guard that enforces "publish only if the business
transaction commits": `SaveWithTx` never adds an entry to `o.events` unless
`commit` returned nil, so a miss on that lookup can only mean the
transaction never durably happened, and there is nothing correct left to
publish. The second guard, `if ev.published`, is a different concern —
durability already happened, but this specific publish attempt is a
duplicate (a retry, a duplicate delivery, a second concurrent caller) —
and it is checked only after the first guard confirms there is a real
event to consider. Collapsing these two guards into one would make a
genuinely unknown event and an already-published one indistinguishable to
the caller, which matters for alerting: one is an integrity bug, the other
is expected idempotent behavior.

Create `outbox.go`:

```go
// Package outbox implements the transactional outbox pattern in memory: an
// event is only durably recorded if its business transaction commits, and a
// durably recorded event is published at most once even under concurrent
// publish attempts.
package outbox

import (
	"errors"
	"sync"
)

// ErrUnknownEvent means id was never durably saved — either it does not
// exist, or its SaveWithTx call's commit failed.
var ErrUnknownEvent = errors.New("unknown outbox event")

type event struct {
	payload   string
	published bool
}

// Outbox is safe for concurrent use.
type Outbox struct {
	mu     sync.Mutex
	events map[string]*event
}

// New returns an empty Outbox.
func New() *Outbox {
	return &Outbox{events: make(map[string]*event)}
}

// SaveWithTx simulates writing the domain row and its outbox event in one
// database transaction: commit runs first, and the event is only recorded if
// commit succeeds. If commit fails, nothing is enqueued for publishing — a
// failed business transaction must never produce a durable event.
func (o *Outbox) SaveWithTx(id, payload string, commit func() error) error {
	if err := commit(); err != nil {
		return err
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	o.events[id] = &event{payload: payload}
	return nil
}

// Publish looks up id and, if it exists and has not already been published,
// invokes publish and marks it published. The lookup and the
// published-or-not check happen inside the same critical section as the
// mark-published write, so concurrent Publish calls for the same id invoke
// publish at most once.
func (o *Outbox) Publish(id string, publish func(payload string) error) (published bool, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	ev, ok := o.events[id]
	if !ok {
		return false, ErrUnknownEvent
	}
	if ev.published {
		return false, nil
	}

	if err := publish(ev.payload); err != nil {
		return false, err
	}
	ev.published = true
	return true, nil
}
```

### The runnable demo

The demo plays out both branches: a commit that fails, leaving `Publish`
unable to find the event at all, and a commit that succeeds, where a
second `Publish` call for the same event is a silent no-op rather than a
duplicate send.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	outbox "example.com/outbox-pattern-transactional-publish"
)

func main() {
	ob := outbox.New()

	// Scenario 1: the business transaction fails to commit, so the event is
	// never durably saved.
	err := ob.SaveWithTx("order-1", "order-1 placed", func() error {
		return errors.New("db connection lost")
	})
	fmt.Println("save order-1 (commit fails):", err)

	published, err := ob.Publish("order-1", func(payload string) error {
		fmt.Println("publishing:", payload)
		return nil
	})
	fmt.Println("publish order-1:", published, err)

	// Scenario 2: the business transaction commits, so the event is durable
	// and can be published — but only once, even if Publish is called twice.
	err = ob.SaveWithTx("order-2", "order-2 placed", func() error {
		return nil
	})
	fmt.Println("save order-2 (commit succeeds):", err)

	published, err = ob.Publish("order-2", func(payload string) error {
		fmt.Println("publishing:", payload)
		return nil
	})
	fmt.Println("publish order-2 first call:", published, err)

	published, err = ob.Publish("order-2", func(payload string) error {
		fmt.Println("publishing:", payload)
		return nil
	})
	fmt.Println("publish order-2 second call:", published, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
save order-1 (commit fails): db connection lost
publish order-1: false unknown outbox event
save order-2 (commit succeeds): <nil>
publishing: order-2 placed
publish order-2 first call: true <nil>
publish order-2 second call: false <nil>
```

### Tests

The first test proves a failed commit leaves `Publish` unable to find the
event — and fails the test outright if the publish callback is ever
invoked. The second locks in the publish-once behavior for sequential
calls. The third fires 32 goroutines at the same event ID and asserts the
side effect ran exactly once, under `-race`.

Create `outbox_test.go`:

```go
package outbox

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSaveWithTxCommitFails(t *testing.T) {
	t.Parallel()

	ob := New()
	wantErr := errors.New("db down")
	err := ob.SaveWithTx("e1", "payload", func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("SaveWithTx error = %v, want %v", err, wantErr)
	}

	_, err = ob.Publish("e1", func(string) error {
		t.Fatal("publish must not be called for an event that never committed")
		return nil
	})
	if !errors.Is(err, ErrUnknownEvent) {
		t.Fatalf("Publish error = %v, want %v", err, ErrUnknownEvent)
	}
}

func TestPublishRunsOnceThenNoOps(t *testing.T) {
	t.Parallel()

	ob := New()
	if err := ob.SaveWithTx("e2", "payload", func() error { return nil }); err != nil {
		t.Fatalf("SaveWithTx: %v", err)
	}

	var calls atomic.Int64
	publish := func(string) error { calls.Add(1); return nil }

	published, err := ob.Publish("e2", publish)
	if !published || err != nil {
		t.Fatalf("first publish: published=%v err=%v, want true nil", published, err)
	}

	published, err = ob.Publish("e2", publish)
	if published || err != nil {
		t.Fatalf("second publish: published=%v err=%v, want false nil", published, err)
	}

	if calls.Load() != 1 {
		t.Fatalf("publish func invoked %d times, want 1", calls.Load())
	}
}

func TestConcurrentPublishInvokesExactlyOnce(t *testing.T) {
	t.Parallel()

	ob := New()
	if err := ob.SaveWithTx("e3", "payload", func() error { return nil }); err != nil {
		t.Fatalf("SaveWithTx: %v", err)
	}

	var calls atomic.Int64
	publish := func(string) error {
		calls.Add(1)
		return nil
	}

	const n = 32
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = ob.Publish("e3", publish)
		}()
	}
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("publish func invoked %d times under concurrency, want exactly 1", calls.Load())
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

The correctness of this module rests on two guards being in the right
order and the second one sharing a lock with the write that follows it:
unknown-event is checked before already-published, and the
already-published check plus the mark-published write happen without
releasing the mutex in between. A version that checked `ev.published`
outside the lock, or split the check and the write into two lock
acquisitions, would let two concurrent publish attempts both observe
`published == false` and both invoke the side effect — exactly the
double-send the outbox pattern exists to prevent. Carry this forward: a
publish-exactly-once guard is only as strong as the critical section
around its check-then-act sequence.

## Resources

- [microservices.io: Transactional outbox pattern](https://microservices.io/patterns/data/transactional-outbox.html) — the pattern this module implements.
- [Debezium: Outbox Event Router](https://debezium.io/documentation/reference/stable/transformations/outbox-event-router.html) — a production implementation using change-data-capture instead of polling.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the primitive that keeps the lookup, publish, and mark-published atomic.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-leader-election-heartbeat-mutex-protected.md](21-leader-election-heartbeat-mutex-protected.md) | Next: [23-dead-letter-queue-error-classification.md](23-dead-letter-queue-error-classification.md)
