# Exercise 5: Labeled counter registry with best-effort scrape via Range

A metrics registry behind a `/metrics` handler is the textbook `sync.Map` case:
labels (the keys) are effectively stable — a bounded set of counters created once
and incremented forever — reads on the increment path must not contend, and the
scrape walks every counter. This module builds a Prometheus-style labeled counter
registry and, crucially, shows why `Range` is the right scrape primitive even
though it is not a consistent snapshot, and why copying the values out is what
makes the exported view usable.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
metricsreg/                   independent module: example.com/metricsreg
  go.mod                      go 1.26
  registry.go                 type Registry; Inc, Add, Scrape
  cmd/
    demo/
      main.go                 runnable demo: increment labels, scrape a snapshot
  registry_test.go            per-label exactness under concurrency, scrape-during-writes, Example
```

- Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: `Registry` with `Inc(label string)`, `Add(label string, delta int64)`, `Scrape() map[string]int64`.
- Test: concurrent `Inc`/`Add` across many labels and goroutines with exact per-label totals; `Scrape` during concurrent writes never panics and returns a self-consistent map.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir go-solutions/15-sync-primitives/04-sync-map/05-metrics-registry && cd go-solutions/15-sync-primitives/04-sync-map/05-metrics-registry
```

### Why LoadOrStore per label and Range to scrape

Each label maps to a `*atomic.Int64`, created on first touch with
`LoadOrStore(label, &atomic.Int64{})` and then incremented through the shared
atomic — the same lock-free typed-pointer idiom as the visit counter, applied to
an unbounded-label registry. `Inc` is `Add(label, 1)`; `Add` fetches-or-creates the
atomic and calls `Add(delta)` on it. The increment path never locks and never
allocates after the label's first use.

`Scrape` is where the `Range` semantics matter. `Range` is *not* a consistent
snapshot: while it walks, other goroutines are still incrementing counters and may
even create new labels, and `Range` may or may not visit those. That sounds
disqualifying but it is exactly right for a scrape — a `/metrics` endpoint is a
best-effort point-in-time read, and the monitoring system expects eventual
consistency, not a stop-the-world barrier on every request. What you must not do is
export the live `*atomic.Int64` pointers; you copy each counter's *current value*
into a fresh `map[string]int64` as you range. That copy-out is what makes the
result usable: the returned map is a plain, immutable snapshot the handler can
serialize without holding any lock, and because each value came from a single
`atomic.Load`, every individual number in it is a valid reading of that counter at
some instant during the scrape — even though the *set* of labels is only
eventually consistent. Best-effort on the set, exact on each value: that is the
honest contract, and it is the correct one for observability.

Create `registry.go`:

```go
package metricsreg

import (
	"sync"
	"sync/atomic"
)

// Registry is a concurrency-safe set of labeled int64 counters, suited to a
// /metrics endpoint. Labels are the stable-keys pattern: a bounded set created
// once and incremented under heavy concurrency. Each label maps to a shared
// *atomic.Int64, so the increment path is lock-free.
type Registry struct {
	counters sync.Map // map[string]*atomic.Int64
}

// NewRegistry returns an empty registry ready for concurrent use.
func NewRegistry() *Registry {
	return &Registry{}
}

// Add increases the counter for label by delta, creating it on first use, and
// returns the new value.
func (r *Registry) Add(label string, delta int64) int64 {
	actual, _ := r.counters.LoadOrStore(label, &atomic.Int64{})
	return actual.(*atomic.Int64).Add(delta)
}

// Inc increases the counter for label by one.
func (r *Registry) Inc(label string) int64 {
	return r.Add(label, 1)
}

// Scrape returns a point-in-time snapshot of all counters as a plain map. It is
// a best-effort read: labels created concurrently may or may not appear, but
// each returned value is a valid atomic reading of its counter.
func (r *Registry) Scrape() map[string]int64 {
	out := make(map[string]int64)
	r.counters.Range(func(key, value any) bool {
		out[key.(string)] = value.(*atomic.Int64).Load()
		return true
	})
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/metricsreg"
)

func main() {
	reg := metricsreg.NewRegistry()
	reg.Inc("http_requests_total")
	reg.Inc("http_requests_total")
	reg.Inc("http_requests_total")
	reg.Add("bytes_written_total", 2048)
	reg.Inc("errors_total")

	snap := reg.Scrape()
	labels := make([]string, 0, len(snap))
	for k := range snap {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	for _, k := range labels {
		fmt.Printf("%s %d\n", k, snap[k])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
bytes_written_total 2048
errors_total 1
http_requests_total 3
```

