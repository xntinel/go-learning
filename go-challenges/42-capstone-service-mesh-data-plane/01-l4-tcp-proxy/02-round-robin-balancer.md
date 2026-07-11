# Exercise 2: Round-Robin Balancer

The proxy in the previous exercise embeds a tiny round-robin balancer to spread connections across upstreams. It looks trivial, but the obvious "optimization" — an atomic counter instead of a mutex — is a real concurrency bug. This exercise lifts the balancer into a standalone module and proves, under the race detector, that the mutex version distributes selections exactly evenly while the atomic version would drift.

This module is fully self-contained: its own `go mod init`, the balancer defined inline, its own demo and tests. Nothing here imports another exercise; it mirrors the `roundRobin` helper the proxy uses internally.

## What you'll build

```text
balancer.go           RoundRobin, New, Next, ErrNoBackends
cmd/
  demo/
    main.go           seven selections over three backends, deterministic order
balancer_test.go      empty rejection, exact order, even distribution, race-safe
                      concurrency with exact per-backend counts
```

- Files: `balancer.go`, `cmd/demo/main.go`, `balancer_test.go`.
- Implement: `RoundRobin` with `New(addrs ...string) (*RoundRobin, error)` and `Next() string`; reject an empty backend list with `ErrNoBackends`.
- Test: empty-list rejection, exact rotation order, even distribution over a whole number of cycles, and a `-race` concurrency test that asserts exact per-backend counts so a lost increment would fail.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p round-robin-balancer/cmd/demo && cd round-robin-balancer
go mod init example.com/round-robin-balancer
go mod edit -go=1.26
```

### Why a mutex, not an atomic counter

Round-robin selection is a read-compute-increment: read the index, compute `idx % len(addrs)`, increment the index. The three steps must be one indivisible operation. The tempting atomic version reads the index with one atomic load, computes the modulo, then stores `idx+1` with a separate atomic store. Between the load and the store, a second goroutine can run the same load, read the same `idx`, compute the same backend, and store the same `idx+1`. Two callers get the same backend and the counter advances by one instead of two. Over many concurrent calls the distribution skews and the counter drifts arbitrarily. This is a classic check-then-act (here read-then-act) race that atomics do not fix, because the atomicity has to span all three steps, not each one in isolation.

A mutex held across the whole sequence makes it indivisible. The critical section is three instructions — a slice index, an integer increment, a modulo — and the lock is released before the caller does anything with the result. Compared to the TCP dial that follows each selection in the real proxy, the mutex cost is invisible. Holding a copy of the address slice in the balancer (rather than aliasing the caller's slice) keeps the rotation stable even if the caller mutates its own slice afterward.

Create `balancer.go`:

```go
package balancer

import (
	"errors"
	"sync"
)

// ErrNoBackends is returned by New when no backend addresses are provided.
var ErrNoBackends = errors.New("balancer: no backend addresses configured")

// RoundRobin distributes selections across a fixed set of backend addresses in
// rotation. The index is guarded by a mutex because the read-compute-increment
// must be one indivisible step; an atomic load/store pair is a check-then-act
// race that lets two goroutines select the same backend.
type RoundRobin struct {
	addrs []string
	mu    sync.Mutex
	idx   int
}

// New creates a RoundRobin over addrs. At least one backend is required.
// The slice is copied so later mutations by the caller do not affect rotation.
func New(addrs ...string) (*RoundRobin, error) {
	if len(addrs) == 0 {
		return nil, ErrNoBackends
	}
	cp := make([]string, len(addrs))
	copy(cp, addrs)
	return &RoundRobin{addrs: cp}, nil
}

// Next returns the next backend address in rotation. It is safe for concurrent
// use by multiple goroutines.
func (rr *RoundRobin) Next() string {
	rr.mu.Lock()
	addr := rr.addrs[rr.idx%len(rr.addrs)]
	rr.idx++
	rr.mu.Unlock()
	return addr
}
```

`Next` is the entire balancer. Because the lock spans the read, the modulo, and the increment, every call observes a unique `idx` value, so the global sequence of returned backends is exactly `addrs[0], addrs[1], ..., addrs[n-1], addrs[0], ...` no matter how many goroutines call concurrently. That property is what the concurrency test pins.

### The runnable demo

The demo selects seven times over three backends and prints the rotation. With three backends, seven selections cycle through twice and then start a third pass, so the first backend appears three times and the other two appear twice. The output is fully deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/round-robin-balancer"
)

func main() {
	rr, err := balancer.New("10.0.0.1:8080", "10.0.0.2:8080", "10.0.0.3:8080")
	if err != nil {
		log.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		fmt.Printf("request %d -> %s\n", i, rr.Next())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request 0 -> 10.0.0.1:8080
request 1 -> 10.0.0.2:8080
request 2 -> 10.0.0.3:8080
request 3 -> 10.0.0.1:8080
request 4 -> 10.0.0.2:8080
request 5 -> 10.0.0.3:8080
request 6 -> 10.0.0.1:8080
```

