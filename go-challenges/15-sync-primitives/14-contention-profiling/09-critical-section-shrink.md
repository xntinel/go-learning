# Exercise 9: Shrink a long critical section: snapshot under the lock, serialize outside

Sharding fixes a hot map; it does nothing for a lock that is held too long. This
module takes the second canonical fix to a real artifact — an internal stats
endpoint that JSON-marshals a counter registry — and moves the expensive
serialization out of the critical section: clone under a short lock, marshal
lock-free, and prove with tests and a benchmark pair that the refactor changes
performance, not behavior.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
stats-snapshot/               independent module: example.com/stats-snapshot
  go.mod                      go 1.23+
  registry.go                 type Registry; Inc, NaiveJSON (marshal under lock),
                              SnapshotJSON (maps.Clone then marshal), handlers
  cmd/
    demo/
      main.go                 runnable demo: seed, render both, show equivalence
  registry_test.go            equivalence test, concurrent writers during
                              snapshots, table-driven handler test,
                              BenchmarkNaive vs BenchmarkShrunk, Example
```

- Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: a counter `Registry` with `Inc`, the naive `NaiveJSON` that marshals the live map while holding the mutex, the fixed `SnapshotJSON` that `maps.Clone`s under a short lock and marshals outside, and an `http.Handler` for each wired to `/internal/stats`.
- Test: naive and shrunk emit byte-identical JSON for the same state; concurrent writers during repeated snapshots stay exact under `-race`; both handlers serve correct JSON over `httptest`; `BenchmarkNaive`/`BenchmarkShrunk` under `b.RunParallel` are the profile-capture vehicle.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/14-contention-profiling/09-critical-section-shrink/cmd/demo
cd go-solutions/15-sync-primitives/14-contention-profiling/09-critical-section-shrink
```

### The failure mode: encoding work inside the lock

The naive handler is how this code gets written the first time, and it looks
innocent:

```
mu.Lock()
defer mu.Unlock()
return json.Marshal(counts)     // encoding work while every writer waits
```

`json.Marshal` over a map walks every entry, sorts the keys, reflects on the
values, and allocates the output buffer — microseconds to milliseconds depending
on size. All of it happens while the mutex is held, which means every `Inc` on
every request-serving goroutine queues behind a *monitoring* endpoint. The mutex
profile makes this diagnosis unambiguous: the wait stacks attribute to
`Inc`-callers blocked on the lock, and `list` shows the hold spanning the marshal
call. This shape — cheap mutation serialized behind an occasional expensive read
— is everywhere in production: stats endpoints, config dumps, cache inspection
handlers, debug snapshots.

### The fix: bound the hold time to a copy

The critical section only needs the *data*, not the encoding. So take the lock,
copy the state, release, and do the slow work on the private copy:

```
mu.Lock()
snap := maps.Clone(counts)      // O(n) copy, no reflection, no allocation of output
mu.Unlock()
return json.Marshal(snap)       // slow work on a private copy, writers unblocked
```

`maps.Clone` is a shallow copy — exactly right here because the values are
`int64`; if the values were pointers or slices, the snapshot would share them
with concurrent mutators and you would need a deep copy. The hold time drops
from "marshal of n entries" to "copy of n entries", typically an order of
magnitude or more, and the encoding runs with zero writers excluded.

The trade-offs are real and worth naming. The snapshot is point-in-time: a
counter incremented after the clone but before the response is sent is not in
the response — for monitoring data that staleness is not just acceptable, it is
the definition of a consistent scrape. The clone allocates O(n) per request —
if the endpoint were hot and the map huge, you would move to sharded aggregation
or an RCU-style pointer swap, but for an internal stats route the allocation is
noise. And the copy is still O(n) *under the lock*; shrinking is about removing
the expensive constant factor (reflection, sorting, allocation of the encoded
form), not about making the section O(1).

Both render paths stay behavior-identical, and the tests can prove it byte for
byte because `encoding/json` marshals map keys in sorted order — map iteration
randomness never reaches the output.

