# Exercise 4: Request Counters And An Inflight Gauge From Zero-Value Atomics

When you need to instrument a handler but do not want a metrics library on the
critical path, `sync/atomic` gives you counters and flags whose zero value is
already a working, lock-free instrument. This exercise builds a server-metrics
struct that is fully usable as `var m Server` — increment on each request, track
inflight with balanced `Add(+1)`/`Add(-1)`, and flip a drain flag atomically.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
metrics/                   independent module: example.com/metrics
  go.mod
  metrics.go               Server (atomic.Int64/atomic.Bool), Start/Finish/Snapshot
  cmd/
    demo/
      main.go              simulates requests, prints the snapshot
  metrics_test.go          concurrent increments under -race, zero-value snapshot
```

Files: `metrics.go`, `cmd/demo/main.go`, `metrics_test.go`.
Implement: a `Server` embedding `atomic.Int64` (total, errors, inflight) and `atomic.Bool` (draining), usable at its zero value; `Start`, `Finish(failed)`, `BeginDrain`, and a plain-struct `Snapshot`.
Test: N goroutines each run a balanced Start/Finish; assert the final totals under `-race`; the zero value reports all zeros; inflight returns to `0`; `BeginDrain` transitions exactly once via compare-and-swap.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/metrics/cmd/demo
cd ~/go-exercises/metrics
go mod init example.com/metrics
```

## Why the zero value is a working instrument — and must not be copied

`atomic.Int64` and `atomic.Bool` are documented as usable at their zero value:
`var n atomic.Int64` reads as `0` and `n.Add(1)` is a correct atomic increment
with no initialization. So a struct built from them needs no constructor — `var m
Server` is a live instrument. `Start` bumps the total request count and the
inflight gauge; `Finish` decrements inflight and, on failure, bumps the error
count; `Snapshot` reads all four fields with `Load` into a plain, copyable
struct.

The gauge pattern is the interesting one: inflight is not a monotonic counter but
a level, tracked by `Add(+1)` on entry and `Add(-1)` on exit. After any set of
balanced request lifecycles it returns to `0`, and at any instant it is the
number of requests currently in flight — the metric you page on. The drain flag
uses `CompareAndSwap(false, true)` so that exactly one caller "wins" the
transition into draining even if several call `BeginDrain` concurrently; that is
how you make "start draining, once" idempotent under concurrency without a mutex.

The one hard rule: `atomic.Int64`/`atomic.Bool` embed a `noCopy` marker, so
copying a `Server` after first use is a bug that `go vet`'s `copylocks` catches.
A copy would split the counter — increments on the copy would not be seen by the
original. Always use pointer receivers and share `*Server` (or keep the single
`var m Server` addressable and call methods on it directly); never pass a
`Server` by value or store it in a slice/map that copies it.

Create `metrics.go`:

```go
package metrics

import "sync/atomic"

// Server is a lock-free set of request metrics. Its zero value is ready to use:
// var m Server. Do not copy a Server after first use; share *Server.
type Server struct {
	total    atomic.Int64
	errors   atomic.Int64
	inflight atomic.Int64
	draining atomic.Bool
}

// Start records the beginning of a request: one more total, one more inflight.
func (s *Server) Start() {
	s.total.Add(1)
	s.inflight.Add(1)
}

// Finish records the end of a request: one fewer inflight, and one more error
// if the request failed.
func (s *Server) Finish(failed bool) {
	s.inflight.Add(-1)
	if failed {
		s.errors.Add(1)
	}
}

// BeginDrain flips the draining flag from false to true exactly once. It returns
// true for the caller that performed the transition, false if draining was
// already set.
func (s *Server) BeginDrain() bool {
	return s.draining.CompareAndSwap(false, true)
}

// Draining reports whether the server is draining.
func (s *Server) Draining() bool {
	return s.draining.Load()
}

// Snapshot is a plain, copyable view of the metrics at one instant.
type Snapshot struct {
	Total    int64
	Errors   int64
	Inflight int64
	Draining bool
}

// Snapshot reads all counters and the drain flag.
func (s *Server) Snapshot() Snapshot {
	return Snapshot{
		Total:    s.total.Load(),
		Errors:   s.errors.Load(),
		Inflight: s.inflight.Load(),
		Draining: s.draining.Load(),
	}
}
```