### Tests

The tests pin four properties: an empty backend list is rejected with `ErrNoBackends`, the rotation order is exactly the input order repeated, a whole number of cycles distributes selections evenly, and — the one that matters most — fifty goroutines each calling `Next` a hundred times produce exactly the counts a correct sequential rotation would. With three backends and 5000 calls, the indices 0..4999 distribute as 1667, 1667, 1666 by their value modulo three; the mutex guarantees each call gets a unique index, so a lost or duplicated increment from the atomic-counter bug would change these exact counts and fail the test.

Create `balancer_test.go`:

```go
package balancer

import (
	"errors"
	"sync"
	"testing"
)

func TestNewRejectsEmpty(t *testing.T) {
	t.Parallel()

	if _, err := New(); !errors.Is(err, ErrNoBackends) {
		t.Fatalf("err = %v, want ErrNoBackends", err)
	}
}

func TestRoundRobinOrder(t *testing.T) {
	t.Parallel()

	rr, err := New("x", "y", "z")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"x", "y", "z", "x", "y", "z", "x"}
	for i, w := range want {
		if got := rr.Next(); got != w {
			t.Fatalf("Next() #%d = %q, want %q", i, got, w)
		}
	}
}

func TestRoundRobinDistributesEvenly(t *testing.T) {
	t.Parallel()

	rr, err := New("a:1", "b:2")
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]int)
	for i := 0; i < 6; i++ {
		got[rr.Next()]++
	}
	if got["a:1"] != 3 || got["b:2"] != 3 {
		t.Errorf("distribution = %v, want a:1=3 b:2=3", got)
	}
}

func TestRoundRobinConcurrentExactCounts(t *testing.T) {
	t.Parallel()

	rr, err := New("x:1", "y:2", "z:3")
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 50
	const perG = 100

	counts := make([]map[string]int, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			local := make(map[string]int)
			for i := 0; i < perG; i++ {
				local[rr.Next()]++
			}
			counts[g] = local
		}(g)
	}
	wg.Wait()

	total := make(map[string]int)
	for _, c := range counts {
		for k, v := range c {
			total[k] += v
		}
	}

	// 5000 calls over 3 backends: indices 0..4999 modulo 3 give 1667/1667/1666.
	// A lost increment (the atomic-counter race) would change these exactly.
	if total["x:1"] != 1667 || total["y:2"] != 1667 || total["z:3"] != 1666 {
		t.Errorf("distribution = %v, want x:1=1667 y:2=1667 z:3=1666", total)
	}
}
```

## Review

The balancer is correct when `Next` holds the mutex across the entire read-compute-increment and returns the input addresses in exact rotation. The bug this design exists to prevent is replacing the mutex with an atomic load and store: that lets two goroutines read the same index and select the same backend, so the distribution skews and the counter drifts. `TestRoundRobinConcurrentExactCounts` is the guard — its exact per-backend counts only hold if every one of the 5000 concurrent calls observed a unique index, which is precisely what the mutex guarantees and the atomic version breaks. Running it under `go test -race` adds the proof that there is no data race on `idx` at all. The order and even-distribution tests confirm the single-threaded behavior is a clean rotation, and the empty-list test confirms a misconfiguration fails at construction rather than panicking on the first `Next`.

## Resources

- [pkg.go.dev/sync#Mutex](https://pkg.go.dev/sync#Mutex) — the mutual exclusion that makes the read-compute-increment indivisible.
- [The Go Memory Model](https://go.dev/ref/mem) — why a check-then-act over two separate atomic operations is still a race.
- [pkg.go.dev/sync/atomic](https://pkg.go.dev/sync/atomic) — the atomic primitives that are correct for a single counter but not for a multi-step read-compute-increment.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-l4-tcp-proxy.md](01-l4-tcp-proxy.md) | Next: [../02-l7-http-proxy/00-concepts.md](../02-l7-http-proxy/00-concepts.md)
