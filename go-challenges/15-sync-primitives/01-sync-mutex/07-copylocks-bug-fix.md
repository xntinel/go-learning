# Exercise 7: Copied-lock bug hunt: fixing a per-route latency recorder with go vet copylocks

A copied `sync.Mutex` is one of the very few concurrency bugs the compiler
accepts, the runtime never flags directly, and only static analysis catches
reliably: the copy is a fresh unlocked mutex that synchronizes with nothing.
This module builds a per-route latency recorder for a `/debug/stats` endpoint,
ships the two classic copy vectors behind a build tag so `go vet -tags copybug`
visibly reports them, and implements the fixed version the way production code
must: pointer receivers everywhere, and a snapshot that returns a plain
lock-free value type.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
latstats/                    independent module: example.com/latstats
  go.mod                     go 1.26
  recorder.go                type Recorder, type Stats; New, Record, Snapshot
  buggy.go                   //go:build copybug — the copylocks bugs, on demand
  cmd/
    demo/
      main.go                runnable demo: mean/max latency per route
  recorder_test.go           exact contention totals, snapshot detachment, Example
```

- Files: `recorder.go`, `buggy.go`, `cmd/demo/main.go`, `recorder_test.go`.
- Implement: a `Recorder` (mutex + `map[string]*agg` of count/sum/max) with pointer-receiver `Record` and a `Snapshot` that copies raw fields under the lock and computes the mean after `Unlock`; plus a tag-guarded `buggyRecorder` with a value receiver and a by-value struct return.
- Test: 100 goroutines x 100 `Record` calls assert exact count, exact sum, and correct max under `-race`; mutating the returned `Stats` provably cannot touch the recorder.
- Verify: `go test -count=1 -race ./...` and `go vet ./...` (both clean), then `go vet -tags copybug ./...` (must report copylocks diagnostics).

```bash
mkdir -p go-solutions/15-sync-primitives/01-sync-mutex/07-copylocks-bug-fix/cmd/demo
cd go-solutions/15-sync-primitives/01-sync-mutex/07-copylocks-bug-fix
```

### Why a copied mutex is invisible until production

`sync.Mutex` is an ordinary struct value. Assigning it, passing it as a
parameter, returning it, or calling a method on a value receiver copies it —
and Go's compiler is perfectly happy to do so. The copy carries the *state
bits* of the original at the moment of the copy but is, semantically, a brand
new lock: goroutine A locking the original and goroutine B locking the copy do
not exclude each other and establish no happens-before edge between their
critical sections. The data both thought was guarded is now racing. Worse, a
copy of a *locked* mutex is permanently locked from its own point of view; the
first `Lock` on it hangs forever.

The two vectors that reach production code again and again are exactly the two
this module tags out:

- a **value receiver** on a method of a lock-containing struct — every call
  operates on a throwaway copy, so `Record` on the buggy type locks a mutex
  that no other call can see, and the `count++` behind it is unsynchronized;
- **returning the struct by value** from a snapshot method — the caller
  receives a copy of the mutex along with the data, and any later method call
  on that copy synchronizes with nothing.

The tool that catches both statically is `go vet`'s `copylocks` analyzer
("check for locks erroneously passed by value"). It runs in CI in every serious
Go shop, which is why the fix pattern below matters: it is what keeps vet
silent for the right reasons, not by suppression.

### The fixed design: pointer receivers plus a lock-free value type

The recorder aggregates `count`, `sum`, and `max` of `time.Duration` per route.
Every method uses a pointer receiver, so there is exactly one mutex for the
lifetime of the `Recorder`. The interesting design decision is `Snapshot`. The
`/debug/stats` handler wants a self-contained value it can format without
holding any lock — so `Snapshot` returns `Stats`, a plain struct with **no
mutex inside**. Copying `Stats` is always safe; there is nothing in it that vet
or the memory model cares about.

`Snapshot` also demonstrates the tail-latency rule from the concepts file: the
raw fields (`count`, `sum`, `max`) are copied under the lock — three word-sized
reads, nanoseconds — and the division that produces the mean happens *after*
`Unlock`. Division is cheap, but the habit is the point: everything that can be
computed from copied fields belongs outside the critical section, because every
nanosecond under the lock is added to every queued `Record` from live request
handlers. Note the division is safe here by construction: an `agg` only exists
after at least one `Record`, so `Count` is never zero when `Snapshot` finds the
route.

The `max` builtin (Go 1.21+) works on `time.Duration` because durations are an
ordered integer type — no hand-rolled comparison needed.

Create `recorder.go`:

```go
package latstats

import (
	"sync"
	"time"
)

// Stats is the lock-free value type Snapshot returns. It contains no mutex,
// so copying, storing, and passing it around is always safe.
type Stats struct {
	Count int64
	Sum   time.Duration
	Max   time.Duration
	Mean  time.Duration
}