## The runnable demo

The demo declares a zero-value `Server`, simulates a handful of requests (one
failing), begins draining, and prints the snapshot. Inflight is `0` because every
`Start` is balanced by a `Finish`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/metrics"
)

func main() {
	var m metrics.Server

	for i := range 5 {
		m.Start()
		m.Finish(i == 2) // the third request fails
	}

	first := m.BeginDrain()
	second := m.BeginDrain()

	s := m.Snapshot()
	fmt.Printf("total=%d errors=%d inflight=%d draining=%v\n",
		s.Total, s.Errors, s.Inflight, s.Draining)
	fmt.Printf("begin-drain first=%v second=%v\n", first, second)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total=5 errors=1 inflight=0 draining=true
begin-drain first=true second=false
```

## Tests

`TestZeroValueSnapshot` proves a bare `var m Server` reports all zeros.
`TestConcurrentRequests` launches N goroutines each running a balanced
Start/Finish and asserts total `== N`, errors `== N/2` (every even index fails),
and inflight `== 0` — under `-race`, this proves the atomics actually serialize
the increments. `TestBeginDrainOnce` runs many goroutines racing to begin drain
and asserts exactly one won the compare-and-swap.

Create `metrics_test.go`:

```go
package metrics

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestZeroValueSnapshot(t *testing.T) {
	t.Parallel()

	var m Server
	if s := m.Snapshot(); s != (Snapshot{}) {
		t.Fatalf("zero value Snapshot = %+v, want all zero", s)
	}
}

func TestConcurrentRequests(t *testing.T) {
	t.Parallel()

	const n = 1000
	var m Server
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Start()
			m.Finish(i%2 == 0)
		}()
	}
	wg.Wait()

	s := m.Snapshot()
	if s.Total != n {
		t.Fatalf("Total = %d, want %d", s.Total, n)
	}
	if s.Errors != n/2 {
		t.Fatalf("Errors = %d, want %d", s.Errors, n/2)
	}
	if s.Inflight != 0 {
		t.Fatalf("Inflight = %d, want 0 after balanced Start/Finish", s.Inflight)
	}
}

func TestBeginDrainOnce(t *testing.T) {
	t.Parallel()

	var m Server
	var wg sync.WaitGroup
	var wins atomic.Int64
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.BeginDrain() {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Fatalf("BeginDrain winners = %d, want exactly 1", got)
	}
	if !m.Draining() {
		t.Fatal("Draining() = false after BeginDrain")
	}
}
```

## Review

The metrics are correct when a zero-value `Server` is a working instrument and
concurrent increments are never lost. The proof is `TestConcurrentRequests` under
`-race`: if any increment used a plain `int64` instead of `atomic.Int64`, the
race detector fires and the total comes up short. Inflight returning to `0` is
the signal that every `Start` was balanced by a `Finish` — an unbalanced pair
leaks the gauge upward and is a classic "inflight climbs forever" incident. The
`CompareAndSwap` on the drain flag is what makes "begin draining" fire exactly
once under concurrency. The trap to avoid is copying the struct: pass `*Server`
everywhere, and let `go vet` `copylocks` flag any accidental value copy.

## Resources

- [`sync/atomic.Int64`](https://pkg.go.dev/sync/atomic#Int64) — `Add`, `Load`, `Store`, `CompareAndSwap`; zero value ready to use.
- [`sync/atomic.Bool`](https://pkg.go.dev/sync/atomic#Bool) — atomic flag with `CompareAndSwap`.
- [`go vet` copylocks](https://pkg.go.dev/cmd/vet) — flags copying a value that must not be copied.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-nil-map-config-merge.md](03-nil-map-config-merge.md) | Next: [05-last-seen-time-iszero.md](05-last-seen-time-iszero.md)
