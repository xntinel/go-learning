# Exercise 2: Injecting the Hidden Seams — Time and Randomness

The dependencies people forget to inject are the implicit calls to global state
buried inside otherwise pure logic: `time.Now()` and the random source. Code that
calls them directly cannot be tested for the exact values it produces, because
those values change on every run. This exercise extracts both into tiny injected
interfaces — a `Clock` and an `IDGen` — and builds an `AuditLog` whose timestamps
and IDs are exactly assertable in tests because the test owns the clock and the
generator.

This module is fully self-contained. It starts with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
audit.go             Clock + IDGen interfaces, SystemClock / RandomIDGen real impls,
                     Event, AuditLog, NewAuditLog (rejects nil), Record, Events
cmd/
  demo/
    main.go          wire demo-local deterministic doubles so the demo is reproducible
audit_test.go        fixed and advancing fake clocks, a counter IDGen; assert exact
                     IDs and timestamps, plus the real impls produce sane values
```

- Files: `audit.go`, `cmd/demo/main.go`, `audit_test.go`.
- Implement: `NewAuditLog(clock, ids)` returning `(*AuditLog, error)`, and `Record(action string) Event` that stamps each event from the injected clock and generator.
- Test: assert exact IDs and timestamps with fake doubles; assert an advancing clock yields increasing timestamps; assert the real `RandomIDGen` is non-empty and distinct.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p injecting-seams/cmd/demo && cd injecting-seams
go mod init example.com/injecting-seams
```

### Why time and randomness must be dependencies

Consider the natural way to write `Record`: stamp the event with `time.Now()` and
an ID from `crypto/rand`, append it, return it. It works, ships, and is
untestable. A test cannot assert the timestamp because it is the wall clock at the
instant the test ran, different every time; it cannot assert the ID because it is
random by construction. The test degrades to checking only that the timestamp is
non-zero and the ID is non-empty — it can never pin the actual values, which is
exactly the behavior that matters when these fields feed an audit trail, a cache
key, or an idempotency token.

The seam is the place you cut the dependency on global state and pass an
implementation in. `Clock` is a one-method interface, `Now() time.Time`; `IDGen`
is one method, `NewID() string`. The package ships the production implementations —
`SystemClock`, whose `Now` returns `time.Now()`, and `RandomIDGen`, whose `NewID`
draws eight bytes from `crypto/rand` and hex-encodes them — and `NewAuditLog`
accepts both as interfaces. In production you wire the real pair. In a test you
wire a `fixedClock` that always returns the same configured instant and a counter
generator that returns `evt-1`, `evt-2`, `evt-3`, and now `Record`'s output is
fully determined: the test asserts the exact ID and the exact timestamp because it
supplied both sources. The very same code path that is nondeterministic in
production is exact under test, and the only thing that changed is which `Clock`
and `IDGen` the caller passed.

A subtle bonus falls out of this design: a fake clock does not have to be
constant. An advancing clock that returns its current instant and then steps
forward lets a test exercise time-dependent logic — ordering, expiry, debouncing —
deterministically, replaying any timeline it likes without ever sleeping.

Create `audit.go`:

```go
package audit

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// Clock is the time seam: the one method AuditLog needs from "what time is it".
type Clock interface {
	Now() time.Time
}

// IDGen is the identity seam: the one method AuditLog needs to mint an ID.
type IDGen interface {
	NewID() string
}

// SystemClock is the production Clock; Now reads the wall clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// RandomIDGen is the production IDGen; NewID draws from crypto/rand. Since Go
// 1.24 crypto/rand.Read is documented never to fail and never to return short,
// so the result needs no error handling.
type RandomIDGen struct{}

func (RandomIDGen) NewID() string {
	var b [8]byte
	rand.Read(b[:])
	return "evt-" + hex.EncodeToString(b[:])
}

// Event is one recorded action, stamped with an injected ID and timestamp.
type Event struct {
	ID     string
	Action string
	At     time.Time
}

// AuditLog records events, taking its ID and timestamp from injected seams
// rather than from package-level globals.
type AuditLog struct {
	clock  Clock
	ids    IDGen
	events []Event
}

// NewAuditLog injects the two seams and rejects nil.
func NewAuditLog(clock Clock, ids IDGen) (*AuditLog, error) {
	if clock == nil {
		return nil, errors.New("audit: clock is required")
	}
	if ids == nil {
		return nil, errors.New("audit: id generator is required")
	}
	return &AuditLog{clock: clock, ids: ids}, nil
}

// Record stamps an event from the injected clock and generator, stores it, and
// returns it. With deterministic doubles the returned Event is fully predictable.
func (l *AuditLog) Record(action string) Event {
	e := Event{
		ID:     l.ids.NewID(),
		Action: action,
		At:     l.clock.Now(),
	}
	l.events = append(l.events, e)
	return e
}

// Events returns a copy of the recorded events so callers cannot mutate the log.
func (l *AuditLog) Events() []Event {
	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out
}
```

`Record` reads from `l.clock` and `l.ids`, never from `time.Now()` or `rand`
directly — that indirection is the entire point. `Events` returns a copy so a
caller iterating the log cannot reach in and rewrite history. Note that the
package's own production types are `SystemClock` and `RandomIDGen`; the
deterministic doubles needed to make output predictable live with the code that
needs predictability — the demo and the tests — not in the library.

### The runnable demo

A demo that wired `SystemClock` and `RandomIDGen` would print a different
timestamp and different IDs on every run, so its output could never be documented.
Instead the demo wires demo-local deterministic doubles — a clock frozen at a
fixed instant and a counter generator — which is itself the lesson made concrete:
because the seams are injected, even the demo controls time and identity, and its
output is fixed and reproducible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/injecting-seams"
)

