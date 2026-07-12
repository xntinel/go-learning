# Exercise 11: Config Hot-Reload Watch: Per-Generation Closed-Channel Edge Trigger

**Level: Intermediate**

A backend loads config once (feature flags, sampling rates, routing tables) and
hot-reloads it on SIGHUP or a control-plane push, and dozens of components must
learn "the value changed, re-read it" without polling. A closed channel is
one-shot, so it cannot be the reusable event by itself: closing it broadcasts one
edge and can never re-arm. The correct idiom hands each reader the current
snapshot plus a `changed` channel that is closed when the next `Store` swaps in a
fresh channel for the following generation. This exercise builds that
`Watcher[T]`: one `Store` closes the outstanding channel to wake every parked
watcher at once, then installs a new channel for the next generation.

This module is self-contained: its own module, a `confwatch` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
confwatch/                   independent module: example.com/confwatch
  go.mod                     go 1.26
  confwatch.go               generic Watcher[T]: New, Load, Store, Version
  cmd/demo/main.go           runnable demo: N watchers released by one Store
  confwatch_test.go          broadcast, per-generation no-rearm, no-lost-update, monotone version, goleak
```

- Files: `confwatch.go`, `cmd/demo/main.go`, `confwatch_test.go`.
- Implement: `New[T](initial T) *Watcher[T]`, `(*Watcher[T]).Load() (T, <-chan struct{})`, `(*Watcher[T]).Store(v T)`, and `(*Watcher[T]).Version() uint64`.
- Test: one `Store` releases N parked watchers; each generation owns its own channel and the old one never re-arms; a looping watcher never loses the newest value under concurrent Stores; the version counter is monotone; no watcher goroutine leaks.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/10-signaling-with-closed-channels/11-config-reload-watch-broadcast/cmd/demo
cd go-solutions/13-goroutines-and-channels/10-signaling-with-closed-channels/11-config-reload-watch-broadcast
go get go.uber.org/goleak
```

### Why a single closed channel cannot be the reusable event

Closing a channel is the cheapest fan-out Go gives you: one `close` wakes an
unbounded number of parked receivers, no value to drain, no per-receiver
bookkeeping. That is exactly right for a *terminal* signal like shutdown. But a
config watch is *edge-triggered and repeats*: reload can happen a hundred times
over a process's life. A closed channel is one-shot and irreversible — there is
no reopen — so you cannot close-and-reuse the same channel. Worse, if you closed a
channel that other goroutines still hold a reference to and then recreated it,
those goroutines keep reading the *old* closed channel forever and spin.

The fix is a fresh channel per generation, published atomically alongside the
value. `Load` returns a generation-consistent pair `(value_g, changed_g)`:
`changed_g` is the channel that will be closed when generation `g+1` arrives.
The watcher's protocol is:

1. `v, changed := w.Load()` — grab the current snapshot and this generation's
   channel together, under the same lock, so they cannot tear.
2. Use `v`. When you need to wait for the next change, `<-changed`.
3. When `changed` fires (it was closed), loop back to step 1. The re-`Load`
   hands you the newer value and the *next* generation's still-open channel.

`Store` is the mirror image and must do two things in one critical section:
install a fresh channel for the next generation, then close the outstanding one.
Doing the swap *before* the close is what makes concurrent Stores safe. The struct
field already points at the new channel, so a racing Store closes a *different*
channel and can never double-close the same one — the panic that plain
`close(ch); ch = make(...)` would hit under concurrency. There is no lost wakeup
either: a watcher that grabbed `changed` and then gets descheduled before its
`<-changed` still wakes, because a receive on an already-closed channel is
immediately ready. The close is the signal, not a value someone must be parked to
catch.

Create `confwatch.go`:

