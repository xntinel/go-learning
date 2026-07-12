# Exercise 7: Turn cumulative lock-wait into a continuous SLO gauge with runtime/metrics

Profiles are the targeted tool you raise during an incident; what tells you an
incident is brewing is a cheap, always-on number. This module builds that number:
a collector around `runtime/metrics`' `/sync/mutex/wait/total:seconds` whose
per-scrape delta is the rate at which the whole process is accumulating lock-wait
— the gauge an SRE alerts on before anyone reaches for pprof.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
lockwait-gauge/               independent module: example.com/lockwait-gauge
  go.mod                      go 1.23+
  collector.go                type Collector; NewCollector (name+kind validation),
                              Scrape (delta + cumulative total), ErrUnknownMetric
  burst.go                    Burst: a contention load generator with a
                              non-elidable critical section (busyWork + sink)
  cmd/
    demo/
      main.go                 runnable demo: scrape, contention burst, scrape,
                              show the delta moved
  collector_test.go           table-driven constructor validation, burst
                              integration test, monotonicity, Example
```

- Files: `collector.go`, `burst.go`, `cmd/demo/main.go`, `collector_test.go`.
- Implement: `NewCollector(name)` that rejects a metric absent from `metrics.All()` or of the wrong kind with the sentinel `ErrUnknownMetric`; `Scrape()` returning the delta since the previous scrape plus the cumulative total; `Burst` to generate real `sync.Mutex` wait.
- Test: the constructor table (valid name, unknown name, wrong-kind name); an integration test asserting the delta across a contention burst is strictly positive and the total is monotonic; no wall-time assertions anywhere.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/14-contention-profiling/07-lock-wait-slo-gauge/cmd/demo
cd go-solutions/15-sync-primitives/14-contention-profiling/07-lock-wait-slo-gauge
```

### Why a gauge, when we already have profiles

The mutex profile answers "which stack" but costs overhead and produces an
artifact a human must read. You cannot leave that loop running as monitoring.
`runtime/metrics` exposes the aggregate instead: `/sync/mutex/wait/total:seconds`
is the approximate cumulative time goroutines have spent blocked on a
`sync.Mutex`, `sync.RWMutex`, or runtime-internal lock, maintained by the runtime
at near-zero cost, independent of the mutex-profile fraction. It is a counter —
monotonically non-decreasing — so the operationally meaningful number is the
*delta between scrapes*: seconds of lock-wait accumulated per scrape interval. A
process burning 0.5 seconds of lock-wait per second of wall time across its
goroutines has a contention problem worth profiling; a process near zero does
not. The division of labor is exactly the concepts file's loop: the gauge is
continuous and cheap and tells you *when*; the profile is targeted and expensive
and tells you *where*.

### Guarding the metric name — the silent-zero failure mode