// agg is the per-route aggregate, only ever touched under Recorder.mu.
type agg struct {
	count int64
	sum   time.Duration
	max   time.Duration
}

// Recorder aggregates request latencies per route. All methods use pointer
// receivers: the mutex must never be copied after first use.
type Recorder struct {
	mu     sync.Mutex
	routes map[string]*agg
}

// New returns an empty Recorder.
func New() *Recorder {
	return &Recorder{routes: make(map[string]*agg)}
}

// Record adds one latency observation for route.
func (r *Recorder) Record(route string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.routes[route]
	if !ok {
		a = &agg{}
		r.routes[route] = a
	}
	a.count++
	a.sum += d
	a.max = max(a.max, d)
}

// Snapshot returns the aggregate for route as a detached Stats value. The raw
// fields are copied under the lock; the division for the mean runs after
// Unlock, outside the critical section.
func (r *Recorder) Snapshot(route string) (Stats, bool) {
	r.mu.Lock()
	a, ok := r.routes[route]
	if !ok {
		r.mu.Unlock()
		return Stats{}, false
	}
	s := Stats{Count: a.count, Sum: a.sum, Max: a.max}
	r.mu.Unlock()

	s.Mean = s.Sum / time.Duration(s.Count)
	return s, true
}
```

### The bug, preserved behind a build tag

Like the `racebug` counter in exercise 3, the broken variant lives behind a
build constraint so the default `go build`, `go vet`, and `go test` never see
it, while `go vet -tags copybug ./...` pulls it in and reports both copylocks
diagnostics on demand. Keeping a reproducible known-bad file in the tree is how
you teach the next engineer what the analyzer output looks like without ever
breaking CI.

Create `buggy.go`:

```go
//go:build copybug

package latstats

import (
	"sync"
	"time"
)

// buggyRecorder demonstrates the two classic copylocks vectors. It is excluded
// from the default build; run `go vet -tags copybug ./...` to see the reports.
type buggyRecorder struct {
	mu    sync.Mutex
	count int64
	sum   time.Duration
}

// Record has a VALUE receiver: every call locks a private copy of mu, so no
// two calls exclude each other and count++ races. vet reports:
//
//	Record passes lock by value: example.com/latstats.buggyRecorder contains sync.Mutex
func (r buggyRecorder) Record(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count++
	r.sum += d
}

// snapshotByValue returns the lock-containing struct itself. The caller gets a
// copy of the mutex along with the data. vet reports:
//
//	return copies lock value: example.com/latstats.buggyRecorder contains sync.Mutex
func (r *buggyRecorder) snapshotByValue() buggyRecorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *r
}
```

Run the analyzer both ways:

```bash
go vet ./...
go vet -tags copybug ./...
```

The first command is silent. The second exits non-zero with two diagnostics
naming `buggyRecorder`, one per vector — `passes lock by value` for the value
receiver and `return copies lock value` for the by-value return. That output
is the signature to recognize in a real CI failure.

### The runnable demo

The demo records a handful of latencies for two routes and prints what the
`/debug/stats` handler would render: count, mean, and max per route, all read
from detached `Stats` values with no lock held during formatting.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/latstats"
)

func main() {
	r := latstats.New()

	for _, d := range []time.Duration{
		12 * time.Millisecond, 48 * time.Millisecond, 30 * time.Millisecond,
	} {
		r.Record("GET /orders", d)
	}
	r.Record("POST /orders", 95*time.Millisecond)
	r.Record("POST /orders", 105*time.Millisecond)

	for _, route := range []string{"GET /orders", "POST /orders"} {
		s, _ := r.Snapshot(route)
		fmt.Printf("%-12s count=%d mean=%s max=%s\n", route, s.Count, s.Mean, s.Max)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /orders  count=3 mean=30ms max=48ms
POST /orders count=2 mean=100ms max=105ms
```

### Tests

`TestRecordExactUnderContention` gives each of 100 goroutines a distinct
duration `(g+1) ms` and has it record that value 100 times, so every aggregate
field has one exact correct answer: count `10000`, sum `100 * (1+2+...+100) ms`,
max `100ms`, mean `50.5ms`. Any lost update shifts the sum or count and the
test fails even before `-race` speaks. `TestSnapshotStats` is the table-driven
correctness check for the aggregation math, and `TestSnapshotIsDetached` proves
the returned `Stats` shares no state with the recorder in either direction.

Create `recorder_test.go`:

