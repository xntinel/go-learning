# Exercise 10: Apply the remaining remedies profile-first: atomic counters and an RWMutex read path

The last two canonical fixes get their production artifact: request-metrics
middleware whose hot counters move from a mutex to `atomic.Int64`, and a
read-mostly feature-flag store whose lookup path moves to `sync.RWMutex` — each
remedy matched to what the mutex profile actually shows, and the win proven by a
profile comparison in a test: the mutex version's stacks appear in the profile,
the atomic version's cannot.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
reqmetrics/                   independent module: example.com/reqmetrics
  go.mod                      go 1.23+
  metrics.go                  type Metrics (atomic.Int64 counters, the after),
                              type MutexMetrics (the before, kept for comparison)
  flags.go                    type FlagStore (sync.RWMutex read-mostly lookup)
  middleware.go               Middleware: in-flight tracking, status recording,
                              maintenance-flag gate; statusRecorder
  cmd/
    demo/
      main.go                 runnable demo: drive requests, print exact counters
  reqmetrics_test.go          concurrent-middleware totals, flag consistency,
                              mutex-profile comparison, table test, Example
```

- Files: `metrics.go`, `flags.go`, `middleware.go`, `cmd/demo/main.go`, `reqmetrics_test.go`.
- Implement: `Metrics` with `atomic.Int64` total/in-flight/per-status-class counters; the retired `MutexMetrics` kept as the profile's before-picture; `FlagStore` with an `RWMutex`-guarded `Enabled`; `Middleware` wiring all three around any `http.Handler`.
- Test: N concurrent requests through `httptest` produce exact totals and status breakdown under `-race`; readers see a consistent flag during concurrent flips; a mutex profile captured under identical load contains the `MutexMetrics` stack and cannot contain the atomic path.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/14-contention-profiling/10-atomic-and-rwmutex-remedies/cmd/demo
cd go-solutions/15-sync-primitives/14-contention-profiling/10-atomic-and-rwmutex-remedies
```

### Remedy three: atomics for the hot counter

When the mutex profile's top stack is a lock that guards nothing but integer
arithmetic — `total++`, `inFlight++`, `statusCounts[class]++` — the lock is pure
overhead. There is no invariant spanning multiple fields that a reader could
observe torn in any way that matters for monitoring: three independent counters
do not need to advance atomically *together*, only each atomically *alone*.
`atomic.Int64.Add` compiles to a single lock-prefixed instruction (or LSE
atomic on arm64); there is no queue, no parking, no scheduler round-trip, and
therefore *nothing for the mutex profiler to record* — an atomic operation never
appears in a mutex profile because no goroutine ever blocks on it. That absence
is exactly what the comparison test asserts.

The honest trade-off: you give up cross-field consistency. A scraper reading
`Total` and `ByClass` mid-burst can see totals that do not sum exactly, because
another `Record` landed between the loads. For metrics this is correct-enough by
definition; for an invariant like "balance equals the sum of postings" it would
be a bug, and you keep the mutex. Matching the remedy to the data's consistency
requirement — not to a reflexive "atomics are faster" — is the senior judgment
this module encodes.

### Remedy four: RWMutex for the read-mostly path

The feature-flag store is the opposite shape: consulted on *every* request,
written on the rare deploy or toggle. Under a plain `Mutex`, readers exclude
each other — a thousand concurrent requests serialize on a lookup that mutates
nothing. `sync.RWMutex.RLock` admits any number of concurrent readers and makes
only writers exclusive, so the read path stops generating contention entirely
while writes stay safe. The known caveats: an `RWMutex` is slower than a plain
`Mutex` when writes are frequent (writer preference logic costs), and it is
catastrophically wrong as a "fix" for a *write*-hot path. Again: the profile
decides. Wait stacks dominated by `RLock`-eligible readers are the signature
that says RWMutex; wait stacks full of writers say it will change nothing.

