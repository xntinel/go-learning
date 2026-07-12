# Exercise 7: Diagnose and fix a worker pool deadlocked on a full queue

This is a classic 03:00 stall: a bounded worker pool stops draining because every
worker is blocked forever on a send to a results channel whose reader has exited.
This exercise reproduces the wedge deterministically, takes a dump that proves it
(a wall of `chan send`), and builds the fixed pool whose sends are context-aware so
no worker can block forever.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
pool/                     independent module: example.com/pool
  go.mod
  pool.go                 Wedge (reproduce the stall) + Dump; Pool.Run (the fix)
  cmd/demo/main.go         show the wedge dump, then the fixed pool drains
  pool_test.go            wedge dump shows chan send; fixed pool drains and cancels cleanly
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: `Wedge(workers)` that parks workers on a bare send to an unbuffered channel with no reader and returns the dump plus a `release`; `Dump()`; a fixed `Pool` whose workers `select` on `ctx.Done()` while sending, with a consumer draining the results.
- Test: the wedge dump contains `chan send` and `pool.stuckWorker`; the fixed pool delivers every result and returns before a timeout; cancelling stops the workers.
- Verify: `go test -count=1 -race ./...`

### The wedge, and the shape it makes

A bounded pool produces results faster than they are consumed, and buffers them on a
channel. If the consumer exits — an error path returned early, the client
disconnected — the channel fills and every producer blocks on its send. Because a
bare channel send has no escape, the workers block *forever*: the pool is wedged, the
queue never drains, and the service's throughput on that path drops to zero with no
error logged.

`Wedge` reproduces this with the minimal ingredients: an unbuffered results channel
with no reader and `workers` goroutines each doing a bare `results <- id`. Every send
blocks immediately. To make the reproduction deterministic rather than sleep-based,
`Wedge` polls its own dump until it observes `chan send` — the goroutines are parked
on a real channel send, and `runtime.Stack` reports exactly that state. `release`
then drains the channel so the blocked sends complete and the workers exit, keeping
the test binary clean.

Why `chan send` and not `select`? A *bare* send that blocks reports the state
`chan send`; a send inside a `select` reports `select`. The wedge uses a bare send
precisely so the dump names the state unambiguously — that is the fingerprint an
on-call engineer greps for.

The fix is `Pool.Run`: workers process jobs and deliver results with a send wrapped
in `select { case out <- v: case <-ctx.Done(): return }`, and a dedicated goroutine
closes `out` once all workers finish. Now a consumer that stops (or a cancelled
context) unblocks the workers instead of wedging them — no send can outlive the
context.

Create `pool.go`:

```go
package pool

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Dump returns a complete goroutine stack dump, growing the buffer so nothing is
// truncated under load.
func Dump() string {
	size := 1 << 16
	for {
		buf := make([]byte, size)
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		size *= 2
	}
}

// Wedge reproduces a stalled pool: workers block forever on a bare send to an
// unbuffered channel that has no reader. It returns a dump taken once the workers
// are parked on the send, plus a release func that drains the channel so the
// goroutines can exit.
func Wedge(workers int) (dump string, release func()) {
	results := make(chan int) // unbuffered, no reader: sends block forever
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go stuckWorker(i, results, &wg)
	}

	dump = dumpWhenBlocked("chan send")
	release = func() {
		go func() {
			for range workers {
				<-results // drain so every blocked send completes
			}
		}()
		wg.Wait()
	}
	return dump, release
}

func stuckWorker(id int, results chan<- int, wg *sync.WaitGroup) {
	defer wg.Done()
	results <- id // bare send: dump reports "chan send"
}

// dumpWhenBlocked polls the goroutine dump until it mentions want, so the
// reproduction is deterministic rather than relying on a fixed sleep.
func dumpWhenBlocked(want string) string {
	for range 500 {
		if d := Dump(); strings.Contains(d, want) {
			return d
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	return Dump()
}

// Pool is the fixed pool: workers deliver results with a context-aware send, so a
// consumer that stops (or a cancelled context) never wedges them.
type Pool struct {
	workers int
}

func New(workers int) *Pool {
	return &Pool{workers: workers}
}

// Run processes jobs concurrently, doubling each, and sends results on out. It
// closes out when every worker has finished. A cancelled ctx stops workers
// instead of blocking them forever on a send.
func (p *Pool) Run(ctx context.Context, jobs []int, out chan<- int) {
	jobCh := make(chan int)
	var wg sync.WaitGroup

	for range p.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				select {
				case out <- j * 2:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobCh)
		for _, j := range jobs {
			select {
			case jobCh <- j:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(out)
	}()
}
```

