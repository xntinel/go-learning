# Exercise 5: Draining In-Flight Streams on Graceful Shutdown

Sending GOAWAY announces a last-stream-id; honoring it means actually letting the streams at or below that boundary run to completion while refusing every new one — and not waiting forever for a stuck stream to finish. This module builds `Drainer`, the coordinator that tracks active streams, refuses new ones above the drain boundary with `REFUSED_STREAM`, and blocks the shutdown path until the in-flight set empties or a grace deadline forces the close.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
drain.go               Drainer, BeginStream, EndStream, Drain, Wait, counts; ErrStreamRefused
cmd/
  demo/
    main.go            open streams, drain, refuse a new one, wait for a clean drain
drain_test.go          refusal boundary, clean drain, drains within grace, forced after timeout
```

- Files: `drain.go`, `cmd/demo/main.go`, `drain_test.go`.
- Implement: `Drainer` with `BeginStream`, `EndStream`, `Drain`, `Wait`, `ActiveCount`, and `RefusedCount`.
- Test: `drain_test.go` checks new streams above the boundary are refused, a finishing set drains cleanly, a stream that finishes within the grace period returns nil at the right virtual time, and a stuck stream forces a deadline-exceeded close.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The drain boundary and the two outcomes

Graceful shutdown has a precise contract: once `Drain(lastStreamID)` is called, every stream already active with an ID at or below `lastStreamID` is allowed to finish, and every new stream above it is refused immediately with `ErrStreamRefused` — the in-package stand-in for the `REFUSED_STREAM` code, which guarantees the stream was never processed so the client can safely retry it on a fresh connection. The `Drainer` keeps the set of active stream IDs in a map; `BeginStream` adds to it (or refuses), `EndStream` removes from it, and the moment that set becomes empty during a drain the coordinator signals completion by closing an `idle` channel exactly once.

`Wait` is where the two outcomes diverge, and the distinction is the whole point. It selects on the `idle` channel and on the caller's context. If `idle` closes first, the connection drained cleanly and `Wait` returns nil. If the context's deadline fires first, one or more streams never finished, and `Wait` returns the context error — a *forced* close that tells the shutdown path it abandoned in-flight work. Without that grace deadline a single wedged stream would hang the shutdown forever; without distinguishing the two outcomes the caller could not tell a clean drain from an abandonment. Because the grace period is real elapsed time, the tests drive it inside a `testing/synctest` bubble: the fake clock lets "a stream finishes after one virtual second" and "the two-second deadline expires" be exact, deterministic assertions rather than sleep-based flakes.

Create `drain.go`:

```go
// Package drain coordinates graceful HTTP/2 shutdown: it tracks in-flight
// streams, refuses new streams above the GOAWAY last-stream-id boundary, and
// blocks until the in-flight set drains or a grace deadline forces the close.
package drain

import (
	"context"
	"errors"
	"sync"
)

// ErrStreamRefused is returned by BeginStream for a stream above the drain
// boundary. It stands for the HTTP/2 REFUSED_STREAM code: the stream was never
// processed, so the client may safely retry it on a new connection.
var ErrStreamRefused = errors.New("drain: stream refused, retry on a new connection")

// ErrNotDraining is returned by Wait when Drain has not been called.
var ErrNotDraining = errors.New("drain: Wait called before Drain")

// Drainer tracks active streams and the graceful-shutdown lifecycle.
// All methods are safe for concurrent use. The zero value is not usable;
// construct with NewDrainer.
type Drainer struct {
	mu           sync.Mutex
	active       map[uint32]struct{}
	draining     bool
	drained      bool
	lastStreamID uint32
	idle         chan struct{}
	refused      int
}

// NewDrainer returns a Drainer with no active streams and no drain in progress.
func NewDrainer() *Drainer {
	return &Drainer{active: make(map[uint32]struct{})}
}

// BeginStream registers a new stream. While draining, a stream with an ID above
// the drain boundary is refused with ErrStreamRefused; one at or below it is
// admitted, since it was already in flight when the drain began.
func (d *Drainer) BeginStream(id uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.draining && id > d.lastStreamID {
		d.refused++
		return ErrStreamRefused
	}
	d.active[id] = struct{}{}
	return nil
}

// EndStream marks a stream finished. If a drain is in progress and the active
// set has just emptied, it signals waiters that the drain is complete.
func (d *Drainer) EndStream(id uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.active, id)
	if d.draining && !d.drained && len(d.active) == 0 {
		d.drained = true
		close(d.idle)
	}
}

// Drain enters draining mode with the given last-stream-id boundary. Streams
// already active at or below it run to completion; new streams above it are
// refused. Drain is idempotent: a second call is a no-op.
func (d *Drainer) Drain(lastStreamID uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.draining {
		return
	}
	d.draining = true
	d.lastStreamID = lastStreamID
	d.idle = make(chan struct{})
	if len(d.active) == 0 {
		d.drained = true
		close(d.idle)
	}
}

// Wait blocks until every in-flight stream finishes or ctx is done. It returns
// nil if the connection drained cleanly, or ctx.Err() if the grace deadline
// expired first — a forced close that abandoned in-flight work.
func (d *Drainer) Wait(ctx context.Context) error {
	d.mu.Lock()
	if !d.draining {
		d.mu.Unlock()
		return ErrNotDraining
	}
	idle := d.idle
	d.mu.Unlock()

	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ActiveCount returns the number of streams currently in flight.
func (d *Drainer) ActiveCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.active)
}

