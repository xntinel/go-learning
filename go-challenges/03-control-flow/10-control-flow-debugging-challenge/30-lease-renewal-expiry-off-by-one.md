# Exercise 30: Lease Renewal Happens Too Late Due to Expiry Off-By-One

**Nivel: Intermedio** — validacion rapida (un test corto).

A distributed lock built on a time-bound lease is only as safe as the
holder's ability to renew it *before* it lapses — the moment it
lapses, any other process racing to acquire the same lock is entitled
to win, and two processes briefly believing they both hold the lock is
exactly the split-brain scenario the lease exists to prevent. A
renewal check written as "has the deadline already passed" gets the
comparison direction right but the timing catastrophically wrong: by
the time `now > expiry` is true, the lease is already gone, and the
renewal request still has to serialize, cross the network, and be
processed before it can possibly land — every one of those
milliseconds is time a competitor can use to acquire the lock first.
The fix is not a different comparison operator; it is renewing against
a deadline that has a safety margin built in, so the renewal request
is already on the wire well before the real expiry arrives. This
module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
lease/                       independent module: example.com/lease-renewal-expiry-off-by-one
  go.mod                      go 1.21
  lease.go                     Lease, New, NeedsRenewal, Renew, Expiry
  cmd/
    demo/
      main.go                  runnable demo: a lease approaching, crossing, and re-approaching its renewal threshold
  lease_test.go                  boundary table around the renewal threshold, plus a post-Renew window case
```

- Files: `lease.go`, `cmd/demo/main.go`, `lease_test.go`.
- Implement: `Lease.NeedsRenewal() bool` that signals true once only a safety margin remains before expiry, not only once expiry has already passed.
- Test: a table of clock positions relative to the renewal threshold (well before, one tick before, exactly at, past it, past raw expiry); a further case asserting the renewal window recurs relative to the *new* expiry after `Renew`.
- Verify: `go test -count=1 ./...`.

### Why "already past the deadline" is the wrong question to ask

The version that ships first reads as the obviously correct
translation of "is the lease expired":

```go
// BUG: waits until the lease is already gone before signaling a renewal
// is needed -- with no margin, a renewal request sent at this instant
// still has to cross the network before it can possibly land.
func (l *Lease) NeedsRenewal() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.now().After(l.expiry)
}
```

This passes any test that only checks "does the lease eventually
report needing renewal" — it does, at the exact instant it lapses.
That is precisely the problem: a real renewal is not instantaneous. It
has to be noticed by whatever polling loop calls `NeedsRenewal`, then
serialized into a request, then sent over the network, then processed
by whatever coordination service tracks the lease — and every one of
those steps takes time the holder no longer has, because by
definition the lease already expired the instant this check first
returned `true`. A competing process polling the same lock can win the
acquisition in that gap, and now two processes both believe they hold
an exclusive lease, which is the exact failure mode leases exist to
rule out.

The fix does not change what "expired" means; it changes *when* the
holder is told to act, moving the signal earlier by a fixed safety
margin:

```go
func (l *Lease) NeedsRenewal() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	threshold := l.expiry.Add(-l.margin)
	return !l.now().Before(threshold)
}
```

`!now.Before(threshold)` is deliberately `>=` rather than `>`: the exact
instant the clock reaches the threshold must already report `true`, not
one tick later, because "the moment renewal becomes due" is itself a
boundary a holder's renewal loop needs to catch on its very next check,
not the check after. With `margin` set to a fraction of `duration`, a
renewal request has that entire margin's worth of time to complete
before the real deadline — turning a single unforgiving instant into a
window with room for network latency and scheduling jitter.

Create `lease.go`:

```go
package lease

import (
	"sync"
	"time"
)

// Lease is a time-bound distributed-lock lease: it must be renewed before
// expiry, or another process is free to acquire it. now is injected so
// tests and demos control elapsed time exactly.
type Lease struct {
	mu       sync.Mutex
	duration time.Duration
	margin   time.Duration
	expiry   time.Time
	now      func() time.Time
}

// New creates a Lease that expires duration from now, renewing margin
// before its deadline rather than waiting until the deadline itself.
func New(duration, margin time.Duration, now func() time.Time) *Lease {
	return &Lease{
		duration: duration,
		margin:   margin,
		expiry:   now().Add(duration),
		now:      now,
	}
}

// NeedsRenewal reports whether the lease should be renewed now. Rather
// than waiting until the lease has actually expired -- by which point a
// renewal request in flight can lose the race to a competing acquirer --
// it signals true once only margin remains before the deadline, at or
// past the renewal threshold.
func (l *Lease) NeedsRenewal() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	threshold := l.expiry.Add(-l.margin)
	return !l.now().Before(threshold)
}

// Renew extends the lease by duration from now.
func (l *Lease) Renew() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.expiry = l.now().Add(l.duration)
}

