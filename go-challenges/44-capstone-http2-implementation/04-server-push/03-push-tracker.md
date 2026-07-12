# Exercise 3: Push Tracker

The tracker is the per-connection gate every push reservation passes through. It allocates even server-initiated stream IDs, deduplicates resources so the same one is never pushed twice on a connection, enforces the client's `SETTINGS_MAX_CONCURRENT_STREAMS` limit, and honors `SETTINGS_ENABLE_PUSH`.

This module is fully self-contained: its own `go mod init`, all code inline, its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
push-tracker/
  go.mod
  tracker.go           Tracker, NewTracker, ReserveStream, CompletePush, SetPushEnabled, ...
  tracker_test.go      even IDs, dedup, push-disabled, concurrent limit, re-enable
  cmd/demo/main.go     reserve five resources against a max-concurrent=3 tracker
```

- Files: `tracker.go`, `tracker_test.go`, `cmd/demo/main.go`.
- Implement: `Tracker` with `NewTracker(maxConcurrent int32)`, `ReserveStream`, `CompletePush`, `SetPushEnabled`, `PushEnabled`, `AlreadyPushed`, plus `ErrPushDisabled` and `ErrConcurrentStreamLimit`.
- Test: IDs are even and increasing; a repeated resource is skipped; a disabled tracker refuses; the concurrency limit trips and clears after `CompletePush`; re-enabling restores pushes.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Three return shapes from one method

`ReserveStream` collapses the entire "may I push this, and on what stream" decision into one call with three distinct outcomes, encoded so the caller can branch cleanly. A non-zero ID with a nil error means the push is reserved and the caller should send a PUSH_PROMISE on that ID. A zero ID with a nil error means the resource was already pushed on this connection and the caller must silently skip it — not an error, just a no-op. A zero ID with an error means the push is forbidden, either because the client disabled push (`ErrPushDisabled`) or because the concurrency limit is reached (`ErrConcurrentStreamLimit`). The demo's switch statement shows the three branches the caller writes against this contract.

The concurrency design is the reason two fields are atomics and two are under a mutex. `pushEnabled` and `activePushes` are read and written from goroutines that have nothing else to coordinate — `SetPushEnabled` is called from the connection's SETTINGS-processing path and `CompletePush` from the stream reader when an RST_STREAM or END_STREAM lands — so making them `atomic.Bool` and `atomic.Int32` lets those callers update state without contending on the mutex. The mutex guards only the two things that must stay consistent with each other: the deduplication map and the `nextID` counter. Allocating an ID and recording the resource have to happen as one atomic step, because two goroutines reserving different resources at the same instant must get different even IDs and must both see the map update; a plain atomic on the counter alone would not keep the map in sync. `nextID` starts at 2 and advances by 2, so every ID it hands out is even, server-initiated, and strictly greater than the last.

Create `tracker.go`:

```go
package push

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ErrPushDisabled is returned when the client has sent SETTINGS_ENABLE_PUSH=0.
var ErrPushDisabled = errors.New("push: server push disabled by client SETTINGS")

// ErrConcurrentStreamLimit is returned when the push would exceed the
// client's SETTINGS_MAX_CONCURRENT_STREAMS limit.
var ErrConcurrentStreamLimit = errors.New("push: max concurrent streams exceeded")

// Tracker maintains per-connection push state.
// The zero value is not valid; use NewTracker.
type Tracker struct {
	pushEnabled   atomic.Bool
	activePushes  atomic.Int32
	maxConcurrent int32

	mu     sync.Mutex
	pushed map[string]bool
	nextID uint32 // next even server-initiated stream ID; starts at 2
}

// NewTracker returns a Tracker with push enabled and the given
// SETTINGS_MAX_CONCURRENT_STREAMS limit. A limit of 0 means unlimited.
func NewTracker(maxConcurrent int32) *Tracker {
	t := &Tracker{
		pushed:        make(map[string]bool),
		nextID:        2,
		maxConcurrent: maxConcurrent,
	}
	t.pushEnabled.Store(true)
	return t
}

