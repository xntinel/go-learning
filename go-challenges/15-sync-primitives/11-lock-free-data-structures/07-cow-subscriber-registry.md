# Exercise 7: Copy-on-Write Subscriber Registry (RCU-Style Reads)

A webhook fan-out registry is read on every event and mutated a few times an
hour. That read/write asymmetry is the textbook case for copy-on-write
publication: readers take one atomic pointer load and iterate an immutable
snapshot — wait-free — while writers clone, modify, and CAS the new slice in.
This exercise builds the registry, proves no reader can ever see a torn slice,
and benchmarks the read path against an `RWMutex` twin.

## What you'll build

```text
cowregistry/                     independent module: example.com/cowregistry
  go.mod
  registry.go                    Event, Subscriber; Registry: Subscribe,
                                 Unsubscribe, Notify, Len (atomic.Pointer + CAS loop)
  mutex_registry.go              RWMutexRegistry: the baseline for the benchmark
  registry_test.go               churn under -race, lost-update CAS proof,
                                 snapshot-integrity readers, read benchmarks, Example
  cmd/
    demo/
      main.go                    subscribe two webhooks, notify, unsubscribe, notify
```

- Files: `registry.go`, `mutex_registry.go`, `registry_test.go`, `cmd/demo/main.go`.
- Implement: `Registry` holding `atomic.Pointer[[]Subscriber]`; `Subscribe`/`Unsubscribe` as clone-modify-CAS loops using `slices.Clone` and `slices.DeleteFunc`; wait-free `Notify`.
- Test: continuous `Notify` while writers churn under `-race`; N concurrent `Subscribe` calls all land (the CAS retry proves itself); no reader ever observes nil or a half-built slice; `BenchmarkNotifyCOW` vs `BenchmarkNotifyRWMutex`.
- Verify: `go test -count=1 -race ./...` then `go test -bench=Notify ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/11-lock-free-data-structures/07-cow-subscriber-registry/cmd/demo
cd go-solutions/15-sync-primitives/11-lock-free-data-structures/07-cow-subscriber-registry
```

### Publish immutable snapshots, never edit shared state

The registry's single mutable cell is `atomic.Pointer[[]Subscriber]`. The slice
it points to is a *snapshot*: complete when published, never modified after.
`Notify` loads the pointer once and iterates — no lock, no loop, no allocation.
A writer that runs mid-iteration swaps the pointer to a *different* slice; the
reader keeps its consistent old snapshot to the end. That is the RCU
(read-copy-update) discipline from OS kernels, expressed with Go primitives, and
it is the same shape as `atomic.Value` config hot-reload — generalized with a
CAS loop because this registry has *concurrent writers*.

Why a CAS loop instead of `Store`? Two concurrent `Subscribe` calls that both
`Load` the same 10-element snapshot would each build an 11-element slice; with
`Store`, the second publication silently discards the first subscriber — a lost
update you would debug in production as "the webhook was registered but never
fired". With `CompareAndSwap`, the second writer's CAS fails (the pointer moved),
and its loop re-clones from the 11-element snapshot. The lost-update test pins
exactly this: 100 concurrent subscribes, 100 registered survivors.

The cloning rule has a sharp edge worth spelling out: `append` on the *loaded*
slice may write into spare capacity of the shared backing array that a reader is
iterating — an in-place mutation of published state, and a data race even though
`append` "returns a new slice". The writer must `slices.Clone` first (a fresh
backing array) and only then modify. `slices.DeleteFunc` likewise mutates the
backing array it is handed, so it too operates on the clone, never the loaded
snapshot.

Cost model, honestly: reads are O(1) pointer loads plus iteration and are
wait-free; writes are O(n) copy plus allocation, and under writer contention the
CAS loop redoes the copy. If your workload mutates constantly or n is huge, COW
is the wrong tool and the `RWMutex` twin (also in this module, as the benchmark
baseline) is the sane default. The read-path benchmark shows what you buy: an
uncontended atomic load versus `RLock`/`RUnlock` bookkeeping on every event, and
no reader/writer convoy when a writer does show up.

Create `registry.go`:

```go
package cowregistry

import (
	"slices"
	"sync/atomic"
)

// Event is what gets fanned out to subscribers.
type Event struct {
	Name    string
	Payload string
}

// Subscriber is a registered webhook target. The Fn field stands in
// for the HTTP delivery the real system would do.
type Subscriber struct {
	ID string
	Fn func(Event)
}

// Registry is a copy-on-write subscriber set: wait-free reads, CAS
// writers. Must not be copied after first use.
type Registry struct {
	subs atomic.Pointer[[]Subscriber]
}

// New returns an empty registry with a valid (non-nil) snapshot, so
// readers never need a nil check.
func New() *Registry {
	r := &Registry{}
	empty := []Subscriber{}
	r.subs.Store(&empty)
	return r
}

// Subscribe registers s. Clone-modify-CAS: concurrent subscribers
// retry instead of overwriting each other.
func (r *Registry) Subscribe(s Subscriber) {
	for {
		old := r.subs.Load()
		next := append(slices.Clone(*old), s)
		if r.subs.CompareAndSwap(old, &next) {
			return
		}
	}
}

// Unsubscribe removes the subscriber with the given id, reporting
// whether it was present.
func (r *Registry) Unsubscribe(id string) bool {
	for {
		old := r.subs.Load()
		next := slices.DeleteFunc(slices.Clone(*old), func(s Subscriber) bool {
			return s.ID == id
		})
		if len(next) == len(*old) {
			return false
		}
		if r.subs.CompareAndSwap(old, &next) {
			return true
		}
	}
}

// Notify delivers e to every subscriber in the current snapshot and
// returns how many were notified. Wait-free: one atomic load.
func (r *Registry) Notify(e Event) int {
	subs := *r.subs.Load()
	for _, s := range subs {
		s.Fn(e)
	}
	return len(subs)
}

// Len reports the current number of subscribers.
func (r *Registry) Len() int {
	return len(*r.subs.Load())
}
```

Create `mutex_registry.go` — the baseline the benchmark compares against:

```go
package cowregistry

import (
	"slices"
	"sync"
)

// RWMutexRegistry is the boring twin: identical contract, RWMutex
// around a plain slice. The benchmark's control group.
type RWMutexRegistry struct {
	mu   sync.RWMutex
	subs []Subscriber
}

func NewRWMutex() *RWMutexRegistry {
	return &RWMutexRegistry{}
}

func (r *RWMutexRegistry) Subscribe(s Subscriber) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs = append(r.subs, s)
}

func (r *RWMutexRegistry) Unsubscribe(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.subs)
	r.subs = slices.DeleteFunc(r.subs, func(s Subscriber) bool {
		return s.ID == id
	})
	return len(r.subs) != n
}

func (r *RWMutexRegistry) Notify(e Event) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.subs {
		s.Fn(e)
	}
	return len(r.subs)
}
```

### Tests

`TestConcurrentSubscribesAllLand` is the CAS-retry proof: 100 goroutines
subscribe concurrently and all 100 must be present — replace the CAS with a
`Store` and this test fails with lost subscribers. `TestChurn` runs continuous
`Notify` readers against subscribe/unsubscribe writers; its assertions are
modest (every snapshot internally consistent, subscriber count within bounds)
because its real teeth are `-race`: any in-place mutation of a published slice
is reported by the detector, not by luck. `TestSnapshotNeverNilOrTorn` pins the
constructor's non-nil guarantee and that a snapshot loaded mid-churn never
contains a zero-value subscriber (which is what a half-built slice would leak).

Create `registry_test.go`:

```go
package cowregistry

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSubscribeNotifyUnsubscribe(t *testing.T) {
	t.Parallel()

	r := New()
	var got atomic.Int64
	r.Subscribe(Subscriber{ID: "a", Fn: func(Event) { got.Add(1) }})
	r.Subscribe(Subscriber{ID: "b", Fn: func(Event) { got.Add(1) }})

	if n := r.Notify(Event{Name: "deploy"}); n != 2 {
		t.Fatalf("Notify = %d subscribers, want 2", n)
	}
	if got.Load() != 2 {
		t.Fatalf("deliveries = %d, want 2", got.Load())
	}

	if !r.Unsubscribe("a") {
		t.Fatal("Unsubscribe(a) = false, want true")
	}
	if r.Unsubscribe("a") {
		t.Fatal("second Unsubscribe(a) = true, want false")
	}
	if n := r.Notify(Event{Name: "deploy"}); n != 1 {
		t.Fatalf("Notify after unsubscribe = %d, want 1", n)
	}
}

func TestConcurrentSubscribesAllLand(t *testing.T) {
	t.Parallel()

	const n = 100
	r := New()
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Subscribe(Subscriber{
				ID: fmt.Sprintf("sub-%d", i),
				Fn: func(Event) {},
			})
		}()
	}
	wg.Wait()

	if got := r.Len(); got != n {
		t.Fatalf("Len = %d, want %d (a CAS lost an update)", got, n)
	}
}

func TestChurn(t *testing.T) {
	t.Parallel()

	r := New()
	r.Subscribe(Subscriber{ID: "keep", Fn: func(Event) {}})

	stop := make(chan struct{})
	var readers, writers sync.WaitGroup

	// Readers: continuous notify until told to stop.
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if n := r.Notify(Event{Name: "tick"}); n < 1 {
						t.Error("snapshot lost the permanent subscriber")
						return
					}
				}
			}
		}()
	}

	// Writers: subscribe/unsubscribe churn.
	for w := range 2 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := range 500 {
				id := fmt.Sprintf("churn-%d-%d", w, i)
				r.Subscribe(Subscriber{ID: id, Fn: func(Event) {}})
				r.Unsubscribe(id)
			}
		}()
	}

	writers.Wait()
	close(stop)
	readers.Wait()

	if got := r.Len(); got != 1 {
		t.Fatalf("Len after churn = %d, want 1", got)
	}
}

func TestSnapshotNeverNilOrTorn(t *testing.T) {
	t.Parallel()

	const n = 300
	r := New()
	stop := make(chan struct{})
	var writer, reader sync.WaitGroup

	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := range n {
			r.Subscribe(Subscriber{ID: fmt.Sprintf("s%d", i), Fn: func(Event) {}})
		}
	}()

	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-stop:
				return
			default:
				snap := r.subs.Load()
				if snap == nil {
					t.Error("reader observed nil snapshot")
					return
				}
				for _, s := range *snap {
					if s.ID == "" || s.Fn == nil {
						t.Error("reader observed a torn subscriber")
						return
					}
				}
			}
		}
	}()

	writer.Wait()
	close(stop)
	reader.Wait()

	if got := r.Len(); got != n {
		t.Fatalf("Len = %d, want %d", got, n)
	}
}

func BenchmarkNotifyCOW(b *testing.B) {
	r := New()
	for i := range 16 {
		r.Subscribe(Subscriber{ID: fmt.Sprintf("s%d", i), Fn: func(Event) {}})
	}
	e := Event{Name: "bench"}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.Notify(e)
		}
	})
}

func BenchmarkNotifyRWMutex(b *testing.B) {
	r := NewRWMutex()
	for i := range 16 {
		r.Subscribe(Subscriber{ID: fmt.Sprintf("s%d", i), Fn: func(Event) {}})
	}
	e := Event{Name: "bench"}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.Notify(e)
		}
	})
}

func ExampleRegistry() {
	r := New()
	r.Subscribe(Subscriber{ID: "audit", Fn: func(e Event) {
		fmt.Println("audit received:", e.Name)
	}})
	n := r.Notify(Event{Name: "user.created"})
	fmt.Println("delivered to", n)
	// Output:
	// audit received: user.created
	// delivered to 1
}
```

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cowregistry"
)

func main() {
	r := cowregistry.New()
	r.Subscribe(cowregistry.Subscriber{ID: "billing", Fn: func(e cowregistry.Event) {
		fmt.Printf("billing <- %s\n", e.Name)
	}})
	r.Subscribe(cowregistry.Subscriber{ID: "audit", Fn: func(e cowregistry.Event) {
		fmt.Printf("audit   <- %s\n", e.Name)
	}})

	fmt.Printf("notified %d subscribers\n", r.Notify(cowregistry.Event{Name: "invoice.paid"}))

	r.Unsubscribe("billing")
	fmt.Printf("notified %d subscribers\n", r.Notify(cowregistry.Event{Name: "invoice.paid"}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
billing <- invoice.paid
audit   <- invoice.paid
notified 2 subscribers
audit   <- invoice.paid
notified 1 subscribers
```

## Review

Three review points decide correctness. First, no code path may modify a slice
reachable from a published pointer — `slices.Clone` before `append` or
`DeleteFunc`, always; the churn test under `-race` is the enforcement. Second,
writers must CAS, not `Store`: the lost-update test with 100 concurrent
subscribes is exactly the production bug a `Store` version ships. Third, the
constructor publishes a valid empty snapshot so `Notify` needs no nil branch.
Know the limits too: a subscriber unsubscribed mid-`Notify` may still receive
the event currently being fanned out from the old snapshot — COW gives snapshot
consistency, not immediate revocation; if delivery-after-unsubscribe is a
correctness problem (not just a courtesy), you need per-subscriber
cancellation, not a different registry. And if the benchmark shows RWMutex
within noise on your workload, ship the RWMutex.

## Resources

- [Read-copy-update](https://en.wikipedia.org/wiki/Read-copy-update) — the kernel pattern this registry reimplements with GC instead of grace periods.
- [slices.Clone and slices.DeleteFunc](https://pkg.go.dev/slices) — what they mutate and what they return; the reason Clone precedes DeleteFunc here.
- [sync/atomic: Pointer](https://pkg.go.dev/sync/atomic#Pointer) — the publication primitive.
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — the baseline the benchmark measures against.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-atomic-circuit-breaker.md](06-atomic-circuit-breaker.md) | Next: [08-spsc-ring-buffer-telemetry.md](08-spsc-ring-buffer-telemetry.md)
