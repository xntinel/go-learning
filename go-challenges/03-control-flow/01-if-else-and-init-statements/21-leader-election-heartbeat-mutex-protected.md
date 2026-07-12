# Exercise 21: Leader Election Heartbeat: Concurrent Lease Renewal and Demotion

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A distributed system where multiple identical instances could all decide
they are in charge needs exactly one of them acting as leader at any
moment, or two instances will both run a scheduled job, both write to a
singleton resource, or both think they own a partition. Each instance
renews a time-bound lease on a heartbeat; whichever one holds an unexpired
lease is the leader, and the decision to claim, renew, or deny has to be
made atomically or two instances renewing at the same instant could both
believe they won. This module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
leaderlease/                independent module: example.com/leader-election-heartbeat-mutex-protected
  go.mod                    go 1.24
  lease.go                  Lease (mutex-protected), Renew(nodeID, now, ttl), Holder()
  cmd/
    demo/
      main.go               two nodes contend for leadership as time advances
  lease_test.go              sequential claim/renew/expire/handoff; concurrent renew -race
```

- Files: `lease.go`, `cmd/demo/main.go`, `lease_test.go`.
- Implement: a `Lease` struct guarded by a `sync.Mutex` with `Renew(nodeID string, now time.Time, ttl time.Duration) (leader bool, term int)`, where the init-statement `if expired := now.After(l.expiresAt); l.holder == "" || expired` decides a fresh claim, followed by guards for "already the leader, extend it" and "someone else holds it, deny."
- Test: sequential claim, rival denial, renewal before expiry, and handoff to a new leader once the old lease lapses; a concurrent test with many goroutines calling `Renew` with distinct node IDs at the same instant, asserting exactly one gets `leader == true`, run under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the claim decision has to happen inside the lock, not around it

The tempting-but-wrong design reads the current holder and expiry outside
a lock, decides what to do, and only locks to write the new state. Between
the read and the write, another goroutine can make the exact same
decision from the exact same stale data — two nodes both see "no live
holder" and both write themselves in as leader, with the second write
silently overwriting the first's claim while both callers still believe
they won. `Renew` avoids this by taking the lock first and keeping the
entire read-decide-write sequence inside one critical section: whichever
goroutine acquires the mutex first sees the true current state, including
any write a concurrent goroutine just made, and the loser's decision is
based on that fact rather than a stale snapshot.

The init-statement `if expired := now.After(l.expiresAt); l.holder == "" || expired`
computes `expired` once and reuses it in the condition — a small but
deliberate choice, since `now.After(l.expiresAt)` would otherwise need to
be written twice if the empty-holder case and the expired-holder case were
separate guards, and repeating a condition is a place where the two copies
can quietly drift apart during a later edit.

Create `lease.go`:

```go
// Package leaderlease implements a mutex-protected leadership lease: nodes
// call Renew periodically, and whoever holds a live, unexpired lease is the
// leader.
package leaderlease

import (
	"sync"
	"time"
)

// Lease tracks who holds leadership and until when. It is safe for
// concurrent use; the zero value is an unheld lease ready to use.
type Lease struct {
	mu        sync.Mutex
	holder    string
	expiresAt time.Time
	term      int
}

// Renew attempts to claim or renew leadership for nodeID at now for ttl. It
// reports whether nodeID is the leader after the call and the current term.
// The lookup of the current holder, the expiry check, and the decision to
// claim, renew, or deny all happen inside one critical section, so two
// concurrent callers can never both believe they hold the same term.
func (l *Lease) Renew(nodeID string, now time.Time, ttl time.Duration) (leader bool, term int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if expired := now.After(l.expiresAt); l.holder == "" || expired {
		// No holder yet, or the previous holder's lease lapsed: nodeID claims it.
		l.holder = nodeID
		l.expiresAt = now.Add(ttl)
		l.term++
		return true, l.term
	}

	if l.holder == nodeID {
		// Already the leader and the lease is still live: extend it.
		l.expiresAt = now.Add(ttl)
		return true, l.term
	}

	// Someone else holds a live lease: deny.
	return false, l.term
}