`metrics.Read` has an unusual contract: you hand it a `[]metrics.Sample` with
the `Name` fields filled in, and it fills in the `Value`s. If a name is unknown
to the runtime — because a Go release renamed or removed it — `Read` does not
return an error; it sets that sample's `Value.Kind()` to `metrics.KindBad` and
moves on. Code that skips the kind check reads a zero value forever. On a
dashboard that failure is invisible: the lock-wait graph flatlines at zero, which
looks like *good news*, while a real regression hides behind it. That is why the
collector validates at construction, not at scrape time: `NewCollector` walks
`metrics.All()` (the runtime's own catalog of supported metrics, with each
metric's kind) and rejects a name that is missing or not `KindFloat64` with the
sentinel `ErrUnknownMetric`, wrapped with `%w`. A deploy of a bad build then
fails loudly at startup instead of monitoring nothing. The scrape path re-checks
the kind anyway — defense in depth costs one comparison.

### Delta semantics and thread safety

`Scrape` returns two numbers: the delta since the previous scrape and the
cumulative total. The constructor primes the baseline with an initial read so
the first `Scrape` measures "since the collector was created" rather than "since
process start" — otherwise the first data point on a long-running process would
be a meaningless spike. A mutex serializes `Scrape` because two concurrent
scrapers (say, two Prometheus servers hitting the same endpoint) would otherwise
race on `last` and split one delta between them arbitrarily.

`Burst` is the load generator: `goroutines` workers each perform `ops` locked
increments holding the lock across `busyWork(spin)` iterations of non-elidable
arithmetic — the same sink/guard pattern as the earlier modules, because if the
compiler deleted the work the lock would be held for no time and the gauge would
have nothing to measure. It returns the final counter so tests can assert exact
correctness under `-race`.

Create `collector.go`:

```go
package lockwait

import (
	"errors"
	"fmt"
	"runtime/metrics"
	"sync"
)

// MetricName is the cumulative lock-wait counter this package watches.
const MetricName = "/sync/mutex/wait/total:seconds"

// ErrUnknownMetric reports that the runtime does not export the requested
// metric with the expected kind. Failing construction loudly beats a dashboard
// that silently reads zero forever.
var ErrUnknownMetric = errors.New("lockwait: metric not exported by this runtime as float64")

// Collector scrapes one cumulative float64 runtime metric and reports the
// delta between scrapes — the rate signal an SRE alerts on.
type Collector struct {
	mu   sync.Mutex
	name string
	last float64
}

// NewCollector validates name against metrics.All (existence and kind) and
// primes the baseline so the first Scrape measures from construction.
func NewCollector(name string) (*Collector, error) {
	ok := false
	for _, d := range metrics.All() {
		if d.Name == name && d.Kind == metrics.KindFloat64 {
			ok = true
			break
		}
	}
	if !ok {
		return nil, fmt.Errorf("collector for %q: %w", name, ErrUnknownMetric)
	}
	c := &Collector{name: name}
	v, err := c.read()
	if err != nil {
		return nil, err
	}
	c.last = v
	return c, nil
}

// read fetches the current cumulative value, re-checking the kind: a KindBad
// reading must never be mistaken for zero.
func (c *Collector) read() (float64, error) {
	sample := []metrics.Sample{{Name: c.name}}
	metrics.Read(sample)
	if kind := sample[0].Value.Kind(); kind != metrics.KindFloat64 {
		return 0, fmt.Errorf("collector for %q: got kind %v: %w", c.name, kind, ErrUnknownMetric)
	}
	return sample[0].Value.Float64(), nil
}

// Scrape returns the lock-wait seconds accumulated since the previous Scrape
// (or since construction) and the cumulative total.
func (c *Collector) Scrape() (delta, total float64, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, err := c.read()
	if err != nil {
		return 0, 0, err
	}
	delta = v - c.last
	c.last = v
	return delta, v, nil
}
```

Create `burst.go`:

```go
package lockwait

import "sync"

// Burst drives goroutines workers through ops locked increments each, holding
// one sync.Mutex across busyWork(spin) so real wait time accumulates. It
// returns the final count so callers can assert exact correctness.
func Burst(goroutines, ops, spin int) int {
	var (
		mu    sync.Mutex
		count int
		wg    sync.WaitGroup
	)
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range ops {
				mu.Lock()
				count++
				guard(busyWork(spin))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return count
}

// busyWork is the non-elidable critical-section stand-in: its result is
// returned and observed via guard, so the compiler cannot delete the loop.
func busyWork(iterations int) uint64 {
	var acc uint64
	for i := range iterations {
		acc += uint64(i)*2 + 1
	}
	return acc
}

var sink uint64

func guard(v uint64) {
	if v == 1<<63 {
		sink = v
	}
}
```

### The runnable demo

The demo scrapes once to settle the baseline, runs a contention burst, and
scrapes again. Absolute seconds vary by machine, so the demo prints the stable
facts: the burst applied every increment, the delta across the burst is
positive, and the cumulative total never decreases.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	lockwait "example.com/lockwait-gauge"
)