### Tests

`TestExactUnderConcurrency` increments a set of labels from many goroutines and,
after `wg.Wait`, asserts each label's total is exactly right — proving no increment
is lost. `TestScrapeDuringWritesIsConsistent` runs `Scrape` repeatedly while
writers hammer the counters; it must never panic, and every value it returns must
be within the range the counter legitimately passed through (monotonic, non-
negative, never above the final total). That pins the "best-effort set, exact
value" contract without asserting a stop-the-world snapshot.

Create `registry_test.go`:

```go
package metricsreg

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
)

func TestExactUnderConcurrency(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	labels := []string{"a", "b", "c", "d"}
	const goroutines = 8
	const perLabel = 1000

	var wg sync.WaitGroup
	for _, label := range labels {
		for range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range perLabel {
					reg.Inc(label)
				}
			}()
		}
	}
	wg.Wait()

	snap := reg.Scrape()
	want := int64(goroutines * perLabel)
	for _, label := range labels {
		if snap[label] != want {
			t.Errorf("counter %q = %d, want %d", label, snap[label], want)
		}
	}
}

func TestScrapeDuringWritesIsConsistent(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	const perLabel = 5000
	labels := []string{"x", "y", "z"}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, label := range labels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perLabel {
				reg.Inc(label)
			}
		}()
	}
	// Close stop once every writer is done, so the scraper can exit.
	go func() {
		wg.Wait()
		close(stop)
	}()

	// Scrape concurrently with the writers: must not panic; each value must be
	// a legal reading in [0, perLabel].
	var scrapes atomic.Int64
	for {
		snap := reg.Scrape()
		for label, v := range snap {
			if v < 0 || v > perLabel {
				t.Errorf("scrape saw %q = %d, out of legal range [0,%d]", label, v, perLabel)
			}
		}
		scrapes.Add(1)
		select {
		case <-stop:
			// One last scrape happened above; verify final totals below.
			final := reg.Scrape()
			for _, label := range labels {
				if final[label] != perLabel {
					t.Errorf("final %q = %d, want %d", label, final[label], perLabel)
				}
			}
			if scrapes.Load() == 0 {
				t.Fatal("scraper never ran")
			}
			return
		default:
		}
	}
}

func ExampleRegistry() {
	reg := NewRegistry()
	reg.Inc("hits")
	reg.Inc("hits")
	reg.Add("bytes", 512)

	snap := reg.Scrape()
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%d\n", k, snap[k])
	}
	// Output:
	// bytes=512
	// hits=2
}
```

## Review

The registry is correct when the increment path is a lock-free atomic add on a
per-label pointer, and `Scrape` copies values out rather than exporting live
pointers. `TestExactUnderConcurrency` proves no increment is lost; the scrape-
during-writes test proves `Scrape` never panics and returns only legal readings
even as counters move under it. The design point to internalize is that `Range`'s
lack of a consistent snapshot is a feature here, not a defect: a metrics scrape is
inherently best-effort and must not block the write path, and the copy-out gives
each value the exact-at-some-instant guarantee that is all a monitoring system
needs. The trap to avoid is returning the `*atomic.Int64` pointers from `Scrape` —
the caller would then read values that keep moving and could not serialize a stable
page. Run `go test -race` to confirm concurrent `Inc` and `Scrape` are clean.

## Resources

- [sync.Map.Range](https://pkg.go.dev/sync#Map.Range) — best-effort iteration, not a consistent snapshot.
- [sync/atomic](https://pkg.go.dev/sync/atomic) — `atomic.Int64` `Add`/`Load` for the lock-free counters.
- [Prometheus Go client: counters](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus#Counter) — how a real labeled-counter registry is shaped.

---

Back to [04-connection-registry.md](04-connection-registry.md) | Next: [06-session-store-expiry.md](06-session-store-expiry.md)