```go
package latstats

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRecordExactUnderContention(t *testing.T) {
	t.Parallel()

	r := New()
	const goroutines, perG = 100, 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func() {
			defer wg.Done()
			d := time.Duration(g+1) * time.Millisecond
			for range perG {
				r.Record("GET /orders", d)
			}
		}()
	}
	wg.Wait()

	s, ok := r.Snapshot("GET /orders")
	if !ok {
		t.Fatal("Snapshot() ok = false, want the recorded route")
	}
	wantCount := int64(goroutines * perG)
	wantSum := time.Duration(perG*goroutines*(goroutines+1)/2) * time.Millisecond
	wantMax := time.Duration(goroutines) * time.Millisecond
	wantMean := wantSum / time.Duration(wantCount)
	if s.Count != wantCount {
		t.Errorf("Count = %d, want %d (a lost update means Record raced)", s.Count, wantCount)
	}
	if s.Sum != wantSum {
		t.Errorf("Sum = %s, want %s", s.Sum, wantSum)
	}
	if s.Max != wantMax {
		t.Errorf("Max = %s, want %s", s.Max, wantMax)
	}
	if s.Mean != wantMean {
		t.Errorf("Mean = %s, want %s", s.Mean, wantMean)
	}
}

func TestSnapshotStats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		durs []time.Duration
		want Stats
	}{
		{
			name: "single observation",
			durs: []time.Duration{10 * time.Millisecond},
			want: Stats{Count: 1, Sum: 10 * time.Millisecond, Max: 10 * time.Millisecond, Mean: 10 * time.Millisecond},
		},
		{
			name: "mean is integer duration division",
			durs: []time.Duration{3 * time.Millisecond, 4 * time.Millisecond},
			want: Stats{Count: 2, Sum: 7 * time.Millisecond, Max: 4 * time.Millisecond, Mean: 3500 * time.Microsecond},
		},
		{
			name: "max tracks the outlier",
			durs: []time.Duration{5 * time.Millisecond, 250 * time.Millisecond, 15 * time.Millisecond},
			want: Stats{Count: 3, Sum: 270 * time.Millisecond, Max: 250 * time.Millisecond, Mean: 90 * time.Millisecond},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := New()
			for _, d := range tt.durs {
				r.Record("GET /x", d)
			}
			got, ok := r.Snapshot("GET /x")
			if !ok {
				t.Fatal("Snapshot() ok = false, want true")
			}
			if got != tt.want {
				t.Errorf("Snapshot() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSnapshotMissingRoute(t *testing.T) {
	t.Parallel()

	r := New()
	if s, ok := r.Snapshot("GET /nope"); ok || s != (Stats{}) {
		t.Errorf("Snapshot(missing) = %+v, %v; want zero Stats and ok=false", s, ok)
	}
}

func TestSnapshotIsDetached(t *testing.T) {
	t.Parallel()

	r := New()
	r.Record("POST /orders", 10*time.Millisecond)

	s1, _ := r.Snapshot("POST /orders")
	s1.Count = 999 // mutating the copy must not reach the recorder
	s1.Max = time.Hour

	r.Record("POST /orders", 30*time.Millisecond)
	s2, _ := r.Snapshot("POST /orders")
	want := Stats{Count: 2, Sum: 40 * time.Millisecond, Max: 30 * time.Millisecond, Mean: 20 * time.Millisecond}
	if s2 != want {
		t.Errorf("Snapshot() after mutating an old copy = %+v, want %+v", s2, want)
	}
}

func ExampleRecorder_Snapshot() {
	r := New()
	r.Record("GET /health", 2*time.Millisecond)
	r.Record("GET /health", 4*time.Millisecond)
	s, ok := r.Snapshot("GET /health")
	fmt.Println(ok, s.Count, s.Mean, s.Max)
	// Output: true 2 3ms 4ms
}
```

## Review

The recorder is correct when every method has a pointer receiver, the only
thing crossing the API boundary is the lock-free `Stats` value, and the mean is
derived from fields copied under the lock rather than computed while holding
it. The contention test's exact sum is the sharp assertion: a value-receiver
`Record` would still compile, still run, and still print plausible numbers in a
demo — but under 100 goroutines the sum would come up short and `-race` would
name the line.

The mistake to internalize is trusting the compiler here: it will never warn
about a copied mutex, and `-race` only catches the copy if a test actually
drives the racing interleaving. `go vet`'s copylocks analyzer is the cheap,
deterministic guard — it belongs in CI next to `-race`, and after this module
you know exactly what its two report shapes look like. Run
`go test -count=1 -race ./...` and `go vet ./...` (clean), then
`go vet -tags copybug ./...` to see the failure you are preventing.

## Resources

- [`cmd/vet`](https://pkg.go.dev/cmd/vet) — the copylocks check: "check for locks erroneously passed by value".
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — "a Mutex must not be copied after first use".
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — the runtime tool that catches a driven copied-lock race; vet is the static one.
- [Go spec: min and max](https://go.dev/ref/spec#Min_and_max) — the `max` builtin used for the running maximum.

---

Back to [06-idempotency-dedupe-store.md](06-idempotency-dedupe-store.md) | Next: [08-ordered-lock-transfer.md](08-ordered-lock-transfer.md)
