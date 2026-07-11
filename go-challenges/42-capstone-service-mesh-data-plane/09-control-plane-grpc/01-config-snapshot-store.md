# Exercise 1: Config Snapshot Store

The control plane's job begins with one data structure: a versioned, atomically swappable snapshot of every resource the data plane needs, plus a way to tell subscribers it changed. This exercise builds that store — a lock-free read path backed by `atomic.Pointer`, a monotonic version counter, and coalescing push notifications.

This module is fully self-contained: its own `go mod init`, all types defined inline, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
store.go              Listener/Cluster/Endpoint, ConfigSnapshot, Store
cmd/
  demo/
    main.go           seed config, subscribe, apply a burst, read latest
store_test.go         version stamping, coalescing, concurrent Apply
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: `Store` with `Apply(ConfigSnapshot)`, `Snapshot() *ConfigSnapshot`, `Version() uint64`, and `Subscribe(nodeID string) (<-chan struct{}, func())`.
- Test: `Apply` increments the version and stamps it into the snapshot, a burst of applies coalesces to one pending notification, cancel removes a subscriber, and concurrent applies all land.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p config-store/cmd/demo && cd config-store
go mod init example.com/config-store
go mod edit -go=1.26
```

### Why an atomic snapshot and a coalescing channel

The proxy reads its configuration on every request — the hottest path in the system — while the control plane updates that configuration occasionally and concurrently. A mutex around the config would put every request in line behind every (rare) write. The fix is to make the read path lock-free: store the whole configuration behind an `atomic.Pointer[ConfigSnapshot]`, and have `Apply` build a brand-new snapshot and swap the pointer in one indivisible store. A reader calls `Load`, gets a stable pointer, and works from it; a concurrent `Apply` swaps in a different pointer that the reader never observes mid-update. There is no torn read because the unit of publication is the entire struct, not its individual fields.

Two ordering details make the version trustworthy. `Apply` stamps `snap.Version` from `version.Add(1)` *before* it stores the pointer, so any reader that sees the new pointer also sees the matching version number — the counter and the snapshot can never disagree. And because `Add` is atomic, fifty goroutines calling `Apply` concurrently produce versions 1 through 50 with no gaps and no duplicates; the final `Version()` is exactly the number of applies.

The notification side is deliberately minimal. Each subscriber gets a channel of capacity 1, and `Apply` sends on it with a `select`/`default` so a full channel is skipped rather than blocked on. That single line is the whole coalescing mechanism: if ten applies fire before the subscriber wakes, nine of the sends hit the `default` branch and are dropped, leaving exactly one pending signal. The subscriber wakes once, reads `Snapshot()`, and sees the cumulative result of all ten changes. The channel carries `struct{}{}`, not the snapshot — the signal means "go read the latest," which is why a coalesced wake-up loses nothing: the reader always pulls current state, never a queued stale copy. `Subscribe` returns a cancel function that deletes the map entry; calling it on disconnect is what keeps a long-lived control plane from leaking a channel per proxy that ever connected.

Create `store.go`:

```go
package configstore

import (
	"sync"
	"sync/atomic"
)

// Listener is a minimal xDS Listener resource: a bound address and port.
type Listener struct {
	Name    string
	Address string
	Port    uint32
}

// Cluster is a minimal xDS Cluster resource: a named upstream service.
type Cluster struct {
	Name     string
	LBPolicy string
}

// Endpoint is one host within a Cluster's load-balancing pool.
type Endpoint struct {
	ClusterName string
	Address     string
	Port        uint32
	Healthy     bool
}

// ConfigSnapshot is an immutable, version-stamped set of all resource types.
// Applying a snapshot atomically replaces the entire running configuration so a
// reader never sees a Route that references a Cluster that does not yet exist.
type ConfigSnapshot struct {
	Version   uint64
	Listeners []Listener
	Clusters  []Cluster
	Endpoints []Endpoint
}

