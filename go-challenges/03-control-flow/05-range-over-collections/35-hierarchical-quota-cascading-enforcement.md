# Exercise 35: Hierarchical Quota Accounting with Cascading Enforcement

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A multi-tenant SaaS platform rarely enforces just one quota per request: an
organization has a monthly cap, each user within it has their own cap, and a
particularly expensive endpoint has a cap of its own — and a single request
must be checked against every one of those layers, together, before any of
them is charged. Deducting from the organization's counter first and only
then discovering the user's counter is already exhausted leaves the
organization's usage permanently overcharged for a request that never
actually completed, a subtle accounting leak that compounds over millions of
requests. This module ranges the layers a request touches in a fixed
precedence order, checks every one of them before committing to any of
them, and reports exactly which layer would have been the one to reject the
request — all as a single atomic operation under load. The module is fully
self-contained: its own `go mod init`, no external dependencies.

## What you'll build

```text
quota/                      independent module: example.com/hierarchical-quota-cascading-enforcement
  go.mod                    go 1.24
  quota.go                  type Manager; Consume, Release, Usage
  cmd/
    demo/
      main.go               runnable demo: org/user/endpoint layers, one rejection, one retry
  quota_test.go              table test: success/rejection/boundary cases; all-or-nothing charging; concurrent Consume under -race
```

- Files: `quota.go`, `cmd/demo/main.go`, `quota_test.go`.
- Implement: `Manager.Consume`, `Manager.Release`, and `Manager.Usage`, all
  synchronized under one `sync.Mutex`.
- Test: a three-case `Consume` table, an all-or-nothing charging case, a
  `Release` case, and a concurrent `Consume` case under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/35-hierarchical-quota-cascading-enforcement/cmd/demo
cd go-solutions/03-control-flow/05-range-over-collections/35-hierarchical-quota-cascading-enforcement
go mod edit -go=1.24
```

### Two ranges over the same layers: check everything, then commit everything

`Consume` ranges its `layers` parameter twice, and the split between those
two ranges is the entire correctness argument. The first range only reads —
for each layer, in the caller's precedence order, it computes what usage
*would* become and compares it to that layer's limit, returning immediately
the moment one layer would overflow, without having touched `m.used` for
any layer at all, including ones earlier in the precedence order that had
plenty of room. Only once that first range completes without finding a
single blocking layer does the second range run, and by that point every
layer is already known to have room, so the second range's `m.used[key] += amount`
can never itself trigger a rejection — it is pure, unconditional
commitment. Interleaving the check and the commit into a single range —
checking layer 1, charging it, checking layer 2, charging it, discovering
layer 3 is over its limit — is the bug this two-pass structure exists to
prevent: it would leave layers 1 and 2 charged for a request that the
system as a whole rejected, an overcharge with no corresponding request
ever having completed, and paying it back would require a second,
easy-to-forget rollback call on every rejection path instead of never
happening in the first place.

Both ranges execute inside one `m.mu.Lock()`/`defer m.mu.Unlock()` pair, not
two — the same "check and act in the same critical section" rule this
lesson's other concurrent exercises rely on. Without it, two concurrent
`Consume` calls for the same tight layer could each complete their own
first-range check while the layer still had exactly enough room for one of
them, and then both proceed to the second range and both charge it,
jointly overshooting the limit that the check was supposed to guarantee
against. Holding the lock across both ranges is what makes "verify room in
every layer, then charge every layer" one indivisible operation from every
other goroutine's point of view.

Create `quota.go`:

```go
package quota

import "sync"

// Manager tracks usage against named quota layers (an organization, a user,
// an endpoint — any string key) and enforces a single request's consumption
// across every layer it touches as one atomic operation: either every layer
// has room and all of them are charged, or none of them are.
type Manager struct {
	mu     sync.Mutex
	limits map[string]int
	used   map[string]int
}

// New builds an empty Manager.
func New() *Manager {
	return &Manager{
		limits: make(map[string]int),
		used:   make(map[string]int),
	}
}

// SetLimit configures layerKey's quota ceiling. A layer with no configured
// limit is treated as unlimited.
func (m *Manager) SetLimit(layerKey string, limit int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limits[layerKey] = limit
}