### Proving the fix with the profile, not vibes

`TestMutexProfileShowsOnlyTheMutexPath` drives identical load through both
counter implementations with the fraction at 1, then writes the mutex profile
with `WriteTo(&buf, 1)` — `debug=1` emits the symbolized text form, so the test
can assert on function names. Two assertions: the profile *contains*
`MutexMetrics` (the before-picture lights up under this load), and it does *not*
contain `(*Metrics).Record` (the atomic path cannot block, so it cannot appear —
ever, no matter what ran earlier in the process, which matters because the mutex
profile is cumulative). `MutexMetrics.Record` carries `//go:noinline` so its
frame is guaranteed to survive into the recorded stacks; without it the compiler
may inline the method into the calling loop and the symbol you grep for
disappears even though the contention is real.

Create `metrics.go`:

```go
package reqmetrics

import (
	"sync"
	"sync/atomic"
)

// Metrics is the lock-free request-metrics collector: the profile-identified
// hot counters on atomic.Int64. An atomic Add never blocks, so this path can
// never appear in a mutex profile.
type Metrics struct {
	total    atomic.Int64
	inFlight atomic.Int64
	status   [6]atomic.Int64 // index = status/100 (1xx..5xx); 0 = unclassifiable
}

// Record counts one completed request with its status code.
func (m *Metrics) Record(status int) {
	m.total.Add(1)
	m.status[statusClass(status)].Add(1)
}

// Total returns the number of completed requests.
func (m *Metrics) Total() int64 { return m.total.Load() }

// InFlight returns the number of requests currently being served.
func (m *Metrics) InFlight() int64 { return m.inFlight.Load() }

// ByClass returns the count of responses in a status class (2 means 2xx).
func (m *Metrics) ByClass(class int) int64 {
	if class < 0 || class > 5 {
		return 0
	}
	return m.status[class].Load()
}

// MutexMetrics is the before-picture: the same counters behind one mutex.
// Kept so the profile comparison can show the wait stacks this design costs.
type MutexMetrics struct {
	mu     sync.Mutex
	total  int64
	status [6]int64
}

// Record counts one completed request under the lock.
//
//go:noinline
func (m *MutexMetrics) Record(status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total++
	m.status[statusClass(status)]++
}

// Total returns the number of completed requests under the lock.
func (m *MutexMetrics) Total() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

func statusClass(status int) int {
	class := status / 100
	if class < 1 || class > 5 {
		return 0
	}
	return class
}
```

Create `flags.go`:

```go
package reqmetrics

import "sync"

// FlagStore is a read-mostly feature-flag lookup consulted on every request
// and written on the rare toggle: the RWMutex shape. Readers run concurrently;
// only writers exclude.
type FlagStore struct {
	mu    sync.RWMutex
	flags map[string]bool
}

// NewFlagStore returns an empty flag store.
func NewFlagStore() *FlagStore {
	return &FlagStore{flags: make(map[string]bool)}
}

// Set toggles a flag; the exclusive write path.
func (f *FlagStore) Set(name string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flags[name] = on
}

// Enabled reports whether a flag is on; the shared read path taken per request.
func (f *FlagStore) Enabled(name string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.flags[name]
}
```

Create `middleware.go`:

```go
package reqmetrics

import "net/http"

// Middleware wraps next with in-flight tracking, per-status counting, and a
// maintenance-mode gate driven by the flag store.
func Middleware(m *Metrics, flags *FlagStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.inFlight.Add(1)
		defer m.inFlight.Add(-1)

		if flags.Enabled("maintenance") {
			http.Error(w, "maintenance in progress", http.StatusServiceUnavailable)
			m.Record(http.StatusServiceUnavailable)
			return
		}

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		m.Record(rec.status)
	})
}

// statusRecorder captures the status code the inner handler wrote so the
// middleware can classify it after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
```

### The runnable demo