// Holder returns the current holder and term, for inspection.
func (l *Lease) Holder() (holder string, term int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holder, l.term
}
```

### The runnable demo

The demo plays out a full handoff: node-a claims term 1, node-b is denied,
node-a renews before its lease expires, node-b is denied again, then
node-a stops renewing — once the clock passes its extended expiry,
node-b claims term 2, and node-a's next attempt is denied.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	leaderlease "example.com/leader-election-heartbeat-mutex-protected"
)

func main() {
	lease := &leaderlease.Lease{}
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	ttl := 5 * time.Second

	step := func(label, node string, offset time.Duration) {
		leader, term := lease.Renew(node, t0.Add(offset), ttl)
		fmt.Printf("%-24s node=%-8s leader=%-5v term=%d\n", label, node, leader, term)
	}

	step("t+0s node-a claims", "node-a", 0)
	step("t+0s node-b tries", "node-b", 0)
	step("t+2s node-a renews", "node-a", 2*time.Second)
	step("t+3s node-b tries", "node-b", 3*time.Second)
	step("t+8s node-b claims", "node-b", 8*time.Second)
	step("t+9s node-a tries", "node-a", 9*time.Second)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
t+0s node-a claims       node=node-a   leader=true  term=1
t+0s node-b tries        node=node-b   leader=false term=1
t+2s node-a renews       node=node-a   leader=true  term=1
t+3s node-b tries        node=node-b   leader=false term=1
t+8s node-b claims       node=node-b   leader=true  term=2
t+9s node-a tries        node=node-a   leader=false term=2
```

### Tests

The sequential test walks the same handoff scenario as the demo and
asserts each `leader`/`term` pair. The concurrency test fires 64 goroutines
with distinct node IDs at the same `now` and asserts exactly one comes back
`leader == true`, run under `-race` to prove the critical section actually
serializes the decision.

Create `lease_test.go`:

```go
package leaderlease

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRenewSequence(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	ttl := 5 * time.Second
	lease := &Lease{}

	leader, term := lease.Renew("node-a", t0, ttl)
	if !leader || term != 1 {
		t.Fatalf("initial claim: leader=%v term=%d, want true 1", leader, term)
	}

	leader, term = lease.Renew("node-b", t0, ttl)
	if leader || term != 1 {
		t.Fatalf("rival at same instant: leader=%v term=%d, want false 1", leader, term)
	}

	leader, term = lease.Renew("node-a", t0.Add(2*time.Second), ttl)
	if !leader || term != 1 {
		t.Fatalf("renew before expiry: leader=%v term=%d, want true 1", leader, term)
	}

	// node-a stops renewing; once now is past the extended expiry, node-b claims
	// a new term.
	leader, term = lease.Renew("node-b", t0.Add(8*time.Second), ttl)
	if !leader || term != 2 {
		t.Fatalf("reclaim after expiry: leader=%v term=%d, want true 2", leader, term)
	}

	leader, term = lease.Renew("node-a", t0.Add(9*time.Second), ttl)
	if leader || term != 2 {
		t.Fatalf("stale node-a after handoff: leader=%v term=%d, want false 2", leader, term)
	}
}

func TestConcurrentRenewExactlyOneLeader(t *testing.T) {
	t.Parallel()

	lease := &Lease{}
	now := time.Now()
	ttl := time.Minute

	const n = 64
	results := make([]bool, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			leader, _ := lease.Renew(fmt.Sprintf("node-%d", i), now, ttl)
			results[i] = leader
		}(i)
	}
	wg.Wait()

	leaders := 0
	for _, r := range results {
		if r {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("got %d concurrent leaders, want exactly 1", leaders)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

The property that matters is not that `Renew` returns the right answer for
one caller — it is that across any number of concurrent callers, exactly
one term ever has exactly one leader. That only holds because the claim
decision and the state write happen inside the same lock acquisition;
splitting them, even briefly, reopens the double-claim window the whole
exercise exists to close. Carry this forward: any "check current state,
then act on it" decision shared across goroutines needs the check and the
act in one critical section, not two.

## Resources

- [etcd: Lease documentation](https://etcd.io/docs/latest/learning/api/#lease-api) — a production system that implements this exact renew-or-expire lease pattern.
- [The Raft Consensus Algorithm](https://raft.github.io/) — the broader leader-election protocol this heartbeat lease is a simplified piece of.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the primitive that keeps the decision atomic.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-blob-storage-retry-exponential-backoff.md](20-blob-storage-retry-exponential-backoff.md) | Next: [22-outbox-pattern-transactional-publish.md](22-outbox-pattern-transactional-publish.md)