// Consume attempts to charge amount against every layer in layers, checked
// in the given precedence order. It ranges layers twice under one lock
// acquisition: the first range only checks whether every layer has room,
// touching no state; only if every layer clears that check does the second
// range actually deduct amount from each one. If any layer would be pushed
// over its limit, Consume charges nothing at all and reports the first
// layer (in precedence order) that blocked it.
func (m *Manager) Consume(amount int, layers ...string) (ok bool, blockedAt string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, key := range layers {
		limit, hasLimit := m.limits[key]
		if !hasLimit {
			continue
		}
		if m.used[key]+amount > limit {
			return false, key
		}
	}

	for _, key := range layers {
		m.used[key] += amount
	}
	return true, ""
}

// Release rolls back a previously committed consumption, for example when a
// request that reserved quota later fails downstream and the quota it
// reserved should not count against the caller. Usage is floored at zero so
// a mistaken double-release cannot drive a layer negative.
func (m *Manager) Release(amount int, layers ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, key := range layers {
		m.used[key] -= amount
		if m.used[key] < 0 {
			m.used[key] = 0
		}
	}
}

// Usage reports layerKey's current consumption.
func (m *Manager) Usage(layerKey string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used[layerKey]
}
```

### The runnable demo

The demo configures an org, a user, and an endpoint layer with progressively
tighter limits, consumes 10 units twice — the second call blocked by the
endpoint layer, the tightest of the three — then retries with a smaller
amount that fits.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/hierarchical-quota-cascading-enforcement"
)

func main() {
	m := quota.New()
	m.SetLimit("org:acme", 100)
	m.SetLimit("user:alice", 30)
	m.SetLimit("endpoint:/v1/search", 15)

	layers := []string{"org:acme", "user:alice", "endpoint:/v1/search"}

	ok1, blocked1 := m.Consume(10, layers...)
	fmt.Printf("consume(10) ok=%v blocked=%q\n", ok1, blocked1)

	ok2, blocked2 := m.Consume(10, layers...)
	fmt.Printf("consume(10) ok=%v blocked=%q\n", ok2, blocked2)

	fmt.Printf("usage: org=%d user=%d endpoint=%d\n",
		m.Usage("org:acme"), m.Usage("user:alice"), m.Usage("endpoint:/v1/search"))

	ok3, blocked3 := m.Consume(5, layers...)
	fmt.Printf("consume(5) ok=%v blocked=%q\n", ok3, blocked3)

	fmt.Printf("usage: org=%d user=%d endpoint=%d\n",
		m.Usage("org:acme"), m.Usage("user:alice"), m.Usage("endpoint:/v1/search"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
consume(10) ok=true blocked=""
consume(10) ok=false blocked="endpoint:/v1/search"
usage: org=10 user=10 endpoint=10
consume(5) ok=true blocked=""
usage: org=15 user=15 endpoint=15
```

### Tests

The table covers a request well under every limit, one that the tightest
layer blocks, and the exactly-at-the-limit boundary (allowed, not
rejected). A dedicated test proves a rejected request leaves every layer's
usage completely untouched, `TestRelease` proves rollback works and floors
at zero, and the concurrency test fires 30 simultaneous 10-unit requests
against a 100-unit limit and must admit exactly 10 of them, under `-race`.

Create `quota_test.go`:

```go
package quota

import (
	"sync"
	"testing"
)

func TestConsume(t *testing.T) {
	t.Parallel()

	build := func() *Manager {
		m := New()
		m.SetLimit("org:acme", 100)
		m.SetLimit("user:alice", 30)
		m.SetLimit("endpoint:/v1/search", 15)
		return m
	}
	layers := []string{"org:acme", "user:alice", "endpoint:/v1/search"}

	tests := []struct {
		name        string
		preload     int // amount already consumed on all three layers before the call under test
		amount      int
		wantOK      bool
		wantBlocked string
	}{
		{
			name:        "well under every limit succeeds",
			preload:     0,
			amount:      10,
			wantOK:      true,
			wantBlocked: "",
		},
		{
			name:        "endpoint layer is the tightest and blocks first",
			preload:     10,
			amount:      10, // org 20/100 ok, user 20/30 ok, endpoint 20/15 blocks
			wantOK:      false,
			wantBlocked: "endpoint:/v1/search",
		},
		{
			name:        "exactly at the limit is allowed, not rejected",
			preload:     5,
			amount:      10, // endpoint would land at exactly 15
			wantOK:      true,
			wantBlocked: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := build()
			if tc.preload > 0 {
				if ok, _ := m.Consume(tc.preload, layers...); !ok {
					t.Fatalf("setup preload of %d failed unexpectedly", tc.preload)
				}
			}

			ok, blocked := m.Consume(tc.amount, layers...)
			if ok != tc.wantOK {
				t.Fatalf("Consume() ok = %v, want %v", ok, tc.wantOK)
			}
			if blocked != tc.wantBlocked {
				t.Fatalf("Consume() blockedAt = %q, want %q", blocked, tc.wantBlocked)
			}
		})
	}
}

func TestConsumeRejectionChargesNoLayerAtAll(t *testing.T) {
	t.Parallel()

	m := New()
	m.SetLimit("org:acme", 100)
	m.SetLimit("user:alice", 30)
	m.SetLimit("endpoint:/v1/search", 15)
	layers := []string{"org:acme", "user:alice", "endpoint:/v1/search"}

	m.Consume(10, layers...)          // org=10, user=10, endpoint=10
	ok, _ := m.Consume(10, layers...) // would push endpoint to 20/15: must be rejected
	if ok {
		t.Fatalf("Consume() ok = true, want false")
	}

	if got := m.Usage("org:acme"); got != 10 {
		t.Fatalf("Usage(org:acme) = %d, want 10 (rejected consume must not charge any layer)", got)
	}
	if got := m.Usage("user:alice"); got != 10 {
		t.Fatalf("Usage(user:alice) = %d, want 10", got)
	}
	if got := m.Usage("endpoint:/v1/search"); got != 10 {
		t.Fatalf("Usage(endpoint:/v1/search) = %d, want 10", got)
	}
}

func TestRelease(t *testing.T) {
	t.Parallel()

	m := New()
	m.SetLimit("org:acme", 100)
	m.Consume(10, "org:acme")
	m.Release(4, "org:acme")

	if got := m.Usage("org:acme"); got != 6 {
		t.Fatalf("Usage(org:acme) after release = %d, want 6", got)
	}

	// Releasing more than was ever consumed must floor at zero, not go negative.
	m.Release(100, "org:acme")
	if got := m.Usage("org:acme"); got != 0 {
		t.Fatalf("Usage(org:acme) after over-release = %d, want 0", got)
	}
}

func TestConcurrentConsumeNeverExceedsLimit(t *testing.T) {
	t.Parallel()

	m := New()
	m.SetLimit("endpoint:/v1/search", 100)

	var mu sync.Mutex
	accepted := 0

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := m.Consume(10, "endpoint:/v1/search"); ok {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if accepted != 10 {
		t.Fatalf("accepted = %d, want exactly 10 (100 limit / 10 per request)", accepted)
	}
	if got := m.Usage("endpoint:/v1/search"); got != 100 {
		t.Fatalf("Usage(endpoint:/v1/search) = %d, want exactly 100", got)
	}
}
```

Run it:

```bash
go test -count=1 -race ./...
```

## Review

The manager is correct when a rejected `Consume` leaves every layer's usage
exactly as it was before the call, an accepted `Consume` charges every named
layer by the same amount, and `blockedAt` always names the first layer in
precedence order that would have overflowed. The bug this design
specifically avoids is charging layers as they pass their individual check
instead of checking all of them first: a request that clears the
organization and user layers but fails at the endpoint layer must never
leave the organization or user counters incremented for a request the
system as a whole rejected — `Consume`'s two full ranges over `layers`,
check then commit, both under one lock acquisition, are what guarantee
every request is charged everywhere or nowhere, never somewhere.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [Stripe API: Rate limiters](https://stripe.com/blog/rate-limiters) — a production discussion of multi-layer request accounting.
- [Go Specification: For statements (range over slice)](https://go.dev/ref/spec#For_statements)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [34-dns-service-discovery-ttl-cache.md](34-dns-service-discovery-ttl-cache.md) | Next: [../06-labels-break-continue-goto/00-concepts.md](../06-labels-break-continue-goto/00-concepts.md)
