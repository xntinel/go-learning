# Exercise 4: PING Keepalive and Liveness Detection

A connection that has gone silent is not necessarily dead, and a connection that looks alive is not necessarily healthy. HTTP/2 `PING` resolves the ambiguity: send an 8-byte opaque payload, the peer echoes it in a PING ACK, and the round-trip both proves liveness and measures latency. This module builds `PingTracker`, which matches ACKs to outstanding pings by payload, measures RTT, enforces a concurrent-ping limit, and reports pings whose deadline has passed.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
ping.go                PingTracker, Sent, Received, Expired, ErrPingTimeout, ErrTooManyPings
cmd/
  demo/
    main.go            send a ping, receive its ack, report the round-trip
ping_test.go           round-trip, unknown ack, expiry, concurrency cap, late-ack timeout
```

- Files: `ping.go`, `cmd/demo/main.go`, `ping_test.go`.
- Implement: `PingTracker` with `Sent`, `Received`, and `Expired`, keyed by the 8-byte payload.
- Test: `ping_test.go` checks a clean round-trip, rejection of an unmatched ACK, deadline expiry, the concurrent-ping cap, a zero deadline, and a late ACK that times out.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/44-capstone-http2-implementation/05-connection-error-handling/04-ping-keepalive/cmd/demo && cd go-solutions/44-capstone-http2-implementation/05-connection-error-handling/04-ping-keepalive
go mod edit -go=1.26
```

### The opaque payload is the correlation key

The HTTP/2 PING payload is deliberately uninterpreted by the protocol: the sender chooses eight arbitrary bytes, the receiver must echo them exactly, and that echo is what lets the sender pair an ACK with the ping it answers. `PingTracker` makes the payload the map key, storing the send time against each outstanding payload, so `Received` is an O(1) lookup that yields the RTT and removes the entry. An ACK whose payload matches no outstanding ping is a protocol oddity — a duplicate, a stale echo, or a confused peer — and is returned as an error rather than silently accepted, because crediting it would corrupt the RTT measurement.

Two safety properties matter. First, RFC 9113 §10.5 names excessive PINGs as an abuse vector, so the tracker caps how many pings may be outstanding at once and returns `ErrTooManyPings` rather than letting an unbounded keepalive loop hammer the peer. Second, a ping that is never answered must eventually be declared dead: `Expired` sweeps the outstanding set and drains every payload whose deadline has elapsed, each one a presumed-dead health check the connection loop can act on, and `Received` independently returns `ErrPingTimeout` if an ACK arrives but only after its deadline. All times are passed in as arguments rather than read from the wall clock inside the tracker, which is what makes every test deterministic: a test supplies an exact send time and an exact ACK time, so an RTT assertion is exact rather than racing the scheduler.

Create `ping.go`:

```go
// Package ping implements HTTP/2 PING keepalive (RFC 9113 §6.7): tracking
// outstanding pings by their 8-byte opaque payload, measuring round-trip
// time, capping concurrency, and detecting pings whose deadline has elapsed.
package ping

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrPingTimeout is returned when a PING ACK is not received within the deadline.
var ErrPingTimeout = errors.New("PING ACK not received within deadline")

// ErrTooManyPings is returned when too many PINGs are outstanding.
// RFC 9113 §10.5 identifies excessive PINGs as an abuse vector.
var ErrTooManyPings = errors.New("too many outstanding PING frames")

// PingTracker tracks outstanding PING frames and measures round-trip time.
// HTTP/2 PING carries an 8-byte opaque payload that the receiver echoes in
// the ACK; the tracker matches ACKs to outstanding pings by payload.
type PingTracker struct {
	mu       sync.Mutex
	pending  map[[8]byte]time.Time
	maxPend  int
	deadline time.Duration
}

// NewPingTracker creates a tracker that allows at most maxPending outstanding
// pings and declares a ping dead if its ACK has not arrived within deadline.
// A deadline of 0 disables the per-ping timeout.
func NewPingTracker(maxPending int, deadline time.Duration) *PingTracker {
	if maxPending <= 0 {
		maxPending = 4
	}
	return &PingTracker{
		pending:  make(map[[8]byte]time.Time),
		maxPend:  maxPending,
		deadline: deadline,
	}
}

// Sent records a PING sent at time at with the given opaque payload.
// Returns ErrTooManyPings if the outstanding-ping limit is already reached.
func (pt *PingTracker) Sent(data [8]byte, at time.Time) error {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if len(pt.pending) >= pt.maxPend {
		return fmt.Errorf("%w: %d already outstanding", ErrTooManyPings, pt.maxPend)
	}
	pt.pending[data] = at
	return nil
}

// Received records a PING ACK with the given opaque payload arriving at time at.
// Returns the round-trip time on success.
// Returns ErrPingTimeout if the ACK arrived after the deadline.
// Returns an error if the payload does not match any outstanding ping.
func (pt *PingTracker) Received(data [8]byte, at time.Time) (time.Duration, error) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	sent, ok := pt.pending[data]
	if !ok {
		return 0, fmt.Errorf("unexpected PING ACK payload: %x", data)
	}
	delete(pt.pending, data)
	rtt := at.Sub(sent)
	if pt.deadline > 0 && rtt > pt.deadline {
		return 0, fmt.Errorf("%w: rtt %s", ErrPingTimeout, rtt)
	}
	return rtt, nil
}

// Expired removes and returns all outstanding PING payloads whose deadline
// has elapsed as of now. Each returned payload represents a presumed-dead
// connection health check.
func (pt *PingTracker) Expired(now time.Time) [][8]byte {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	var out [][8]byte
	for data, sent := range pt.pending {
		if pt.deadline > 0 && now.Sub(sent) > pt.deadline {
			out = append(out, data)
		}
	}
	for _, data := range out {
		delete(pt.pending, data)
	}
	return out
}
```

### The runnable demo

The demo sends one ping at a fixed time and acknowledges it 8 ms later, so the reported round-trip is exactly 8 ms — deterministic because both times are supplied explicitly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/ping-keepalive"
)

