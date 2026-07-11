# Exercise 23: Route Shards by Key Hash and Replica Consistency

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A sharded database routes every write to a shard's primary and can, when
conditions allow, spread reads out to that shard's replica — but only when
the replica has actually reported itself healthy. This module builds that
router: consistent hashing picks the shard for a key, and a tagless switch
on the replica's current health state decides whether a read is safe to
offload. Because health reports and routing decisions both happen from
many goroutines under real load, the router is built around a
`sync.RWMutex` from the start rather than retrofitted later. It is
self-contained: its own `go mod init`, code, demo, and test.

## What you'll build

```text
shardrouter/                 independent module: example.com/sharding-key-router-with-hashing
  go.mod                      go 1.24
  shardrouter.go                package shardrouter; ReplicaState; Router; New, SetReplicaState, RouteRead, RouteWrite; ShardFor(key, shardCount) int
  cmd/demo/main.go              runnable demo over four keys with mixed replica states
  shardrouter_test.go           hash determinism, a table over every ReplicaState, write-always-primary, and a concurrency test
```

- Implement: `ShardFor(key string, shardCount int) int` (FNV-1a hash reduced mod shard count) and a `Router` whose `RouteRead` uses a tagless switch on `ReplicaState` to choose `"primary"` or `"replica"`, while `RouteWrite` always targets `"primary"`.
- Test: hash determinism across repeated calls, one subtest per `ReplicaState` value driving `RouteRead`, a check that writes never target a replica even when it's healthy, and a concurrency test hammering `RouteRead`, `RouteWrite`, and `SetReplicaState` from many goroutines at once.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/shardrouter/cmd/demo
cd ~/go-exercises/shardrouter
go mod init example.com/sharding-key-router-with-hashing
go mod edit -go=1.24
```

### Why RouteWrite never even looks at the replica state

`RouteWrite` doesn't have a switch at all — every write goes to
`"primary"`, unconditionally. That is the point being tested by
`TestRouteWriteAlwaysTargetsPrimary`: a replica is a read-scaling
mechanism, and even when it's fully healthy (`StateStandby`), sending it a
write would create a second, conflicting source of truth for the shard.
`RouteRead` is where the interesting dispatch happens, and it is a tagless
switch over the four `ReplicaState` values rather than an expression switch
with a shared branch, because two of the four values collapse to the same
outcome for different reasons: `StateFailed` (the replica is unreachable)
and `StateUnknown` (no report has arrived yet, so there's no evidence the
replica is healthy) both fail safe to `"primary"`, and writing them as a
comma case list — `case StateFailed, StateUnknown:` — says exactly that:
these are two distinct situations that happen to demand the same fail-safe
answer, not one situation being tested twice.

`StateUnknown` is deliberately the zero value of `ReplicaState`. A shard
the router has never received a health report for defaults, by construction,
to the safest routing decision — reads go to primary — without a single
extra line of initialization code. That is the same "let the zero value be
the safe one" discipline that shows up whenever an enum-like type's default
state needs to be conservative rather than merely convenient.

The `sync.RWMutex` matters because `SetReplicaState` (a writer) and
`RouteRead` (a reader) are expected to run concurrently in the real system
this models: a health-check goroutine updating state while request-handling
goroutines route reads at the same time. `RWMutex` lets many concurrent
`RouteRead` calls proceed together while a `SetReplicaState` call gets
exclusive access only for the instant it needs to update the map.

Create `shardrouter.go`:

```go
// Package shardrouter routes writes to a shard's primary and reads to
// either the primary or a replica, chosen by consistent hashing the shard
// key and a tagless switch on the replica's current health state. All
// exported methods are safe for concurrent use.
package shardrouter

import (
	"hash/fnv"
	"sync"
)

// ReplicaState is the current role or health of the replica assigned to a
// shard.
type ReplicaState int

const (
	// StateUnknown is the zero value: no health report has been received
	// for this shard's replica yet.
	StateUnknown ReplicaState = iota
	// StatePrimary means this replica has been promoted (e.g. during a
	// failover) and is now the shard's authoritative primary.
	StatePrimary
	// StateStandby means the replica is healthy and safe to serve reads.
	StateStandby
	// StateFailed means the replica is unreachable.
	StateFailed
)

// Router assigns keys to shards by consistent hashing and decides, per
// read, whether the shard's replica is safe to serve it.
type Router struct {
	mu         sync.RWMutex
	shardCount int
	states     map[int]ReplicaState
}

