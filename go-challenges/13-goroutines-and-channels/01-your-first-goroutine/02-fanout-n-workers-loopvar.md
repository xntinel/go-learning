# Exercise 2: Fan Out N Parallel Workers Over an Index Range

The next primitive launches `n` goroutines, gives each its own index, and waits
for all of them. The real framing is dispatching N parallel downstream health
probes, one per configured dependency: probe every dependency at once, then
return only after all probes have reported.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
healthprobe/                 independent module: example.com/healthprobe
  go.mod
  fanout.go                  FanOutN(n int, work func(i int)) — launch n, join all
  cmd/
    demo/
      main.go                probe N dependencies in parallel
  fanout_test.go             all-ran + unique-index contract, under -race with n=100
```

Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
Implement: `FanOutN(n int, work func(i int))` that launches `n` goroutines each receiving its own index and returns after all complete.
Test: an `atomic.Int64` equal to `n` proves every worker ran; a `[]atomic.Bool` indexed by `i` proves each index `0..n-1` appears exactly once (no duplicate, no missing).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/healthprobe/cmd/demo
cd ~/go-exercises/healthprobe
go mod init example.com/healthprobe
```

### The per-index capture contract

`FanOutN` must guarantee two things: every one of the `n` workers runs, and each
worker sees a *distinct* index in `0..n-1`. The second guarantee is where the
historical loop-variable bug lived. Before Go 1.22, `for i := 0; i < n; i++ { go
func() { work(i) }() }` shared one `i` across all closures, so most workers
observed the final value `n` and some indices never ran at all — a silent,
schedule-dependent corruption. Since Go 1.22 the loop variable is per-iteration,
so `for i := range n { go func() { work(i) }() }` is correct: each iteration's
closure captures its own `i`.

The modern idiom here is `for i := range n`, which iterates `i` over `0..n-1`.
`Add(n)` is called once, up front, before any goroutine launches — that keeps the
`Wait`-observes-`Add` ordering correct. Each goroutine does `defer wg.Done()` so
the count is decremented on every exit path. The unique-index test is the proof
that the capture is per-iteration: it records `seen[i]` from inside each worker
and then asserts every slot from `0` to `n-1` was set exactly once. If capture
were shared, some slots would be false (missing) and the count would not reach
`n`.

Create `fanout.go`:

```go
package fanout

import "sync"

// FanOutN launches n goroutines, passing each its own index in 0..n-1, and
// returns only after all n have completed. The real use is dispatching one
// parallel task per configured item — for example a health probe per
// dependency — and joining before reporting an aggregate result.
func FanOutN(n int, work func(i int)) {
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			work(i)
		}()
	}
	wg.Wait()
}
```

### The runnable demo

The demo probes three dependencies in parallel and records each result in its own
slice slot (disjoint indices, so no shared write). `FanOutN` returns only once
every probe is done, so the summary is complete.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/healthprobe"
)

func main() {
	deps := []string{"postgres", "redis", "s3"}
	healthy := make([]bool, len(deps))

	fanout.FanOutN(len(deps), func(i int) {
		// Probe deps[i]; write only slot i, so no synchronization is needed.
		healthy[i] = true
	})

	for i, name := range deps {
		fmt.Printf("%s healthy=%t\n", name, healthy[i])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
postgres healthy=true
redis healthy=true
s3 healthy=true
```

### Tests

`TestFanOutNRunsAllWorkers` asserts the `atomic.Int64` counter equals `n`.
`TestFanOutNIndicesAreUnique` is the contract test: it runs `FanOutN(100, ...)`,
records `seen[i]` from each worker into a `[]atomic.Bool`, and asserts every index
`0..99` appears exactly once. `atomic.Bool` per slot keeps the recording race-free
even though workers run concurrently; the disjoint indices mean there is no
contention, but the `-race` run still proves it.

Create `fanout_test.go`:

```go
package fanout

import (
	"sync/atomic"
	"testing"
)

func TestFanOutNRunsAllWorkers(t *testing.T) {
	t.Parallel()

	const n = 100
	var counter atomic.Int64
	FanOutN(n, func(i int) {
		counter.Add(1)
	})
	if got := counter.Load(); got != n {
		t.Fatalf("counter = %d, want %d", got, n)
	}
}

func TestFanOutNIndicesAreUnique(t *testing.T) {
	t.Parallel()

	const n = 100
	seen := make([]atomic.Bool, n)
	var duplicates atomic.Int64
	FanOutN(n, func(i int) {
		if seen[i].Swap(true) {
			duplicates.Add(1) // this index was already recorded
		}
	})

	if got := duplicates.Load(); got != 0 {
		t.Fatalf("duplicate indices observed = %d, want 0", got)
	}
	for i := range n {
		if !seen[i].Load() {
			t.Fatalf("index %d never ran", i)
		}
	}
}

func TestFanOutNZeroIsANoOp(t *testing.T) {
	t.Parallel()

	var counter atomic.Int64
	FanOutN(0, func(i int) {
		counter.Add(1)
	})
	if got := counter.Load(); got != 0 {
		t.Fatalf("counter = %d, want 0", got)
	}
}
```

## Review

`FanOutN` is correct when the counter reaches `n` and every index `0..n-1` is
observed exactly once. The unique-index test is the one that actually pins the
loop-variable semantics; a regression to a shared `i` (for example reintroducing
a variable declared outside the loop and mutated) would surface as missing indices
and a duplicate count above zero under `-race`. Keep `Add(n)` up front, not inside
the loop or the goroutine, and prefer `for i := range n` over a C-style loop.
Note the zero case: `FanOutN(0, ...)` must be a clean no-op, which `Add(0)` plus
an empty loop gives you for free.

## Resources

- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [Go 1.22 loop variable scoping change](https://go.dev/blog/loopvar-preview)
- [Effective Go: Goroutines](https://go.dev/doc/effective_go#goroutines)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-fanout-single-background-task.md](01-fanout-single-background-task.md) | Next: [03-waitgroup-go-modern-idiom.md](03-waitgroup-go-modern-idiom.md)