```go
// Package confwatch broadcasts config hot-reloads to many parked watchers using
// a fresh closed channel per generation. A closed channel is one-shot, so it can
// signal one edge but never re-arm; the reusable event is a channel that Store
// swaps out and closes on every change.
package confwatch

import "sync"

// Watcher holds the current snapshot of a config value and a "changed" channel
// that is closed exactly once, the next time Store runs. Each Store advances a
// generation: it installs a fresh changed channel for the following generation
// and closes the outstanding one to wake every parked reader at once.
type Watcher[T any] struct {
	mu      sync.Mutex
	val     T
	changed chan struct{}
	version uint64
}

// New returns a Watcher seeded with initial and an open changed channel for the
// first generation.
func New[T any](initial T) *Watcher[T] {
	return &Watcher[T]{val: initial, changed: make(chan struct{})}
}

// Load returns the current value together with the channel that is closed the
// next time Store is called. The pair is generation-consistent: the returned
// channel belongs to the same generation as the returned value, so parking on it
// and then calling Load again observes a value at least one generation newer.
//
// The caller must not hold onto a channel across a wake: after the channel is
// closed, call Load again to obtain both the new value and the next generation's
// still-open channel.
func (w *Watcher[T]) Load() (value T, changed <-chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.val, w.changed
}

// Store swaps in a new value and advances the generation. It installs a fresh
// changed channel for the next generation and then closes the outstanding one,
// broadcasting to every waiter with a single close. Both steps run under the
// mutex, so concurrent Store calls serialize and no channel is ever closed twice.
func (w *Watcher[T]) Store(v T) {
	w.mu.Lock()
	defer w.mu.Unlock()
	old := w.changed
	w.val = v
	w.version++
	w.changed = make(chan struct{})
	// Swap-then-close under the lock: the field already points at the fresh
	// channel, so a racing Store closes a different channel and never double-closes.
	close(old)
}

// Version reports the generation counter. It starts at 0 and increases by one on
// every Store, so it is strictly monotone and never regresses.
func (w *Watcher[T]) Version() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.version
}
```

### The runnable demo

The demo parks eight watchers on the first generation's channel, waits until every
one has grabbed its channel, then fires a single `Store`. All eight wake from one
close, re-read the value, and confirm they observed the new snapshot. The output
is deterministic because the counters are atomics summed after a `WaitGroup`
barrier, not printed from inside the racing goroutines.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/confwatch"
)

// Config is a tiny stand-in for a hot-reloadable config snapshot.
type Config struct {
	SamplingRate int
}