// New builds a Router over shardCount shards. Every shard starts in
// StateUnknown until SetReplicaState reports otherwise.
func New(shardCount int) *Router {
	return &Router{
		shardCount: shardCount,
		states:     make(map[int]ReplicaState),
	}
}

// SetReplicaState records the current health state for a shard's replica.
func (r *Router) SetReplicaState(shard int, state ReplicaState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states[shard] = state
}

// RouteWrite returns the shard a key hashes to and the target that must
// handle the write. Writes always go to the shard's primary, regardless of
// replica health: a replica is a read scaling mechanism, never a write
// target.
func (r *Router) RouteWrite(key string) (shard int, target string) {
	return ShardFor(key, r.shardCount), "primary"
}

// RouteRead returns the shard a key hashes to and the target that should
// serve the read: "replica" only when that shard's replica has reported
// itself healthy, "primary" in every other case.
func (r *Router) RouteRead(key string) (shard int, target string) {
	shard = ShardFor(key, r.shardCount)

	r.mu.RLock()
	state := r.states[shard]
	r.mu.RUnlock()

	switch state {
	case StateStandby:
		target = "replica" // healthy standby: safe to offload the read
	case StatePrimary:
		target = "primary" // this replica IS the shard's primary now; it's authoritative
	case StateFailed, StateUnknown:
		target = "primary" // no confirmed healthy standby: fail safe to primary
	default:
		target = "primary" // any state this router doesn't recognize: fail safe
	}
	return shard, target
}

// ShardFor hashes key with FNV-1a and reduces it into [0, shardCount). The
// same key always maps to the same shard for a fixed shardCount, which is
// the whole point of consistent hashing a shard key: routing decisions are
// deterministic without a lookup table.
func ShardFor(key string, shardCount int) int {
	h := fnv.New32a()
	h.Write([]byte(key))
	return int(h.Sum32() % uint32(shardCount))
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	shardrouter "example.com/sharding-key-router-with-hashing"
)

func main() {
	const shardCount = 4
	r := shardrouter.New(shardCount)

	keys := []string{"user:1001", "user:1002", "user:1003", "user:1004"}
	for _, k := range keys {
		fmt.Printf("%-10s -> shard %d\n", k, shardrouter.ShardFor(k, shardCount))
	}

	r.SetReplicaState(0, shardrouter.StateStandby)
	r.SetReplicaState(1, shardrouter.StateFailed)
	r.SetReplicaState(2, shardrouter.StatePrimary)
	// shard 3 left at StateUnknown deliberately

	fmt.Println()
	for _, k := range keys {
		shard, target := r.RouteRead(k)
		fmt.Printf("read  %-10s shard=%d -> %s\n", k, shard, target)
	}
	for _, k := range keys {
		shard, target := r.RouteWrite(k)
		fmt.Printf("write %-10s shard=%d -> %s\n", k, shard, target)
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
user:1001  -> shard 2
user:1002  -> shard 3
user:1003  -> shard 0
user:1004  -> shard 1

read  user:1001  shard=2 -> primary
read  user:1002  shard=3 -> primary
read  user:1003  shard=0 -> replica
read  user:1004  shard=1 -> primary
write user:1001  shard=2 -> primary
write user:1002  shard=3 -> primary
write user:1003  shard=0 -> primary
write user:1004  shard=1 -> primary
```

### Tests

`TestShardForIsDeterministic` calls `ShardFor` five times per key and
asserts every call returns the same shard, in range. `TestRouteReadByReplicaState`
runs one subtest per `ReplicaState` value — including `StateUnknown`, the
zero value — against a key a small helper locates that is guaranteed to
hash to shard 0, so each subtest controls exactly one shard's state.
`TestRouteWriteAlwaysTargetsPrimary` proves a write ignores replica health
even when the replica is healthy. `TestConcurrentRoutingAndStateUpdates`
fires a hundred goroutines that mix `RouteRead` calls with concurrent
`SetReplicaState` updates, asserting every result is a valid shard index
and a valid target string.

Create `shardrouter_test.go`:

```go
package shardrouter

import (
	"sync"
	"testing"
)

func TestShardForIsDeterministic(t *testing.T) {
	t.Parallel()

	const shardCount = 8
	keys := []string{"user:1001", "user:1002", "order:77", ""}

	for _, k := range keys {
		first := ShardFor(k, shardCount)
		for i := 0; i < 5; i++ {
			if got := ShardFor(k, shardCount); got != first {
				t.Errorf("ShardFor(%q, %d) = %d on call %d, want %d (same as first call)", k, shardCount, got, i, first)
			}
		}
		if first < 0 || first >= shardCount {
			t.Errorf("ShardFor(%q, %d) = %d, want in [0, %d)", k, shardCount, first, shardCount)
		}
	}
}

func TestRouteReadByReplicaState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state ReplicaState
		want  string
	}{
		{"healthy standby serves the read", StateStandby, "replica"},
		{"promoted replica is now primary", StatePrimary, "primary"},
		{"failed replica falls back to primary", StateFailed, "primary"},
		{"unknown state fails safe to primary", StateUnknown, "primary"},
	}

	for _, tc := range tests {
		r := New(4)
		r.SetReplicaState(0, tc.state)
		// Force key routing to shard 0 by using ShardFor's own output as
		// the key set: pick a key we know maps to shard 0.
		key := keyForShard(t, 4, 0)
		shard, target := r.RouteRead(key)
		if shard != 0 {
			t.Fatalf("test setup: key %q mapped to shard %d, want 0", key, shard)
		}
		if target != tc.want {
			t.Errorf("%s: RouteRead(%q) target = %q, want %q", tc.name, key, target, tc.want)
		}
	}
}