Create `registry.go`:

```go
package stats

import (
	"encoding/json"
	"maps"
	"net/http"
	"sync"
)

// Registry is a concurrency-safe counter set behind an internal stats route:
// hot Inc on the request path, occasional JSON renders for monitoring.
type Registry struct {
	mu     sync.Mutex
	counts map[string]int64
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{counts: make(map[string]int64)}
}

// Inc adds delta to the named counter. This is the hot path every
// request-serving goroutine touches.
func (r *Registry) Inc(name string, delta int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[name] += delta
}

// NaiveJSON marshals the live map while holding the lock: every Inc in the
// process queues behind reflection, key sorting, and buffer allocation. This
// is the version the mutex profile convicts.
func (r *Registry) NaiveJSON() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return json.Marshal(r.counts)
}

// SnapshotJSON clones under a short lock and marshals the private copy
// lock-free: hold time is bounded by the copy, not the encoding.
func (r *Registry) SnapshotJSON() ([]byte, error) {
	r.mu.Lock()
	snap := maps.Clone(r.counts)
	r.mu.Unlock()
	return json.Marshal(snap)
}

// NaiveHandler serves /internal/stats with the marshal-under-lock render.
func (r *Registry) NaiveHandler() http.Handler {
	return renderHandler(r.NaiveJSON)
}

// SnapshotHandler serves /internal/stats with the snapshot render.
func (r *Registry) SnapshotHandler() http.Handler {
	return renderHandler(r.SnapshotJSON)
}

func renderHandler(render func() ([]byte, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := render()
		if err != nil {
			http.Error(w, "render stats", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})
}
```

### The runnable demo

The demo seeds the registry the way middleware would, renders through both
paths, and shows they agree byte for byte — the guarantee that lets you swap the
implementation under a live endpoint without a behavior review.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"

	stats "example.com/stats-snapshot"
)

func main() {
	r := stats.NewRegistry()
	r.Inc("http_requests_total", 42)
	r.Inc("cache_hits_total", 7)
	r.Inc("http_requests_total", 1)

	naive, err := r.NaiveJSON()
	if err != nil {
		fmt.Println("naive:", err)
		return
	}
	snap, err := r.SnapshotJSON()
	if err != nil {
		fmt.Println("snapshot:", err)
		return
	}

	fmt.Printf("naive:    %s\n", naive)
	fmt.Printf("snapshot: %s\n", snap)
	fmt.Printf("identical: %v\n", bytes.Equal(naive, snap))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
naive:    {"cache_hits_total":7,"http_requests_total":43}
snapshot: {"cache_hits_total":7,"http_requests_total":43}
identical: true
```

### Tests and the benchmark pair

`TestEquivalence` pins the refactor contract: identical bytes from both paths on
the same state. `TestConcurrentWritersDuringSnapshots` is the `-race` proof that
snapshotting never corrupts or loses writes: writers hammer `Inc` while a reader
loops `SnapshotJSON`, then the final render carries the exact totals.
`TestHandlers` drives both handlers through `httptest` in a table and decodes
the response. The benchmarks are the profile-capture vehicle, not a test
assertion: run

```bash
go test -bench 'Naive|Shrunk' -mutexprofile=mutex.prof .
go tool pprof mutex.prof
```

and compare — under the naive benchmark the wait samples sit on `Inc` callers
blocked behind `NaiveJSON`; under the shrunk benchmark they collapse, because
the marshal no longer runs inside the lock. Wall-clock deltas stay out of the
test assertions; the moved wait stacks in the profile are the evidence.

Create `registry_test.go`:

```go
package stats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func seed(r *Registry) {
	r.Inc("http_requests_total", 42)
	r.Inc("cache_hits_total", 7)
	r.Inc("queue_depth", 3)
}

func TestEquivalence(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	seed(r)

	naive, err := r.NaiveJSON()
	if err != nil {
		t.Fatal(err)
	}
	snap, err := r.SnapshotJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(naive, snap) {
		t.Fatalf("renders differ:\nnaive    = %s\nsnapshot = %s", naive, snap)
	}
}

func TestConcurrentWritersDuringSnapshots(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	const writers, opsPerWriter = 8, 2000
	var wg sync.WaitGroup
	wg.Add(writers + 1)
	for range writers {
		go func() {
			defer wg.Done()
			for range opsPerWriter {
				r.Inc("hits", 1)
			}
		}()
	}
	go func() {
		defer wg.Done()
		for range 200 {
			if _, err := r.SnapshotJSON(); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()

	body, err := r.SnapshotJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]int64
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if want := int64(writers * opsPerWriter); got["hits"] != want {
		t.Fatalf("hits = %d, want %d (snapshotting lost writes)", got["hits"], want)
	}
}

func TestHandlers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		handler func(*Registry) http.Handler
	}{
		{name: "naive", handler: (*Registry).NaiveHandler},
		{name: "snapshot", handler: (*Registry).SnapshotHandler},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := NewRegistry()
			seed(r)
			srv := httptest.NewServer(tt.handler(r))
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + "/internal/stats")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			var got map[string]int64
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got["http_requests_total"] != 42 || got["cache_hits_total"] != 7 || got["queue_depth"] != 3 {
				t.Fatalf("decoded stats = %v", got)
			}
		})
	}
}