The demo builds a tiny service behind the middleware, drives a mixed sequence —
three successes, one 404, then a request while maintenance mode is on — and
prints the exact breakdown the counters collected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	reqmetrics "example.com/reqmetrics"
)

func main() {
	var m reqmetrics.Metrics
	flags := reqmetrics.NewFlagStore()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	srv := httptest.NewServer(reqmetrics.Middleware(&m, flags, mux))
	defer srv.Close()

	get := func(path string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			panic(err)
		}
		resp.Body.Close()
	}

	for range 3 {
		get("/ok")
	}
	get("/missing")

	flags.Set("maintenance", true)
	get("/ok")

	fmt.Printf("total=%d inflight=%d\n", m.Total(), m.InFlight())
	fmt.Printf("2xx=%d 4xx=%d 5xx=%d\n", m.ByClass(2), m.ByClass(4), m.ByClass(5))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total=5 inflight=0
2xx=3 4xx=1 5xx=1
```

### Tests

`TestMiddlewareCountsUnderLoad` fires 200 concurrent requests (half hitting a
real route, half a 404) and asserts the exact totals — the `-race`-verified proof
that lock-free does not mean approximately-correct. `TestFlagConsistency` flips a
flag from many writers while readers hammer `Enabled`, then asserts the final
state; the race detector guarantees the RWMutex actually covers the map.
`TestMutexProfileShowsOnlyTheMutexPath` is the profile comparison described
above. `TestStatusClass` pins the bucketing in a table.

Create `reqmetrics_test.go`:

```go
package reqmetrics

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
)

func TestMiddlewareCountsUnderLoad(t *testing.T) {
	t.Parallel()
	var m Metrics
	flags := NewFlagStore()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	srv := httptest.NewServer(Middleware(&m, flags, mux))
	t.Cleanup(srv.Close)

	const perPath = 100
	var wg sync.WaitGroup
	wg.Add(2 * perPath)
	for range perPath {
		for _, path := range []string{"/ok", "/missing"} {
			go func() {
				defer wg.Done()
				resp, err := http.Get(srv.URL + path)
				if err != nil {
					t.Error(err)
					return
				}
				resp.Body.Close()
			}()
		}
	}
	wg.Wait()

	if got := m.Total(); got != 2*perPath {
		t.Fatalf("Total = %d, want %d", got, 2*perPath)
	}
	if got := m.InFlight(); got != 0 {
		t.Fatalf("InFlight after drain = %d, want 0", got)
	}
	if got := m.ByClass(2); got != perPath {
		t.Fatalf("2xx = %d, want %d", got, perPath)
	}
	if got := m.ByClass(4); got != perPath {
		t.Fatalf("4xx = %d, want %d", got, perPath)
	}
}

func TestMaintenanceGate(t *testing.T) {
	t.Parallel()
	var m Metrics
	flags := NewFlagStore()
	flags.Set("maintenance", true)

	srv := httptest.NewServer(Middleware(&m, flags,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("inner handler must not run during maintenance")
		})))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/ok")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if got := m.ByClass(5); got != 1 {
		t.Fatalf("5xx = %d, want 1", got)
	}
}

func TestFlagConsistency(t *testing.T) {
	t.Parallel()
	flags := NewFlagStore()

	const writers, readers, flips = 4, 8, 500
	var wg sync.WaitGroup
	wg.Add(writers + readers)
	for range writers {
		go func() {
			defer wg.Done()
			for i := range flips {
				flags.Set("beta", i%2 == 0)
			}
		}()
	}
	for range readers {
		go func() {
			defer wg.Done()
			for range flips {
				_ = flags.Enabled("beta") // -race proves the RWMutex covers the map
			}
		}()
	}
	wg.Wait()

	flags.Set("beta", true)
	if !flags.Enabled("beta") {
		t.Fatal("Enabled(beta) = false after final Set(true)")
	}
}