func main() {
	c, err := lockwait.NewCollector(lockwait.MetricName)
	if err != nil {
		fmt.Println("collector:", err)
		return
	}

	if _, _, err := c.Scrape(); err != nil { // settle the baseline
		fmt.Println("scrape:", err)
		return
	}

	n := lockwait.Burst(16, 3000, 400)
	delta1, total1, err := c.Scrape()
	if err != nil {
		fmt.Println("scrape:", err)
		return
	}

	lockwait.Burst(16, 3000, 400)
	delta2, total2, err := c.Scrape()
	if err != nil {
		fmt.Println("scrape:", err)
		return
	}

	fmt.Printf("burst increments applied: %d\n", n)
	fmt.Printf("wait accumulated during burst: %v\n", delta1 > 0)
	fmt.Printf("cumulative total monotonic: %v\n", total2 >= total1 && delta2 >= 0)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
burst increments applied: 48000
wait accumulated during burst: true
cumulative total monotonic: true
```

### Tests

`TestNewCollector` is the table: the real metric constructs; an invented name is
rejected with `ErrUnknownMetric`; and — the subtle row — a metric that *exists*
but with the wrong kind (`/gc/cycles/total:gc-cycles` is `KindUint64`) is also
rejected, proving the guard checks kind and not just existence.
`TestBurstAccumulatesWait` is the integration test: scrape, burst, scrape, and
assert the delta is strictly positive and the total monotonic. It asserts
*correctness and direction*, never magnitudes or wall time — sample magnitudes
depend on the machine and would flake on shared CI. It is not parallel because
the metric is process-global; other tests' contention could only add wait, but
keeping it serial makes the delta attribution honest.

Create `collector_test.go`:

```go
package lockwait

import (
	"errors"
	"fmt"
	"testing"
)

func TestNewCollector(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		metric  string
		wantErr bool
	}{
		{name: "real float64 metric", metric: MetricName, wantErr: false},
		{name: "unknown metric", metric: "/does/not/exist:seconds", wantErr: true},
		{name: "existing metric of wrong kind", metric: "/gc/cycles/total:gc-cycles", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, err := NewCollector(tt.metric)
			if tt.wantErr {
				if !errors.Is(err, ErrUnknownMetric) {
					t.Fatalf("NewCollector(%q) error = %v, want errors.Is(..., ErrUnknownMetric)", tt.metric, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewCollector(%q) unexpected error: %v", tt.metric, err)
			}
			if c == nil {
				t.Fatal("NewCollector returned nil collector without error")
			}
		})
	}
}

func TestBurstAccumulatesWait(t *testing.T) {
	// Process-global metric: keep serial so the delta is attributable.
	c, err := NewCollector(MetricName)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Scrape(); err != nil {
		t.Fatal(err)
	}

	const goroutines, ops, spin = 16, 3000, 400
	if got := Burst(goroutines, ops, spin); got != goroutines*ops {
		t.Fatalf("Burst count = %d, want %d", got, goroutines*ops)
	}

	delta1, total1, err := c.Scrape()
	if err != nil {
		t.Fatal(err)
	}
	if delta1 <= 0 {
		t.Fatalf("delta across burst = %v, want > 0", delta1)
	}

	Burst(goroutines, ops, spin)
	delta2, total2, err := c.Scrape()
	if err != nil {
		t.Fatal(err)
	}
	if delta2 <= 0 {
		t.Fatalf("delta across second burst = %v, want > 0", delta2)
	}
	if total2 < total1 {
		t.Fatalf("cumulative total decreased: %v -> %v", total1, total2)
	}
}

func TestBusyWorkArithmetic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		iterations int
		want       uint64
	}{
		{iterations: 0, want: 0},
		{iterations: 1, want: 1},
		{iterations: 3, want: 9},  // 1 + 3 + 5
		{iterations: 5, want: 25}, // sum of first 5 odd numbers
	}
	for _, tt := range tests {
		if got := busyWork(tt.iterations); got != tt.want {
			t.Fatalf("busyWork(%d) = %d, want %d", tt.iterations, got, tt.want)
		}
	}
}

func ExampleNewCollector() {
	_, err := NewCollector("/does/not/exist:seconds")
	fmt.Println(errors.Is(err, ErrUnknownMetric))
	// Output: true
}
```

## Review

The collector is correct when a bad metric name can never masquerade as a healthy
zero: `NewCollector` fails construction for a missing or wrong-kind name, and the
scrape path re-checks `Value.Kind()` so even a runtime that changed underneath a
running binary cannot feed the dashboard garbage. The integration test's shape is
the other lesson: it asserts the delta is positive and the total monotonic —
directional facts guaranteed by the counter's semantics — and deliberately never
asserts how many seconds accumulated, because magnitudes belong to the machine,
not the contract. The mistakes to avoid: reading `Value.Float64()` without the
kind check (a `KindBad` value panics on accessor misuse and reads as garbage
semantics either way); exporting the raw cumulative total to a dashboard instead
of the per-scrape delta (a counter always grows — only its slope means anything);
and scraping from multiple goroutines without serialization, which splits one
interval's delta arbitrarily between scrapers. Wire `Scrape` into your metrics
endpoint and alert on the rate; when it rises, the previous and next modules are
what you reach for.

## Resources

- [runtime/metrics](https://pkg.go.dev/runtime/metrics) — `All`, `Read`, `Sample`, `Value.Kind`, and the metric-name catalog including `/sync/mutex/wait/total:seconds`.
- [runtime/metrics.Value](https://pkg.go.dev/runtime/metrics#Value) — `KindFloat64`, `KindBad`, and the accessor contract.
- [Diagnostics (official Go documentation)](https://go.dev/doc/diagnostics) — runtime statistics and where they sit relative to profiling.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-http-pprof-debug-endpoint.md](06-http-pprof-debug-endpoint.md) | Next: [08-cpu-vs-mutex-triage.md](08-cpu-vs-mutex-triage.md)
