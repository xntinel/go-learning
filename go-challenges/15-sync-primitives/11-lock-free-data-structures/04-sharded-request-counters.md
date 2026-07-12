# Exercise 4: Sharded Atomic Counters for Hot-Path HTTP Metrics

A per-status-class request counter runs on every single request your server
handles. One `atomic.Int64` is correct — and becomes a contention point at high
parallelism, because every core fights over the same cache line. This exercise
builds the production fix: a striped counter with cache-line-padded shards,
wired into `net/http` middleware.

## What you'll build

```text
httpmetrics/                     independent module: example.com/httpmetrics
  go.mod
  counter.go                     paddedInt64 (64-byte slots); ShardedCounter: Add, Sum, Shards
  middleware.go                  RequestCounters; Middleware(next http.Handler); statusRecorder
  counter_test.go                exact-sum under concurrency, httptest middleware counts,
                                 single-vs-sharded contention benchmark, Example
  cmd/
    demo/
      main.go                    64 goroutines x 10000 increments; exact Sum
```

- Files: `counter.go`, `middleware.go`, `counter_test.go`, `cmd/demo/main.go`.
- Implement: `ShardedCounter` (power-of-two shard count sized from `runtime.NumCPU`, each shard padded to 64 bytes, shard picked with `math/rand/v2`), and `RequestCounters` middleware counting responses by status class.
- Test: N goroutines x M increments sum to exactly N*M under `-race`; httptest requests land in the right class; `BenchmarkSingleCounter` vs `BenchmarkShardedCounter` under `b.RunParallel`.
- Verify: `go test -count=1 -race ./...` then `go test -bench=. ./...`

### False sharing, and why padding is not superstition

`atomic.Int64.Add` is a single hardware instruction, but the cache-coherence
protocol moves memory between cores in 64-byte lines. When eight cores hammer
one counter, the line holding it ping-pongs: each `Add` must acquire exclusive
ownership, invalidating every other core's copy. Worse, two *different* counters
that happen to share a line contend with each other even though no goroutine
ever touches both — false sharing.

The striped counter attacks both problems. `Add` picks one of N shards and
increments it, so concurrent writers usually touch different lines; `Sum` loads
all shards and adds them up. Each shard is a `paddedInt64` — an `atomic.Int64`
followed by 56 bytes of padding — so each slice element owns a full line and
neighbors cannot false-share. The trade is explicit: writes scale nearly
linearly with cores, while reads cost O(shards) and memory costs 64 bytes per
shard. For a metrics counter written on every request and read once per scrape
interval, that trade is exactly right.

Two design details deserve scrutiny. Shard count is `runtime.NumCPU()` rounded
up to a power of two, so shard selection is a mask (`h & mask`) instead of a
modulo. And shard *selection* uses `rand.Uint64()` from `math/rand/v2` — its
top-level functions read cheap per-thread state without any shared lock (unlike
the global source in the old `math/rand`), which makes it an honest userland
stand-in for "which CPU am I on", something Go deliberately does not expose.
Random spreading means a goroutine occasionally collides with another on the
same shard; that costs a little fairness, never correctness.

Correctness is the part worth saying precisely: every increment lands in
*exactly one* shard atomically, so once writers quiesce, `Sum` is exact. While
writers are in flight, `Sum` is a consistent-enough snapshot for monitoring but
is not linearizable — it may miss increments that complete during the loop.
Dashboards do not care. Control flow would; do not use it there.

Create `counter.go`:

```go
package httpmetrics

import (
	"math/rand/v2"
	"runtime"
	"sync/atomic"
)

// paddedInt64 occupies a full 64-byte cache line so adjacent shards
// never false-share.
type paddedInt64 struct {
	v atomic.Int64
	_ [56]byte
}

// ShardedCounter is a write-optimized counter: Add touches one shard,
// Sum reads all of them. The zero value is NOT ready to use; call
// NewShardedCounter.
type ShardedCounter struct {
	shards []paddedInt64
	mask   uint64
}

// NewShardedCounter sizes the counter to the machine: NumCPU rounded
// up to a power of two, so shard selection is a mask.
func NewShardedCounter() *ShardedCounter {
	n := 1
	for n < runtime.NumCPU() {
		n <<= 1
	}
	return &ShardedCounter{
		shards: make([]paddedInt64, n),
		mask:   uint64(n - 1),
	}
}

// Add adds delta to one shard. rand/v2's per-thread state makes the
// pick cheap and lock-free; random spreading approximates per-CPU
// striping without runtime support.
func (c *ShardedCounter) Add(delta int64) {
	c.shards[rand.Uint64()&c.mask].v.Add(delta)
}

// Sum returns the total across shards. Exact once writers quiesce;
// a monitoring-grade snapshot while they run.
func (c *ShardedCounter) Sum() int64 {
	var total int64
	for i := range c.shards {
		total += c.shards[i].v.Load()
	}
	return total
}

// Shards reports the shard count (for tests and capacity planning).
func (c *ShardedCounter) Shards() int {
	return len(c.shards)
}
```

### The middleware

HTTP middleware cannot see the status code a handler writes unless it interposes
on the `ResponseWriter`, so `statusRecorder` wraps it and captures the first
`WriteHeader`. A handler that never calls `WriteHeader` implicitly sends 200 on
first `Write`, so the recorder starts at `http.StatusOK`. Counters are bucketed
by status class (2xx, 3xx, 4xx, 5xx) — the cardinality a dashboard actually
alerts on.

Create `middleware.go`:

```go
package httpmetrics

import "net/http"

// RequestCounters counts completed requests by status class. Each
// class gets its own sharded counter so a 2xx flood and a 5xx storm
// do not contend with each other either.
type RequestCounters struct {
	byClass [6]*ShardedCounter // index = status / 100; 0 and 1 unused
}

func NewRequestCounters() *RequestCounters {
	rc := &RequestCounters{}
	for i := range rc.byClass {
		rc.byClass[i] = NewShardedCounter()
	}
	return rc
}

// Count returns the number of completed requests whose status was in
// the given class (2 counts 2xx, 5 counts 5xx).
func (rc *RequestCounters) Count(class int) int64 {
	if class < 0 || class >= len(rc.byClass) {
		return 0
	}
	return rc.byClass[class].Sum()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Middleware wraps next, recording one increment per completed
// request on the hot path — which is why the counter is sharded.
func (rc *RequestCounters) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if class := rec.status / 100; class >= 0 && class < len(rc.byClass) {
			rc.byClass[class].Add(1)
		}
	})
}
```

### Tests and the contention benchmark

`TestSumExact` is the conservation test: 32 goroutines x 5000 increments must
sum to exactly 160000 — sharding must never lose or double-count. The middleware
test drives real requests through `httptest.NewServer` and asserts per-class
counts, including a handler that never calls `WriteHeader` (the implicit-200
path). The benchmark pair increments a single `atomic.Int64` versus the sharded
counter under `b.RunParallel`; on a multicore machine the sharded version pulls
ahead as parallelism rises, and that gap *is* the cache-line story made visible.

Create `counter_test.go`:

```go
package httpmetrics

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSumExact(t *testing.T) {
	t.Parallel()

	const goroutines = 32
	const perGoroutine = 5000

	c := NewShardedCounter()
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				c.Add(1)
			}
		}()
	}
	wg.Wait()

	if got, want := c.Sum(), int64(goroutines*perGoroutine); got != want {
		t.Fatalf("Sum = %d, want %d", got, want)
	}
	if c.Shards() < 1 {
		t.Fatalf("Shards = %d, want >= 1", c.Shards())
	}
}

func TestMiddlewareCountsByClass(t *testing.T) {
	t.Parallel()

	rc := NewRequestCounters()
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok") // implicit 200: WriteHeader never called
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	srv := httptest.NewServer(rc.Middleware(mux))
	defer srv.Close()

	paths := []struct {
		path string
		hits int
	}{
		{"/ok", 3},
		{"/missing", 2},
		{"/boom", 1},
	}
	for _, p := range paths {
		for range p.hits {
			resp, err := http.Get(srv.URL + p.path)
			if err != nil {
				t.Fatalf("GET %s: %v", p.path, err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}

	tests := []struct {
		class int
		want  int64
	}{
		{2, 3},
		{4, 2},
		{5, 1},
		{3, 0},
	}
	for _, tc := range tests {
		if got := rc.Count(tc.class); got != tc.want {
			t.Errorf("Count(%dxx) = %d, want %d", tc.class, got, tc.want)
		}
	}
}

func BenchmarkSingleCounter(b *testing.B) {
	var c atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Add(1)
		}
	})
}

func BenchmarkShardedCounter(b *testing.B) {
	c := NewShardedCounter()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Add(1)
		}
	})
}

func ExampleShardedCounter() {
	c := NewShardedCounter()
	for range 5 {
		c.Add(2)
	}
	fmt.Println(c.Sum())
	// Output: 10
}
```

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/httpmetrics"
)

func main() {
	c := httpmetrics.NewShardedCounter()

	const goroutines = 64
	const perGoroutine = 10000

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				c.Add(1)
			}
		}()
	}
	wg.Wait()

	fmt.Printf("increments: %d\n", goroutines*perGoroutine)
	fmt.Printf("sum exact:  %v\n", c.Sum() == goroutines*perGoroutine)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
increments: 640000
sum exact:  true
```

Then run the benchmark and compare:

```bash
go test -bench=Counter -cpu=1,4,8 .
```

At `-cpu=1` the single counter usually wins slightly (no shard pick, no mask);
as cores climb, the sharded counter's lead grows. Record both curves — that
crossover is the empirical justification (or refutation) for shipping the
sharded version on your hardware.

## Review

Correctness here has one theorem: each `Add` lands atomically in exactly one
shard, so the quiesced `Sum` is exact — the conservation test enforces it. The
classic mistakes are structural. Dropping the padding compiles and passes every
test, then quietly loses the entire scalability win to false sharing — which is
why the benchmark, not the test suite, is the regression net for this type.
Sizing shards to a fixed constant instead of `runtime.NumCPU` either wastes
memory on small machines or under-shards big ones. In the middleware, forgetting
that a handler may never call `WriteHeader` (the implicit 200) miscounts the
happy path — the `/ok` handler in the test exists precisely to pin that. And
treat `Sum` mid-storm as what it is: a monitoring snapshot, not a linearizable
read.

## Resources

- [False sharing](https://en.wikipedia.org/wiki/False_sharing) — why unrelated counters in one cache line contend.
- [math/rand/v2](https://pkg.go.dev/math/rand/v2) — top-level functions with cheap per-thread state, no global lock.
- [net/http: Handler and HandlerFunc](https://pkg.go.dev/net/http#Handler) — the middleware contract this module wraps.
- [runtime.NumCPU](https://pkg.go.dev/runtime#NumCPU) — the shard-count input.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-stress-and-invariant-suite.md](03-stress-and-invariant-suite.md) | Next: [05-cas-token-bucket-limiter.md](05-cas-token-bucket-limiter.md)