func TestMutexProfileShowsOnlyTheMutexPath(t *testing.T) {
	// Process-global profiler state: not parallel.
	prev := runtime.SetMutexProfileFraction(1)
	t.Cleanup(func() { runtime.SetMutexProfileFraction(prev) })

	const goroutines, ops = 32, 3000
	hammer := func(record func(int)) {
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for range goroutines {
			go func() {
				defer wg.Done()
				for range ops {
					record(http.StatusOK)
				}
			}()
		}
		wg.Wait()
	}

	var before MutexMetrics
	hammer(before.Record)
	var after Metrics
	hammer(after.Record)

	if got := before.Total(); got != goroutines*ops {
		t.Fatalf("MutexMetrics.Total = %d, want %d", got, goroutines*ops)
	}
	if got := after.Total(); got != goroutines*ops {
		t.Fatalf("Metrics.Total = %d, want %d", got, goroutines*ops)
	}

	var buf bytes.Buffer
	if err := pprof.Lookup("mutex").WriteTo(&buf, 1); err != nil {
		t.Fatal(err)
	}
	text := buf.String()

	if !strings.Contains(text, "MutexMetrics") {
		t.Fatal("mutex profile has no MutexMetrics stack: the before-picture never contended")
	}
	if strings.Contains(text, "(*Metrics).Record") {
		t.Fatal("mutex profile attributes samples to the atomic path, which cannot block")
	}
}

func TestStatusClass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status int
		want   int
	}{
		{status: 200, want: 2},
		{status: 204, want: 2},
		{status: 404, want: 4},
		{status: 503, want: 5},
		{status: 100, want: 1},
		{status: 0, want: 0},
		{status: 999, want: 0},
	}
	for _, tt := range tests {
		if got := statusClass(tt.status); got != tt.want {
			t.Fatalf("statusClass(%d) = %d, want %d", tt.status, got, tt.want)
		}
	}
}

func ExampleMetrics() {
	var m Metrics
	m.Record(200)
	m.Record(200)
	m.Record(404)
	fmt.Println(m.Total(), m.ByClass(2), m.ByClass(4))
	// Output: 3 2 1
}
```

## Review

The module is correct when the exact totals survive 200 truly concurrent
requests under `-race`, the maintenance gate short-circuits without invoking the
inner handler, and the profile comparison holds: `MutexMetrics` stacks present,
`(*Metrics).Record` impossible. The judgment to carry away is the matching, not
the mechanism. Atomics fit *independent* counters — reach for them when the
profile shows a lock guarding nothing but arithmetic, and keep the mutex when
readers need multiple fields to be mutually consistent. RWMutex fits paths the
profile shows as read-dominated — it makes a write-hot path *worse*. Mistakes to
avoid: mixing atomic and mutex access to the same field (all access must go
through one discipline); reading two atomic counters and treating their
relationship as a consistent snapshot; dropping the `//go:noinline` and then
trusting a symbol-name grep against an inlined frame; and asserting sample
counts or timings — the test asserts presence and impossibility, the two things
the profiler actually guarantees. This closes the chapter's loop: gauge, expose,
capture, triage, and now every remedy applied where — and only where — the
profile pointed.

## Resources

- [sync/atomic.Int64](https://pkg.go.dev/sync/atomic#Int64) — the typed atomic counters and their Add/Load contract.
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — reader/writer semantics and when writers block readers.
- [runtime/pprof.Profile.WriteTo](https://pkg.go.dev/runtime/pprof#Profile.WriteTo) — debug=1 symbolized text output used by the comparison test.
- [Diagnostics (official Go documentation)](https://go.dev/doc/diagnostics) — the official map of Go's profiling and tracing tools.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-critical-section-shrink.md](09-critical-section-shrink.md) | Next: [../../16-concurrency-patterns/01-pipeline-pattern/00-concepts.md](../../16-concurrency-patterns/01-pipeline-pattern/00-concepts.md)
