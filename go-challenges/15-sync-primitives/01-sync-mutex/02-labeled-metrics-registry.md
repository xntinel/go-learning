# Exercise 2: Concurrent labeled counter registry

Behind every `/metrics` endpoint is a map of labeled counters — `requests{route=/home}`,
`errors{code=500}` — written from every request goroutine and read by the
scrape. This module builds that registry as a mutex-guarded
`map[string]int64`, and it demonstrates the one method that scraping actually
needs: `Snapshot`, which copies the map under the lock so the scraper can iterate
without racing the writers.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
metricsreg/                  independent module: example.com/metricsreg
  go.mod                     go 1.26
  registry.go                type Registry; Add, Get, Len, Snapshot
  cmd/
    demo/
      main.go                runnable demo: add labeled counts, snapshot, print
  registry_test.go           concurrent-writers total test, missing-key comma-ok, snapshot-is-copy
```

- Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: a `Registry` (mutex + `map[string]int64`) with `Add(label, delta)`, `Get(label) (int64, bool)`, `Len() int`, and `Snapshot() map[string]int64`.
- Test: concurrent writers to distinct and shared labels assert per-label totals and `Len`; `Get(missing)` returns `ok==false`; `Snapshot` returns an independent copy.
- Verify: `go test -count=1 -race ./...`

### Why a bare map needs a lock, and why Snapshot copies

A Go map is not safe for concurrent use: concurrent writes, or a concurrent read
and write, can corrupt its internal structure, and the runtime deliberately
crashes with `fatal error: concurrent map writes` when it detects it. So every
touch of the map — `Add`, `Get`, `Len` — happens under one mutex. `Add` uses the
`m[label] += delta` form, which reads and writes the entry as one operation
inside a single critical section, so two goroutines incrementing the same label
cannot lose an update.

`Get` returns the comma-ok pair: `(0, false)` for a label never seen. That is
honest presence reporting — an unseen label is not a zero counter, it is absent,
and the caller can tell the difference. This is a real distinction for a
dashboard that wants to show "no data" rather than "0".

`Snapshot` is the method the scrape uses, and it exists because you must never
hand a caller the live map to range over. If the scraper iterated the real map
while a request goroutine wrote to it, that is a concurrent read/write and the
runtime crashes. Instead `Snapshot` copies the map under the lock with
`maps.Clone` and returns the copy; the caller ranges over its own private map
with no lock at all. This is copy-under-lock: the only work done while holding
the mutex is the copy, and all iteration and formatting happen afterward,
outside the critical section.

Create `registry.go`:

```go
package metricsreg

import (
	"maps"
	"sync"
)

// Registry is a concurrency-safe set of labeled int64 counters, the store
// behind a /metrics endpoint. Its zero value is not ready; use New.
type Registry struct {
	mu     sync.Mutex
	counts map[string]int64
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{counts: make(map[string]int64)}
}

// Add increments the counter for label by delta (delta may be negative).
func (r *Registry) Add(label string, delta int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[label] += delta
}

// Get returns the counter for label and whether it exists. An unseen label
// reports (0, false), distinguishing "absent" from "zero".
func (r *Registry) Get(label string) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.counts[label]
	return v, ok
}

// Len reports the number of distinct labels.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.counts)
}