### The runnable demo

The demo takes a wedge dump and prints that it shows `chan send` and names the stuck
worker, releases it, then runs the fixed pool over a few jobs and prints the summed
results (deterministic: each job is doubled).

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"

	"example.com/pool"
)

func main() {
	dump, release := pool.Wedge(4)
	fmt.Println("wedged on chan send:", strings.Contains(dump, "chan send"))
	fmt.Println("names stuckWorker:", strings.Contains(dump, "pool.stuckWorker"))
	release()

	out := make(chan int)
	pool.New(4).Run(context.Background(), []int{1, 2, 3}, out)
	sum := 0
	for v := range out {
		sum += v
	}
	fmt.Println("fixed pool sum:", sum)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wedged on chan send: true
names stuckWorker: true
fixed pool sum: 12
```

### Tests

`TestWedgeDumpShowsChanSend` proves the reproduction produces the diagnostic
fingerprint. `TestPoolDrains` proves the fixed pool delivers every result and returns
before a timeout. `TestPoolCancelStops` cancels the context and asserts the workers
stop and `out` closes, rather than wedging.

Create `pool_test.go`:

```go
package pool

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestWedgeDumpShowsChanSend(t *testing.T) {
	dump, release := Wedge(4)
	defer release()

	if !strings.Contains(dump, "chan send") {
		t.Errorf("wedge dump should show 'chan send'")
	}
	if !strings.Contains(dump, "pool.stuckWorker") {
		t.Errorf("wedge dump should name the stuck worker function")
	}
}

func TestPoolDrains(t *testing.T) {
	t.Parallel()
	out := make(chan int)
	New(4).Run(context.Background(), []int{1, 2, 3, 4, 5}, out)

	sum := 0
	done := make(chan struct{})
	go func() {
		for v := range out {
			sum += v
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool wedged: results channel never closed")
	}
	if want := 2 * (1 + 2 + 3 + 4 + 5); sum != want {
		t.Fatalf("sum = %d; want %d", sum, want)
	}
}

func TestPoolCancelStops(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan int)
	New(4).Run(ctx, []int{1, 2, 3, 4, 5, 6, 7, 8}, out)
	cancel()

	done := make(chan struct{})
	go func() {
		for range out {
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("workers did not stop after cancel")
	}
}
```

## Review

The diagnosis is correct when the wedge dump names both the *state* (`chan send`) and
the *site* (`pool.stuckWorker`) — together they tell an on-call engineer "producers
are blocked here, the consumer is gone." The fix is correct when the pool always makes
progress: `TestPoolDrains` confirms every result is delivered and the channel closes,
and `TestPoolCancelStops` confirms a cancelled context unblocks the workers instead of
wedging them. The essential difference between the two pools is a single `select`: the
broken worker's bare `results <- id` has no escape, while the fixed worker's send
races `ctx.Done()`. The `close(out)`-after-`wg.Wait()` goroutine guarantees no worker
sends after the channel closes, so the drain in the tests is race-free. Run `go test
-race` to confirm.

## Resources

- [`runtime.Stack`](https://pkg.go.dev/runtime#Stack) — the dump whose `chan send` state fingerprints the wedge.
- [Go statement / select](https://go.dev/ref/spec#Select_statements) — the language rule that makes a `select` send cancellable.
- [`context.Context`](https://pkg.go.dev/context#Context) — the cancellation the fixed worker watches while sending.

---

Prev: [06-mutex-profile-lock-contention.md](06-mutex-profile-lock-contention.md) | Back to [00-concepts.md](00-concepts.md) | Next: [08-goroutine-count-regression-watchdog.md](08-goroutine-count-regression-watchdog.md)