// fixedClock is a demo-local double: every Now returns the same instant.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// seqIDs is a demo-local double: a deterministic counter generator.
type seqIDs struct{ n int }

func (s *seqIDs) NewID() string {
	s.n++
	return fmt.Sprintf("evt-%d", s.n)
}

func main() {
	// Inject deterministic seams so this demo's output is fully reproducible.
	clock := fixedClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log, err := audit.NewAuditLog(clock, &seqIDs{})
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	for _, action := range []string{"login", "update-profile", "logout"} {
		e := log.Record(action)
		fmt.Printf("recorded %s action=%s at=%s\n", e.ID, e.Action, e.At.Format(time.RFC3339))
	}
	fmt.Printf("total events: %d\n", len(log.Events()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
recorded evt-1 action=login at=2026-01-01T00:00:00Z
recorded evt-2 action=update-profile at=2026-01-01T00:00:00Z
recorded evt-3 action=logout at=2026-01-01T00:00:00Z
total events: 3
```

### Tests

The tests are where injecting the seams pays off. `TestRecord_UsesInjectedSeams`
wires a fixed clock and a counter generator and asserts the *exact* IDs and the
*exact* timestamp — assertions that are impossible against the real clock and
random source. `TestRecord_AdvancingClockOrdersEvents` wires a clock that steps
forward on every read and proves the second event's timestamp is exactly one step
after the first, exercising time-dependent behavior with no sleeping.
`TestNewAuditLog_RejectsNil` pins the construction guards, and
`TestRealImpls_ProduceSaneValues` exercises the production `SystemClock` and
`RandomIDGen` so neither is dead code: the clock is non-zero and two generated IDs
are non-empty and distinct.

Create `audit_test.go`:

```go
package audit

import (
	"testing"
	"time"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type tickClock struct {
	t    time.Time
	step time.Duration
}

func (c *tickClock) Now() time.Time {
	now := c.t
	c.t = c.t.Add(c.step)
	return now
}

type seqIDs struct{ n int }

func (s *seqIDs) NewID() string {
	s.n++
	return "evt-" + itoa(s.n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestRecord_UsesInjectedSeams(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	log, err := NewAuditLog(fixedClock{t: at}, &seqIDs{})
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}

	e1 := log.Record("login")
	e2 := log.Record("logout")

	if e1.ID != "evt-1" || e2.ID != "evt-2" {
		t.Errorf("ids = %q, %q; want evt-1, evt-2", e1.ID, e2.ID)
	}
	if !e1.At.Equal(at) || !e2.At.Equal(at) {
		t.Errorf("timestamps = %v, %v; want both %v", e1.At, e2.At, at)
	}
	if got := log.Events(); len(got) != 2 {
		t.Fatalf("events len = %d, want 2", len(got))
	}
}

func TestRecord_AdvancingClockOrdersEvents(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	log, _ := NewAuditLog(&tickClock{t: start, step: time.Second}, &seqIDs{})

	first := log.Record("a")
	second := log.Record("b")

	if !first.At.Equal(start) {
		t.Errorf("first.At = %v, want %v", first.At, start)
	}
	if got := second.At.Sub(first.At); got != time.Second {
		t.Errorf("gap = %v, want 1s", got)
	}
}

func TestNewAuditLog_RejectsNil(t *testing.T) {
	t.Parallel()

	if _, err := NewAuditLog(nil, &seqIDs{}); err == nil {
		t.Error("expected error for nil clock")
	}
	if _, err := NewAuditLog(fixedClock{}, nil); err == nil {
		t.Error("expected error for nil id generator")
	}
}

func TestRealImpls_ProduceSaneValues(t *testing.T) {
	t.Parallel()

	if (SystemClock{}).Now().IsZero() {
		t.Error("SystemClock.Now returned the zero time")
	}

	g := RandomIDGen{}
	a, b := g.NewID(), g.NewID()
	if a == "" || b == "" {
		t.Errorf("RandomIDGen produced an empty ID: %q, %q", a, b)
	}
	if a == b {
		t.Errorf("RandomIDGen produced duplicate IDs: %q", a)
	}
}
```

## Review

The seam is correct when no production logic reaches for a global: `Record` calls
`l.clock.Now()` and `l.ids.NewID()` and never `time.Now()` or `rand` directly, so
the test that supplies a fixed clock and a counter generator can assert the exact
timestamp and the exact ID. That exactness — `evt-1` and a known instant rather
than "non-empty" and "non-zero" — is the whole return on extracting the seams.
Confirm the advancing-clock test orders events deterministically with no sleeping,
and that the real `SystemClock` and `RandomIDGen` are exercised by a test so they
are not unverified dead code.

The common mistakes are two. The first is leaving `time.Now()` or `rand` inline in
the logic and only injecting the "real" dependencies like databases; the implicit
ones are the dependencies that most often make a test flaky or imprecise, and they
are the cheapest to extract — a one-method interface each. The second is letting
the fake clock leak into production or the production clock leak into a test: keep
`SystemClock`/`RandomIDGen` in the library and the deterministic doubles next to
the demo and tests that need reproducibility, wired only at the composition root.

## Resources

- [`time` package](https://pkg.go.dev/time) — `time.Time`, `time.Date`, and the formatting the clock seam produces.
- [`crypto/rand`](https://pkg.go.dev/crypto/rand) — the production random source, whose `Read` is guaranteed since Go 1.24 never to fail or return short.
- [Working Effectively with Legacy Code: seams](https://www.artima.com/articles/working-effectively-with-legacy-code) — Michael Feathers on the seam, the place you substitute behavior without editing the code in place.

---

Back to [01-constructor-injection.md](01-constructor-injection.md) | Next: [03-provider-container.md](03-provider-container.md)