func main() {
	const watchers = 8
	w := confwatch.New(Config{SamplingRate: 10})

	initial, _ := w.Load()
	fmt.Printf("initial sampling rate: %d\n", initial.SamplingRate)

	var parked sync.WaitGroup // signals each watcher has grabbed its changed channel
	var done sync.WaitGroup   // signals each watcher has observed the new generation
	parked.Add(watchers)
	done.Add(watchers)

	var released atomic.Int64 // how many watchers a single Store woke
	var sawNew atomic.Int64   // how many watchers observed the new value

	for range watchers {
		go func() {
			// Grab the current value and the changed channel for this generation.
			_, changed := w.Load()
			parked.Done()
			// Park until the next Store closes this generation's channel. Even if
			// the close happens before we reach this receive, a closed channel is
			// already-ready, so the wake is never lost.
			<-changed
			released.Add(1)
			// Re-read to pick up the value the Store installed.
			cur, _ := w.Load()
			if cur.SamplingRate == 25 {
				sawNew.Add(1)
			}
			done.Done()
		}()
	}

	parked.Wait()
	fmt.Printf("watchers parked: %d\n", watchers)

	// One Store closes the outstanding channel and wakes every parked watcher.
	w.Store(Config{SamplingRate: 25})
	done.Wait()

	fmt.Printf("one Store released all watchers: %d\n", released.Load())
	fmt.Printf("every watcher observed new value 25: %v\n", sawNew.Load() == watchers)
	fmt.Printf("version advanced 0 -> %d\n", w.Version())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial sampling rate: 10
watchers parked: 8
one Store released all watchers: 8
every watcher observed new value 25: true
version advanced 0 -> 1
```

### Tests

`TestSingleStoreBroadcastsToManyWatchers` parks 200 watchers on one generation's
channel and fires a single `Store`; asserting all 200 were released and observed
the new value pins the broadcast asymmetry (one close wakes N, a value send would
wake one). `TestPerGenerationChannelsDoNotRearm` checks that after a `Store` the
old channel is closed forever while a fresh `Load` returns the new value with a
still-open channel — the old one never re-arms. `TestStoreAdvancesVersionAndValue`
pins the monotone counter and value swap on the single-threaded path.
`TestNoLostFinalUpdateUnderConcurrentStores` is the liveness core: a looping
watcher under a storm of concurrent Stores always eventually observes the newest
value (intermediate values may be coalesced; the final one is never lost).
`TestConcurrentStoresVersionMonotone` proves 16 concurrent storers never panic and
land the version at exactly the total number of Stores — no dropped or doubled
close. `TestMain` runs goleak so any surviving watcher goroutine fails the run.

Create `confwatch_test.go`:

```go
package confwatch

import (
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak after all tests: every watcher goroutine any test starts
// must have exited, or the leak detector fails the run.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestSingleStoreBroadcastsToManyWatchers pins the broadcast asymmetry: N
// watchers all parked on the same generation's changed channel are released by a
// single Store. One close wakes all N; a value send would wake only one.
func TestSingleStoreBroadcastsToManyWatchers(t *testing.T) {
	const n = 200
	w := New(0)

	var parked, done sync.WaitGroup
	parked.Add(n)
	done.Add(n)
	var released, sawNew atomic.Int64

	for range n {
		go func() {
			_, changed := w.Load()
			parked.Done()
			<-changed
			released.Add(1)
			if v, _ := w.Load(); v == 42 {
				sawNew.Add(1)
			}
			done.Done()
		}()
	}

	parked.Wait()
	w.Store(42) // single close releases every parked watcher
	done.Wait()

	if got := released.Load(); got != n {
		t.Fatalf("released = %d, want %d (one close must wake all)", got, n)
	}
	if got := sawNew.Load(); got != n {
		t.Fatalf("observed new value = %d, want %d", got, n)
	}
}

// TestPerGenerationChannelsDoNotRearm pins that each generation owns its own
// channel: after Store, the previous channel is closed forever and a fresh Load
// hands back the new value with a still-open channel. The old channel is never
// re-armed.
func TestPerGenerationChannelsDoNotRearm(t *testing.T) {
	w := New("v0")

	v0, ch0 := w.Load()
	if v0 != "v0" {
		t.Fatalf("initial value = %q, want v0", v0)
	}
	select {
	case <-ch0:
		t.Fatal("gen-0 channel closed before any Store")
	default:
	}

	w.Store("v1")

	// The gen-0 channel must now be closed (receive returns immediately).
	select {
	case <-ch0:
	default:
		t.Fatal("gen-0 channel not closed after Store")
	}

	// A fresh Load sees the new value and a still-open gen-1 channel.
	v1, ch1 := w.Load()
	if v1 != "v1" {
		t.Fatalf("value after Store = %q, want v1", v1)
	}
	select {
	case <-ch1:
		t.Fatal("gen-1 channel closed with no second Store")
	default:
	}

	// The old channel stays closed; there is no re-arm.
	select {
	case <-ch0:
	default:
		t.Fatal("gen-0 channel re-armed")
	}
}

// TestStoreAdvancesVersionAndValue pins the monotone version counter and the
// value swap on the single-threaded path.
func TestStoreAdvancesVersionAndValue(t *testing.T) {
	w := New(100)
	if v := w.Version(); v != 0 {
		t.Fatalf("initial version = %d, want 0", v)
	}
	for i := range 5 {
		w.Store(i)
		if got, want := w.Version(), uint64(i+1); got != want {
			t.Fatalf("version after store %d = %d, want %d", i, got, want)
		}
		if v, _ := w.Load(); v != i {
			t.Fatalf("value after store %d = %d, want %d", i, v, i)
		}
	}
}

// TestNoLostFinalUpdateUnderConcurrentStores pins the core liveness property: a
// watcher that loops Load, park, Load always eventually observes the newest value
// under a storm of concurrent Stores. Intermediate values may be coalesced, but
// the final Store is never lost, because Load returns a generation-consistent
// (value, channel) pair and every Store closes the outstanding channel.
func TestNoLostFinalUpdateUnderConcurrentStores(t *testing.T) {
	const (
		watchers  = 25
		producers = 8
		perProd   = 400
		target    = 1_000_000
	)
	w := New(0)

	var watchWG sync.WaitGroup
	for range watchers {
		watchWG.Go(func() {
			for {
				v, changed := w.Load()
				if v == target {
					return
				}
				<-changed // never blocks forever: the next Store closes this channel
			}
		})
	}

	var prodWG sync.WaitGroup
	for range producers {
		prodWG.Go(func() {
			for i := range perProd {
				w.Store(i + 1) // storm of intermediate values, none equal target
			}
		})
	}
	prodWG.Wait()

	w.Store(target) // the newest value; every watcher must observe it and exit

	// If any watcher had parked on a channel that never closes, this hangs and the
	// test times out. Returning proves no final update was lost.
	watchWG.Wait()

	if v, _ := w.Load(); v != target {
		t.Fatalf("final value = %d, want %d", v, target)
	}
}

// TestConcurrentStoresVersionMonotone pins that concurrent Stores never panic
// (swap-then-close under the mutex) and that the version counter equals the exact
// number of Stores, proving no close was dropped or doubled.
func TestConcurrentStoresVersionMonotone(t *testing.T) {
	const (
		storers  = 16
		perStore = 500
	)
	w := New(0)

	var wg sync.WaitGroup
	for g := range storers {
		wg.Go(func() {
			for i := range perStore {
				w.Store(g*perStore + i)
			}
		})
	}
	wg.Wait()

	if got, want := w.Version(), uint64(storers*perStore); got != want {
		t.Fatalf("version = %d, want %d (each Store must advance exactly once)", got, want)
	}
}
```

## Review

`Watcher[T]` is correct when three invariants hold together. First, broadcast: one
`Store` closes a single outstanding channel and every parked watcher wakes,
proven by `TestSingleStoreBroadcastsToManyWatchers` releasing all 200. Second,
per-generation ownership: the value and the channel are published as one
generation-consistent pair under the mutex, and the swap-before-close means the
old channel is closed exactly once and never re-armed, proven by
`TestPerGenerationChannelsDoNotRearm` and by
`TestConcurrentStoresVersionMonotone` landing the counter at the exact store count
with no panic. Third, no lost update: because each `Load` returns the newest
`(value, channel)` and every `Store` closes the outstanding channel, a looping
watcher can coalesce intermediate values but always re-reads the latest after a
wake, proven by `TestNoLostFinalUpdateUnderConcurrentStores` terminating. The
production bug this prevents is the classic reload-notification design that closes
and recreates one shared channel: concurrent reloaders double-close and panic, or
watchers spin on a stale closed channel and never see the new config. A fresh
channel per generation, swapped then closed under a lock, is the safe reusable
edge trigger — and goleak confirms no watcher goroutine outlives the final Store.

## Resources

- [The Go Memory Model](https://go.dev/ref/mem) -- why the mutex around swap-then-close gives every watcher a happens-before edge to the new value.
- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) -- the critical section that serializes concurrent Stores so no channel is closed twice.
- [Go blog: Go Concurrency Patterns -- pipelines and cancellation](https://go.dev/blog/pipelines) -- closing a channel as a broadcast to many receivers.
- [pkg.go.dev: go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- the leak detector that turns "no watcher outlives shutdown" into a test assertion.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-admission-drain-gate-load-shedding.md](10-admission-drain-gate-load-shedding.md) | Next: [12-singleflight-stampede-completion-barrier.md](12-singleflight-stampede-completion-barrier.md)