// SetPushEnabled records the value from the client's SETTINGS_ENABLE_PUSH
// parameter. Call this whenever a SETTINGS frame changes the value.
func (t *Tracker) SetPushEnabled(enabled bool) {
	t.pushEnabled.Store(enabled)
}

// PushEnabled reports whether the client currently allows server push.
func (t *Tracker) PushEnabled() bool {
	return t.pushEnabled.Load()
}

// ReserveStream allocates the next even stream ID for a push to resourcePath
// and records the resource to prevent duplicate pushes on this connection.
//
// Return values:
//   - (id > 0, nil): push reserved; caller should send PUSH_PROMISE on id.
//   - (0, nil): resourcePath was already pushed; caller must skip it.
//   - (0, err): push not allowed (ErrPushDisabled or ErrConcurrentStreamLimit).
func (t *Tracker) ReserveStream(resourcePath string) (streamID uint32, err error) {
	if !t.pushEnabled.Load() {
		return 0, ErrPushDisabled
	}
	if t.maxConcurrent > 0 && t.activePushes.Load() >= t.maxConcurrent {
		return 0, ErrConcurrentStreamLimit
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.pushed[resourcePath] {
		return 0, nil
	}
	id := t.nextID
	t.nextID += 2
	t.pushed[resourcePath] = true
	t.activePushes.Add(1)
	return id, nil
}

// CompletePush decrements the active-push counter. Call this when a pushed
// stream reaches END_STREAM or is cancelled by RST_STREAM from the client.
func (t *Tracker) CompletePush() {
	t.activePushes.Add(-1)
}

// AlreadyPushed reports whether resourcePath was already pushed on this
// connection.
func (t *Tracker) AlreadyPushed(resourcePath string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pushed[resourcePath]
}
```

### The runnable demo

The demo reserves five resources against a tracker capped at three concurrent pushes, calling `CompletePush` immediately after each successful reservation so the slot frees up — which is why all four distinct resources reserve successfully despite the cap of three. The fourth entry repeats `/style.css` and is skipped by deduplication.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	push "example.com/push-tracker"
)

func main() {
	// Max 3 concurrent pushes.
	tracker := push.NewTracker(3)
	resources := []string{"/style.css", "/app.js", "/font.woff2", "/style.css", "/extra.js"}
	fmt.Println("Tracker reservations (max concurrent=3):")
	for _, res := range resources {
		id, err := tracker.ReserveStream(res)
		switch {
		case err != nil:
			fmt.Printf("  LIMIT %-18s %v\n", res, err)
		case id == 0:
			fmt.Printf("  SKIP  %-18s (already pushed)\n", res)
		default:
			fmt.Printf("  PUSH  %-18s on stream %d\n", res, id)
			// In production, CompletePush is called when the pushed stream
			// reaches END_STREAM or the client sends RST_STREAM CANCEL.
			tracker.CompletePush()
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Tracker reservations (max concurrent=3):
  PUSH  /style.css         on stream 2
  PUSH  /app.js            on stream 4
  PUSH  /font.woff2        on stream 6
  SKIP  /style.css         (already pushed)
  PUSH  /extra.js          on stream 8
```

The repeated `/style.css` returns stream 0 and is reported as a skip; the IDs 2, 4, 6, 8 are all even and strictly increasing, exactly as a server-initiated stream sequence must be.

### Tests

The tests pin even-and-increasing ID allocation, deduplication returning a zero ID, the push-disabled refusal, the concurrency limit tripping and then clearing after one `CompletePush`, and a disable-then-re-enable cycle restoring pushes.

Create `tracker_test.go`:

```go
package push

import (
	"errors"
	"fmt"
	"testing"
)

func TestTrackerAllocatesEvenIDs(t *testing.T) {
	t.Parallel()

	tr := NewTracker(0)
	for i := uint32(1); i <= 5; i++ {
		id, err := tr.ReserveStream(fmt.Sprintf("/res%d.css", i))
		if err != nil {
			t.Fatalf("ReserveStream: %v", err)
		}
		if id == 0 {
			t.Fatalf("expected non-zero ID for first reservation of /res%d.css", i)
		}
		if id%2 != 0 {
			t.Fatalf("stream ID %d is odd, want even", id)
		}
		if id != i*2 {
			t.Fatalf("stream ID = %d, want %d", id, i*2)
		}
	}
}

func TestTrackerDeduplication(t *testing.T) {
	t.Parallel()

	tr := NewTracker(0)
	id1, err := tr.ReserveStream("/style.css")
	if err != nil || id1 == 0 {
		t.Fatalf("first ReserveStream: id=%d err=%v", id1, err)
	}
	id2, err := tr.ReserveStream("/style.css")
	if err != nil {
		t.Fatalf("second ReserveStream: %v", err)
	}
	if id2 != 0 {
		t.Fatalf("duplicate reservation returned non-zero ID %d", id2)
	}
}

func TestTrackerPushDisabled(t *testing.T) {
	t.Parallel()

	tr := NewTracker(0)
	tr.SetPushEnabled(false)

	_, err := tr.ReserveStream("/style.css")
	if !errors.Is(err, ErrPushDisabled) {
		t.Fatalf("err = %v, want ErrPushDisabled", err)
	}
}

func TestTrackerConcurrentLimit(t *testing.T) {
	t.Parallel()

	tr := NewTracker(2)
	if _, err := tr.ReserveStream("/a.css"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := tr.ReserveStream("/b.css"); err != nil {
		t.Fatalf("second: %v", err)
	}
	_, err := tr.ReserveStream("/c.css")
	if !errors.Is(err, ErrConcurrentStreamLimit) {
		t.Fatalf("err = %v, want ErrConcurrentStreamLimit", err)
	}

	// After one CompletePush, the limit is no longer exceeded.
	tr.CompletePush()
	id, err := tr.ReserveStream("/c.css")
	if err != nil || id == 0 {
		t.Fatalf("after CompletePush: id=%d err=%v", id, err)
	}
}

func TestAlreadyPushedAfterReserve(t *testing.T) {
	t.Parallel()

	tr := NewTracker(0)
	if tr.AlreadyPushed("/style.css") {
		t.Fatal("AlreadyPushed should be false before ReserveStream")
	}
	if _, err := tr.ReserveStream("/style.css"); err != nil {
		t.Fatalf("ReserveStream: %v", err)
	}
	if !tr.AlreadyPushed("/style.css") {
		t.Fatal("AlreadyPushed should be true after ReserveStream")
	}
}

func TestTrackerReenablesPush(t *testing.T) {
	t.Parallel()

	tr := NewTracker(0)
	tr.SetPushEnabled(false)
	if _, err := tr.ReserveStream("/style.css"); !errors.Is(err, ErrPushDisabled) {
		t.Fatalf("err = %v, want ErrPushDisabled", err)
	}
	tr.SetPushEnabled(true)
	id, err := tr.ReserveStream("/style.css")
	if err != nil || id == 0 {
		t.Fatalf("after re-enable: id=%d err=%v", id, err)
	}
}
```

## Review

The tracker is correct when ID allocation and the dedup-map write happen under the same lock — run the suite under `-race`, because that is what proves two concurrent `ReserveStream` calls cannot hand out the same ID or race on the map. Confirm the three return shapes are distinct and the caller can tell "skip, already pushed" (zero ID, nil error) from "forbidden" (zero ID, error). The classic bug this module guards against is forgetting to call `CompletePush` when a push ends or is cancelled: `activePushes` then climbs monotonically until it hits `maxConcurrent` and every later reservation fails with `ErrConcurrentStreamLimit` even though nothing is in flight. The next two modules build the pieces that drive `SetPushEnabled` and `CompletePush` from the wire — SETTINGS negotiation and RST_STREAM cancellation.

## Resources

- [RFC 9113 §5.1.2 — Stream Concurrency](https://httpwg.org/specs/rfc9113.html#StreamConcurrency) — how pushed streams count against SETTINGS_MAX_CONCURRENT_STREAMS.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Bool` and `atomic.Int32`, the lock-free fields behind `SetPushEnabled` and `CompletePush`.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock that keeps ID allocation and the dedup map consistent.

---

Back to [02-push-policy.md](02-push-policy.md) | Next: [04-enable-push-negotiation.md](04-enable-push-negotiation.md)