// RefusedCount returns how many new streams have been refused since draining began.
func (d *Drainer) RefusedCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.refused
}
```

### The runnable demo

The demo opens three streams, drains at last-stream-id 5, refuses a new stream 7, then finishes the in-flight streams and waits for a clean drain.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/graceful-drain"
)

func main() {
	d := drain.NewDrainer()
	for _, id := range []uint32{1, 3, 5} {
		_ = d.BeginStream(id)
	}
	fmt.Println("active before drain:", d.ActiveCount())

	d.Drain(5) // last processed stream is 5
	if err := d.BeginStream(7); err != nil {
		fmt.Println("stream 7:", err)
	}

	// Finish the in-flight streams, then wait for a clean drain.
	go func() {
		d.EndStream(1)
		d.EndStream(3)
		d.EndStream(5)
	}()

	if err := d.Wait(context.Background()); err != nil {
		fmt.Println("forced close:", err)
	} else {
		fmt.Println("drained cleanly, refused:", d.RefusedCount())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
active before drain: 3
stream 7: drain: stream refused, retry on a new connection
drained cleanly, refused: 1
```

### Tests

`TestRefusesStreamsAboveBoundary` pins the boundary: with a drain at 3, a new stream 5 is refused and a new stream 2 is admitted. `TestDrainsCleanlyWhenStreamsFinish` finishes the in-flight set from goroutines and asserts `Wait` returns nil. The two `synctest` tests drive the grace clock: `TestDrainsWithinGrace` finishes a stream after one virtual second and asserts `Wait` returns nil at exactly that virtual time, and `TestForcedAfterGraceDeadline` leaves a stream stuck and asserts `Wait` returns `context.DeadlineExceeded` at exactly the two-second deadline.

Create `drain_test.go`:

```go
package drain

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestRefusesStreamsAboveBoundary(t *testing.T) {
	t.Parallel()
	d := NewDrainer()
	if err := d.BeginStream(1); err != nil {
		t.Fatalf("BeginStream(1) before drain: %v", err)
	}
	d.Drain(3)
	if err := d.BeginStream(5); !errors.Is(err, ErrStreamRefused) {
		t.Errorf("BeginStream(5) above boundary = %v, want ErrStreamRefused", err)
	}
	if err := d.BeginStream(2); err != nil {
		t.Errorf("BeginStream(2) at or below boundary = %v, want nil", err)
	}
	if got := d.RefusedCount(); got != 1 {
		t.Errorf("RefusedCount = %d, want 1", got)
	}
}

func TestDrainsCleanlyWhenStreamsFinish(t *testing.T) {
	t.Parallel()
	d := NewDrainer()
	ids := []uint32{1, 3, 5}
	for _, id := range ids {
		if err := d.BeginStream(id); err != nil {
			t.Fatalf("BeginStream(%d): %v", id, err)
		}
	}
	d.Drain(5)

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.EndStream(id)
		}()
	}
	wg.Wait()

	if err := d.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after all streams finished = %v, want nil", err)
	}
	if got := d.ActiveCount(); got != 0 {
		t.Errorf("ActiveCount = %d, want 0", got)
	}
}

func TestDrainsWithinGrace(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := NewDrainer()
		if err := d.BeginStream(1); err != nil {
			t.Fatalf("BeginStream(1): %v", err)
		}
		d.Drain(1)

		go func() {
			time.Sleep(time.Second) // in-flight stream finishes after 1s
			d.EndStream(1)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		if err := d.Wait(ctx); err != nil {
			t.Fatalf("Wait = %v, want nil (drained within grace)", err)
		}
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("drained at %v, want exactly 1s", elapsed)
		}
	})
}

func TestForcedAfterGraceDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := NewDrainer()
		if err := d.BeginStream(1); err != nil {
			t.Fatalf("BeginStream(1): %v", err)
		}
		d.Drain(1) // stream 1 never finishes

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		start := time.Now()
		err := d.Wait(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait = %v, want context.DeadlineExceeded (forced close)", err)
		}
		if elapsed := time.Since(start); elapsed != 2*time.Second {
			t.Fatalf("forced at %v, want exactly 2s", elapsed)
		}
	})
}
```

## Review

The drainer is correct when the boundary is inclusive and the grace deadline is honored in both directions. The first mistake is refusing streams at or below the last-stream-id: those were already in flight when the drain began and must be allowed to finish, so only IDs strictly above the boundary are refused. The second is closing the `idle` channel more than once — if both `EndStream` and an empty-at-drain-time `Drain` close it, the second close panics; the `drained` guard ensures exactly one close. The third is omitting the grace deadline so `Wait` blocks forever on a stuck stream, or, equally wrong, forcing the close the instant draining begins and abandoning streams that would have finished in milliseconds. `Wait` returning nil versus `ctx.Err()` is the signal the shutdown path needs to know whether work was abandoned. The `synctest` tests prove the timing exactly, and `-race` confirms the active-set map is safe under the concurrent `BeginStream`/`EndStream`/`Wait` access.

## Resources

- [RFC 9113 §6.8 — GOAWAY](https://httpwg.org/specs/rfc9113.html#GOAWAY) — the last-stream-id contract that the drain boundary enforces.
- [RFC 9113 §8.1 — HTTP Message Framing](https://httpwg.org/specs/rfc9113.html#HttpSequence) — why a refused stream (REFUSED_STREAM) is safe to retry.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — the fake-clock bubble that makes the grace-period tests deterministic.
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — the grace deadline whose expiry turns a clean drain into a forced close.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-ping-keepalive.md](04-ping-keepalive.md) | Next: [06-rapid-reset-defense.md](06-rapid-reset-defense.md)