// Store holds the current ConfigSnapshot behind an atomic pointer so reads
// never block writers, and fans out coalesced change notifications.
type Store struct {
	snapshot atomic.Pointer[ConfigSnapshot]
	version  atomic.Uint64

	subMu sync.Mutex
	subs  map[string]chan struct{}
}

// NewStore returns an empty Store at version 0.
func NewStore() *Store {
	s := &Store{subs: make(map[string]chan struct{})}
	s.snapshot.Store(&ConfigSnapshot{})
	return s
}

// Apply stamps the version, atomically swaps the snapshot, then notifies every
// subscriber with a non-blocking send. It is safe for concurrent use.
func (s *Store) Apply(snap ConfigSnapshot) {
	snap.Version = s.version.Add(1)
	s.snapshot.Store(&snap)
	s.subMu.Lock()
	for _, ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default: // channel already holds a pending signal; do not block
		}
	}
	s.subMu.Unlock()
}

// Snapshot returns the current snapshot. The pointer is stable; callers must
// not mutate the pointed-to value.
func (s *Store) Snapshot() *ConfigSnapshot { return s.snapshot.Load() }

// Version returns the current version counter.
func (s *Store) Version() uint64 { return s.version.Load() }

// Subscribe registers nodeID and returns a coalescing notification channel
// (capacity 1) plus a cancel function that removes the subscription.
func (s *Store) Subscribe(nodeID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.subMu.Lock()
	s.subs[nodeID] = ch
	s.subMu.Unlock()
	return ch, func() {
		s.subMu.Lock()
		delete(s.subs, nodeID)
		s.subMu.Unlock()
	}
}
```

### The runnable demo

The demo seeds an initial configuration, subscribes, fires three applies in a tight loop, and reads the latest snapshot after a single wake-up. The version jumps to 4 (one seed plus three) but the subscriber wakes exactly once, which is the coalescing behavior in action.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	configstore "example.com/config-store"
)

func main() {
	store := configstore.NewStore()

	store.Apply(configstore.ConfigSnapshot{
		Listeners: []configstore.Listener{{Name: "ingress", Address: "0.0.0.0", Port: 8080}},
		Clusters:  []configstore.Cluster{{Name: "backend", LBPolicy: "round_robin"}},
		Endpoints: []configstore.Endpoint{
			{ClusterName: "backend", Address: "10.0.0.1", Port: 9090, Healthy: true},
		},
	})
	snap := store.Snapshot()
	fmt.Printf("initial: version=%d endpoints=%d\n", snap.Version, len(snap.Endpoints))

	ch, cancel := store.Subscribe("proxy-1")
	defer cancel()

	// A burst of applies coalesces into a single wake-up.
	for i := 0; i < 3; i++ {
		store.Apply(configstore.ConfigSnapshot{
			Endpoints: []configstore.Endpoint{
				{ClusterName: "backend", Address: "10.0.0.1", Port: 9090, Healthy: true},
				{ClusterName: "backend", Address: "10.0.0.2", Port: 9090, Healthy: true},
			},
		})
	}

	<-ch
	latest := store.Snapshot()
	fmt.Printf("after burst: version=%d endpoints=%d\n", latest.Version, len(latest.Endpoints))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial: version=1 endpoints=1
after burst: version=4 endpoints=2
```

### Tests

The tests pin the version-stamping contract, prove that a burst coalesces to exactly one buffered signal, confirm that cancel removes a subscriber so later applies neither block nor panic, and stress the counter from fifty goroutines under `-race`.

Create `store_test.go`:

```go
package configstore

import (
	"fmt"
	"sync"
	"testing"
)

func TestStoreInitialVersion(t *testing.T) {
	t.Parallel()
	if got := NewStore().Version(); got != 0 {
		t.Fatalf("Version() = %d, want 0", got)
	}
}

func TestStoreApplyIncrementsVersion(t *testing.T) {
	t.Parallel()
	s := NewStore()
	s.Apply(ConfigSnapshot{})
	s.Apply(ConfigSnapshot{})
	if got := s.Version(); got != 2 {
		t.Fatalf("Version() = %d, want 2", got)
	}
}

func TestStoreApplyStampsSnapshotVersion(t *testing.T) {
	t.Parallel()
	s := NewStore()
	s.Apply(ConfigSnapshot{
		Listeners: []Listener{{Name: "ingress", Address: "0.0.0.0", Port: 8080}},
	})
	snap := s.Snapshot()
	if snap.Version != 1 {
		t.Fatalf("snapshot.Version = %d, want 1", snap.Version)
	}
	if len(snap.Listeners) != 1 || snap.Listeners[0].Name != "ingress" {
		t.Fatalf("snapshot.Listeners = %+v", snap.Listeners)
	}
}

func TestStoreSubscribeReceivesNotification(t *testing.T) {
	t.Parallel()
	s := NewStore()
	ch, cancel := s.Subscribe("proxy-1")
	defer cancel()
	s.Apply(ConfigSnapshot{})
	select {
	case <-ch:
	default:
		t.Fatal("Apply did not notify subscriber")
	}
}

func TestStoreCancelRemovesSubscriber(t *testing.T) {
	t.Parallel()
	s := NewStore()
	_, cancel := s.Subscribe("proxy-2")
	cancel()
	// Applies after cancel must not block or panic.
	s.Apply(ConfigSnapshot{})
	s.Apply(ConfigSnapshot{})
}

func TestStoreNotificationsCoalesce(t *testing.T) {
	t.Parallel()
	s := NewStore()
	ch, cancel := s.Subscribe("proxy-3")
	defer cancel()
	// Three rapid applies coalesce into at most one pending signal; the
	// channel of capacity 1 never blocks Apply.
	s.Apply(ConfigSnapshot{})
	s.Apply(ConfigSnapshot{})
	s.Apply(ConfigSnapshot{})
	<-ch // exactly one signal is buffered
	select {
	case <-ch:
		t.Fatal("expected coalesced single signal, got a second")
	default:
	}
}

func TestStoreConcurrentApply(t *testing.T) {
	t.Parallel()
	s := NewStore()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Apply(ConfigSnapshot{})
		}()
	}
	wg.Wait()
	if got := s.Version(); got != 50 {
		t.Fatalf("Version() after 50 concurrent applies = %d, want 50", got)
	}
}

func ExampleStore_Version() {
	s := NewStore()
	fmt.Println(s.Version())
	s.Apply(ConfigSnapshot{
		Listeners: []Listener{{Name: "ingress", Address: "0.0.0.0", Port: 8080}},
	})
	fmt.Println(s.Version())
	// Output:
	// 0
	// 1
}
```

## Review

The store is correct when reads never block writers and the version never disagrees with the snapshot. The most common mistake is stamping the version after the pointer swap instead of before, which opens a window where a reader sees the new snapshot carrying an old version number; `TestStoreApplyStampsSnapshotVersion` pins the order. The second is using an unbuffered channel or a blocking send for notifications, which deadlocks `Apply` the instant a subscriber is slow — `TestStoreNotificationsCoalesce` proves the capacity-1, non-blocking design drops extras instead of blocking, and that the subscriber still observes the latest state because it reads `Snapshot()` rather than a value carried on the channel. `TestStoreConcurrentApply` under `-race` is the proof that the atomic counter and pointer carry the whole concurrency story: no mutex guards the snapshot, yet fifty racing applies produce exactly fifty versions. Forgetting to call the cancel function leaks a channel per disconnected proxy; `TestStoreCancelRemovesSubscriber` documents that cancel is what unregisters it.

## Resources

- [`sync/atomic`: `Pointer` and `Uint64`](https://pkg.go.dev/sync/atomic) — the lock-free snapshot swap and the monotonic version counter, both Go 1.19+.
- [The Go Memory Model](https://go.dev/ref/mem) — why an atomic store publishes the whole struct and a later atomic load observes it intact.
- [Envoy: xDS configuration overview](https://www.envoyproxy.io/docs/envoy/latest/configuration/overview/configuration) — the Listener/Cluster/Endpoint hierarchy these structs model in miniature.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-ack-nack-protocol.md](02-ack-nack-protocol.md)