func benchmarkRegistry(b *testing.B, render func(*Registry) ([]byte, error)) {
	r := NewRegistry()
	for i := range 64 {
		r.Inc(fmt.Sprintf("metric_%02d", i), int64(i))
	}
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			r.Inc("hits", 1)
			if i%64 == 0 { // periodic render from every worker, like a scraper
				if _, err := render(r); err != nil {
					b.Error(err) // Error, not Fatal: RunParallel bodies are not the main goroutine
					return
				}
			}
			i++
		}
	})
}

func BenchmarkNaive(b *testing.B) {
	benchmarkRegistry(b, (*Registry).NaiveJSON)
}

func BenchmarkShrunk(b *testing.B) {
	benchmarkRegistry(b, (*Registry).SnapshotJSON)
}

func ExampleRegistry_SnapshotJSON() {
	r := NewRegistry()
	r.Inc("http_requests_total", 42)
	r.Inc("cache_hits_total", 7)

	body, _ := r.SnapshotJSON()
	fmt.Println(string(body))
	// Output: {"cache_hits_total":7,"http_requests_total":42}
}
```

## Review

The refactor is done when three facts hold: the two render paths emit identical
bytes for the same state (the contract that makes the swap safe under a live
route), concurrent writers lose nothing under `-race` while snapshots stream, and
the re-captured mutex profile shows the wait stacks off the render path. The
mistakes to avoid: shallow-cloning a map whose values are pointers or slices —
the snapshot then shares mutable state with writers and the marshal races;
holding the lock across `w.Write` as well as the marshal (network writes inside
a critical section are the same bug, worse); and "fixing" this shape by sharding
— sharding distributes point contention across keys, but a whole-map render
still has to visit every shard, so the profile-identified remedy here is
shrinking, not sharding. Note also what the benchmark pair deliberately does not
do: assert times. Run it with `-mutexprofile` and read where the wait went; that
re-measurement is the proof, and it is a human step.

## Resources

- [maps.Clone](https://pkg.go.dev/maps#Clone) — the shallow-copy semantics the snapshot relies on.
- [encoding/json.Marshal](https://pkg.go.dev/encoding/json#Marshal) — map keys are marshaled in sorted order, which makes byte-equality testable.
- [testing.B.RunParallel](https://pkg.go.dev/testing#B.RunParallel) — the parallel benchmark harness used as the profile-capture vehicle.
- [Profiling Go Programs (Go blog)](https://go.dev/blog/pprof) — reading the before/after profiles with top and list.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-cpu-vs-mutex-triage.md](08-cpu-vs-mutex-triage.md) | Next: [10-atomic-and-rwmutex-remedies.md](10-atomic-and-rwmutex-remedies.md)