// Snapshot returns an independent copy of all counters, safe to iterate without
// the lock. Callers must range over the returned map, never the live one.
func (r *Registry) Snapshot() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.counts)
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
	r := metricsreg.New()
	r.Add("requests{route=/home}", 3)
	r.Add("requests{route=/home}", 2)
	r.Add("requests{route=/checkout}", 1)

	snap := r.Snapshot()
	labels := make([]string, 0, len(snap))
	for label := range snap {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		fmt.Printf("%s = %d\n", label, snap[label])
	}
	fmt.Printf("labels=%d\n", r.Len())

	if _, ok := r.Get("requests{route=/missing}"); !ok {
		fmt.Println("missing label: absent")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
requests{route=/checkout} = 1
requests{route=/home} = 5
labels=2
missing label: absent
```

### Tests

`TestConcurrentTotals` fans out writers over a shared label and several distinct
labels; because `+=` under the lock cannot lose an update, the shared label's
total is exactly `writers*perWriter` and `Len` counts every distinct label.
`TestGetMissing` pins the honest-presence contract: an unseen label reports
`ok==false`. `TestSnapshotIsIndependentCopy` mutates the returned map and adds
more to the registry, then proves neither change bled into the other — the copy
is genuinely detached.

Create `registry_test.go`:

```go
package metricsreg

import (
	"fmt"
	"sync"
	"testing"
)

func TestConcurrentTotals(t *testing.T) {
	t.Parallel()

	r := New()
	const writers, perWriter = 100, 200

	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func() {
			defer wg.Done()
			for range perWriter {
				r.Add("shared", 1)
			}
			r.Add(fmt.Sprintf("worker-%d", i), 1)
		}()
	}
	wg.Wait()

	if got, want := mustGet(t, r, "shared"), int64(writers*perWriter); got != want {
		t.Fatalf("shared = %d, want %d", got, want)
	}
	if got, want := r.Len(), writers+1; got != want {
		t.Fatalf("Len() = %d, want %d", got, want)
	}
}

func TestGetMissing(t *testing.T) {
	t.Parallel()

	r := New()
	r.Add("present", 1)

	if _, ok := r.Get("absent"); ok {
		t.Fatal("Get(absent) reported ok=true; want false")
	}
	if v, ok := r.Get("present"); !ok || v != 1 {
		t.Fatalf("Get(present) = %d,%v; want 1,true", v, ok)
	}
}

func TestSnapshotIsIndependentCopy(t *testing.T) {
	t.Parallel()

	r := New()
	r.Add("a", 1)

	snap := r.Snapshot()
	snap["a"] = 999 // mutate the copy
	snap["b"] = 999 // add to the copy
	r.Add("a", 10)  // mutate the registry after snapshot

	if v, _ := r.Get("a"); v != 11 {
		t.Fatalf("registry a = %d, want 11 (snapshot mutation leaked in)", v)
	}
	if _, ok := r.Get("b"); ok {
		t.Fatal("registry gained key b from a snapshot mutation")
	}
}

func mustGet(t *testing.T, r *Registry, label string) int64 {
	t.Helper()
	v, ok := r.Get(label)
	if !ok {
		t.Fatalf("label %q missing", label)
	}
	return v
}

func Example() {
	r := New()
	r.Add("hits", 2)
	r.Add("hits", 3)
	v, ok := r.Get("hits")
	fmt.Println(v, ok)
	// Output: 5 true
}
```

## Review

The registry is correct when the map is never touched outside the lock and
`Snapshot` is the only way callers see the data. The concurrent-totals test
proves `+=` under one lock does not lose updates; the missing-key test pins that
`Get` reports absence honestly; the snapshot test proves `maps.Clone` returns a
detached copy, which is what makes the scrape path lock-free.

The trap specific to a map registry is handing out the live map — returning
`r.counts` directly, or ranging over it in a method that then calls out to
formatting code — which lets a scraper race a writer and crash the process with
`fatal error: concurrent map writes`. Copy under the lock, iterate after. A
second trap is splitting a read-modify-write of one entry across two lock/unlock
pairs instead of the single `+=`; keep it one critical section. Run `go test
-race`.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock guarding the map.
- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — the copy-under-lock primitive used by `Snapshot`.
- [Go maps in action](https://go.dev/blog/maps) — including that maps are not safe for concurrent use.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — proving the writers do not race.

---

Back to [01-request-inflight-gauge.md](01-request-inflight-gauge.md) | Next: [03-race-contention-test.md](03-race-contention-test.md)