func main() {
	pt := ping.NewPingTracker(4, 5*time.Second)

	var payload [8]byte
	copy(payload[:], "demo0001")

	now := time.Now()
	if err := pt.Sent(payload, now); err != nil {
		log.Fatal(err)
	}

	rtt, err := pt.Received(payload, now.Add(8*time.Millisecond))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("PING round-trip: %v\n", rtt)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
PING round-trip: 8ms
```

### Tests

`TestPingTrackerRoundTrip` confirms a matched ACK yields the exact RTT. `TestPingTrackerRejectsUnknownACK` rejects an echo with no outstanding ping. `TestPingTrackerDetectsExpired` sweeps a ping sent in the past and confirms a second sweep returns nothing. `TestPingTrackerEnforcesConcurrencyLimit` fills the cap and asserts the next `Sent` returns `ErrTooManyPings`. `TestPingTrackerZeroDeadlineNeverExpires` confirms a zero deadline disables timeouts. `TestPingTrackerTimeoutOnLateACK` supplies an ACK far past the deadline and asserts `ErrPingTimeout`.

Create `ping_test.go`:

```go
package ping

import (
	"errors"
	"testing"
	"time"
)

func TestPingTrackerRoundTrip(t *testing.T) {
	t.Parallel()
	pt := NewPingTracker(4, 5*time.Second)
	var payload [8]byte
	copy(payload[:], "testping")
	now := time.Now()
	if err := pt.Sent(payload, now); err != nil {
		t.Fatalf("Sent: %v", err)
	}
	rtt, err := pt.Received(payload, now.Add(10*time.Millisecond))
	if err != nil {
		t.Fatalf("Received: %v", err)
	}
	if rtt != 10*time.Millisecond {
		t.Errorf("rtt = %v, want 10ms", rtt)
	}
}

func TestPingTrackerRejectsUnknownACK(t *testing.T) {
	t.Parallel()
	pt := NewPingTracker(4, time.Second)
	var payload [8]byte
	copy(payload[:], "unknown!")
	_, err := pt.Received(payload, time.Now())
	if err == nil {
		t.Error("Received with no matching Sent should return error")
	}
}

func TestPingTrackerDetectsExpired(t *testing.T) {
	t.Parallel()
	deadline := 100 * time.Millisecond
	pt := NewPingTracker(4, deadline)
	var payload [8]byte
	copy(payload[:], "expiring")
	past := time.Now().Add(-200 * time.Millisecond) // sent well in the past
	if err := pt.Sent(payload, past); err != nil {
		t.Fatalf("Sent: %v", err)
	}
	expired := pt.Expired(time.Now())
	if len(expired) != 1 || expired[0] != payload {
		t.Errorf("Expired = %v, want [%x]", expired, payload)
	}
	// After Expired drains it, a second call should return nothing.
	if again := pt.Expired(time.Now()); len(again) != 0 {
		t.Errorf("second Expired = %v, want empty", again)
	}
}

func TestPingTrackerEnforcesConcurrencyLimit(t *testing.T) {
	t.Parallel()
	pt := NewPingTracker(2, time.Second)
	var p1, p2, p3 [8]byte
	p1[0], p2[0], p3[0] = 1, 2, 3
	now := time.Now()
	if err := pt.Sent(p1, now); err != nil {
		t.Fatalf("first Sent: %v", err)
	}
	if err := pt.Sent(p2, now); err != nil {
		t.Fatalf("second Sent: %v", err)
	}
	if err := pt.Sent(p3, now); !errors.Is(err, ErrTooManyPings) {
		t.Errorf("third Sent: err = %v, want ErrTooManyPings", err)
	}
}

func TestPingTrackerZeroDeadlineNeverExpires(t *testing.T) {
	t.Parallel()
	pt := NewPingTracker(4, 0) // 0 = no deadline
	var payload [8]byte
	past := time.Now().Add(-24 * time.Hour)
	if err := pt.Sent(payload, past); err != nil {
		t.Fatalf("Sent: %v", err)
	}
	if expired := pt.Expired(time.Now()); len(expired) != 0 {
		t.Errorf("zero deadline: Expired = %v, want empty", expired)
	}
	// ACK after 24h still succeeds with zero deadline.
	rtt, err := pt.Received(payload, time.Now())
	if err != nil {
		t.Fatalf("Received: %v", err)
	}
	if rtt < 24*time.Hour {
		t.Errorf("rtt = %v, want >= 24h", rtt)
	}
}

func TestPingTrackerTimeoutOnLateACK(t *testing.T) {
	t.Parallel()
	pt := NewPingTracker(4, 5*time.Second)
	var payload [8]byte
	copy(payload[:], "lateping")
	now := time.Now()
	if err := pt.Sent(payload, now); err != nil {
		t.Fatalf("Sent: %v", err)
	}
	// ACK arrives 10s after send, well past the 5s deadline.
	_, err := pt.Received(payload, now.Add(10*time.Second))
	if !errors.Is(err, ErrPingTimeout) {
		t.Errorf("Received late ACK: err = %v, want ErrPingTimeout", err)
	}
}
```

## Review

The tracker is correct when an ACK is credited only to the exact ping it echoes and every time comparison uses the supplied arguments rather than a hidden clock read. The first mistake is accepting an unmatched ACK — it has no send time to subtract, so the RTT is meaningless and the right answer is an error. The second is forgetting the concurrency cap: an unbounded keepalive loop that never blocks on outstanding pings is itself the abuse RFC 9113 §10.5 warns about, so `Sent` must refuse once `maxPending` are in flight. Keep `Sent`, `Received`, and `Expired` all under the one mutex, since a keepalive goroutine sending pings races a reader goroutine processing ACKs; `-race` validates that pairing. Pass times in rather than calling `time.Now()` inside, which is what lets the late-ACK and expiry tests be exact instead of flaky.

## Resources

- [RFC 9113 §6.7 — PING](https://httpwg.org/specs/rfc9113.html#PING) — the 8-byte opaque payload, the ACK flag, and the stream-0 restriction.
- [RFC 9113 §10.5 — Denial-of-Service Considerations](https://httpwg.org/specs/rfc9113.html#DoSConsiderations) — why excessive PINGs are an abuse vector and the basis for the concurrency cap.
- [time.Duration](https://pkg.go.dev/time#Duration) — the round-trip measurement type and its formatting.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-goaway-shutdown.md](03-goaway-shutdown.md) | Next: [05-graceful-drain.md](05-graceful-drain.md)