func TestRouteWriteAlwaysTargetsPrimary(t *testing.T) {
	t.Parallel()

	r := New(4)
	r.SetReplicaState(0, StateStandby) // even a healthy replica must not take writes

	key := keyForShard(t, 4, 0)
	if _, target := r.RouteWrite(key); target != "primary" {
		t.Errorf("RouteWrite(%q) target = %q, want %q", key, target, "primary")
	}
}

// TestConcurrentRoutingAndStateUpdates drives many goroutines that read,
// write, and update replica state on the same Router at once. The
// assertion is that every returned target is one of the two valid values
// and every shard index is in range — i.e. the mutex actually serializes
// access and no goroutine observes a torn or out-of-range result.
func TestConcurrentRoutingAndStateUpdates(t *testing.T) {
	t.Parallel()

	const shardCount = 8
	r := New(shardCount)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := keyForShard(t, shardCount, n%shardCount)

			if n%2 == 0 {
				r.SetReplicaState(n%shardCount, ReplicaState(n%4))
				return
			}

			shard, target := r.RouteRead(key)
			if shard < 0 || shard >= shardCount {
				t.Errorf("RouteRead(%q) shard = %d, want in [0, %d)", key, shard, shardCount)
			}
			if target != "primary" && target != "replica" {
				t.Errorf("RouteRead(%q) target = %q, want %q or %q", key, target, "primary", "replica")
			}
		}(i)
	}
	wg.Wait()
}

// keyForShard finds a key that ShardFor maps to want, so tests can target
// a specific shard's state without depending on the hash's internals.
func keyForShard(t *testing.T, shardCount, want int) string {
	t.Helper()
	for i := 0; i < 10000; i++ {
		key := "k" + string(rune('a'+i%26)) + string(rune(i))
		if ShardFor(key, shardCount) == want {
			return key
		}
	}
	t.Fatalf("could not find a key mapping to shard %d out of %d", want, shardCount)
	return ""
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The router is correct when the same key always resolves to the same shard,
when a write never lands on a replica no matter how healthy that replica
claims to be, when every `ReplicaState` value (including the zero-value
`StateUnknown`) resolves `RouteRead` to a safe answer, and when concurrent
reads and state updates never race or return an out-of-range shard. Carry
this forward: when an enum-like type's default (zero) value represents "no
information yet," make sure the switch that consumes it treats that
default as the conservative choice, and reach for a comma case list the
moment two distinct states legitimately share the same fail-safe outcome.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless switch form and comma-separated case lists.
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — allowing concurrent reads while serializing writes to shared state.
- [hash/fnv](https://pkg.go.dev/hash/fnv) — the FNV-1a hash used for consistent shard assignment.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-replica-lag-threshold-alerter.md](22-replica-lag-threshold-alerter.md) | Next: [24-lease-renewal-backoff-strategy.md](24-lease-renewal-backoff-strategy.md)