// Expiry returns the current expiry deadline.
func (l *Lease) Expiry() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.expiry
}
```

### The runnable demo

A 10-second lease with a 2-second safety margin: the demo checks at
7s (not yet due), 8s (exactly at the threshold), renews, then checks
again at 15s and 16s relative to the *new* expiry.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/lease-renewal-expiry-off-by-one"
)

func main() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := base
	now := func() time.Time { return clock }

	l := lease.New(10*time.Second, 2*time.Second, now)
	fmt.Println("initial expiry:", l.Expiry().Sub(base))

	clock = base.Add(7 * time.Second)
	fmt.Println("t=7s needs renewal:", l.NeedsRenewal())

	clock = base.Add(8 * time.Second)
	fmt.Println("t=8s needs renewal:", l.NeedsRenewal())

	l.Renew()
	fmt.Println("renewed; new expiry offset:", l.Expiry().Sub(base))

	clock = base.Add(15 * time.Second)
	fmt.Println("t=15s needs renewal:", l.NeedsRenewal())

	clock = base.Add(16 * time.Second)
	fmt.Println("t=16s needs renewal:", l.NeedsRenewal())
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
initial expiry: 10s
t=7s needs renewal: false
t=8s needs renewal: true
renewed; new expiry offset: 18s
t=15s needs renewal: false
t=16s needs renewal: true
```

### Tests

`TestNeedsRenewalTable` covers five clock positions relative to the
8-second renewal threshold (10s duration, 2s margin): well before,
one millisecond before, exactly at, just past it, and past the raw
10-second expiry itself — the threshold case is the one a `>` instead
of `>=` comparison would get wrong. `TestRenewResetsExpiryAndRenewalWindow`
confirms the window recurs relative to the *new* expiry after a
renewal, not the original one.

Create `lease_test.go`:

```go
package lease

import (
	"testing"
	"time"
)

func TestNeedsRenewalTable(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	// duration=10s, margin=2s -> renewal threshold sits at t=8s.

	tests := []struct {
		name string
		at   time.Duration // offset from base
		want bool
	}{
		{"well before the renewal threshold", 5 * time.Second, false},
		{"one tick before the threshold", 7999 * time.Millisecond, false},
		{"exactly at the renewal threshold", 8 * time.Second, true},
		{"past the threshold but before expiry", 9 * time.Second, true},
		{"past the raw expiry itself", 11 * time.Second, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			clock := base
			now := func() time.Time { return clock }
			l := New(10*time.Second, 2*time.Second, now)

			clock = base.Add(tc.at)
			if got := l.NeedsRenewal(); got != tc.want {
				t.Fatalf("NeedsRenewal() at t=%v = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

func TestRenewResetsExpiryAndRenewalWindow(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := base
	now := func() time.Time { return clock }

	l := New(10*time.Second, 2*time.Second, now)

	clock = base.Add(8 * time.Second)
	if !l.NeedsRenewal() {
		t.Fatal("NeedsRenewal() = false at the threshold, want true")
	}
	l.Renew()

	if l.NeedsRenewal() {
		t.Fatal("NeedsRenewal() = true immediately after Renew, want false")
	}

	wantExpiry := base.Add(18 * time.Second)
	if got := l.Expiry(); !got.Equal(wantExpiry) {
		t.Fatalf("Expiry() = %v, want %v", got, wantExpiry)
	}

	// The renewal window must recur relative to the NEW expiry, not the
	// original one.
	clock = base.Add(16 * time.Second)
	if !l.NeedsRenewal() {
		t.Fatal("NeedsRenewal() = false 2s before the new expiry, want true")
	}
}
```

Run: `go test -count=1 ./...`.

## Review

`NeedsRenewal` is correct when it reports `true` at, and only at, a
fixed safety margin before the real deadline — proven with a table
that pins the exact millisecond on either side of that threshold, not
just "eventually true before the lease lapses." The mistake this
design avoids is treating "expired" and "needs renewal" as the same
question: a check against the raw `expiry` answers the first question
correctly but answers it too late for the second, because a renewal
that starts only once the lease is already gone can never win the race
against a competitor's acquisition attempt. Building the margin into
the comparison — `!now.Before(expiry.Add(-margin))`, with the
inclusive `>=` semantics that catch the threshold instant itself —
turns "renew right before it's too late" from a race that depends on
scheduling luck into a check with a deliberate, tunable amount of slack
built in.

## Resources

- [etcd: Lease documentation](https://etcd.io/docs/v3.5/learning/api/#lease-api) — TTL-based leases and the client's responsibility to renew (keep-alive) before expiry.
- [The Chubby lock service for loosely-coupled distributed systems](https://static.googleusercontent.com/media/research.google.com/en//archive/chubby-osdi06.pdf) — session leases and the operational necessity of a renewal safety margin.
- [time.Time.Before](https://pkg.go.dev/time#Time.Before) — the strict-inequality building block used here to derive an inclusive `>=` via negation.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-dns-cache-ttl-expiry-ignored.md](29-dns-cache-ttl-expiry-ignored.md) | Next: [31-consistent-hash-ring-rebalance.md](31-consistent-hash-ring-rebalance.md)
