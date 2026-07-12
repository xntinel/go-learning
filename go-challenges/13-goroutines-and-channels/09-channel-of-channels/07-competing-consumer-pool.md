# Exercise 7: Scaling the Actor: One Inbox, Many Workers

The single-goroutine actor is the right design when a component owns mutable
state. But a *stateless* request handler — one that computes its answer from the
request alone — has no such constraint, and pinning it to one goroutine caps
throughput needlessly. This exercise scales the service to a competing-consumer
pool: `W` workers all reading the same inbox, each answering on the request's own
reply channel, so per-request reply semantics survive the fan-out.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
pool/                      independent module: example.com/pool
  go.mod
  pool.go                  type Service; W workers share one inbox channel
  cmd/
    demo/
      main.go              runnable demo: pool answers a batch of calls
  pool_test.go             all-served-once, throughput-improves, race tests
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: a `Service` that starts `W` worker goroutines all receiving from one shared inbox; `Call(n)` sends a request with its own reply channel and waits; `Shutdown` closes `quit` and joins the pool with a `WaitGroup`.
- Test: submit many distinct requests, assert each is answered exactly once with the right value; assert a multi-worker pool finishes a sleep-bound batch faster than a single worker; all under `-race`.
- Verify: `go test -count=1 -race ./...`

### Competing consumers on a shared channel

`W` worker goroutines all execute `for { select { case req := <-inbox: ...; case
<-quit: return } }` on the *same* `inbox` channel. A channel with multiple
receivers delivers each sent value to exactly one of them — the runtime picks a
ready receiver — so a request is handled by whichever worker is free. This is the
competing-consumer pattern: no dispatcher, no per-worker channels, just several
goroutines racing to receive from one queue. Throughput scales with `W` for a
workload bound by per-request latency (I/O waits, sleeps), because while one
worker is blocked on its request another is already handling the next.

Per-request reply semantics are untouched by the fan-out. Each request still
carries its own capacity-one reply channel, and the worker that handles the
request answers on that channel, so the caller always receives its own response.
Fanning out changes *who* handles a request, not *how* the answer gets back.

The hard rule this exercise exists to make concrete: fanning out is safe only
because the handler is stateless. `handle(n)` reads nothing but its argument and
immutable config; two workers running it concurrently cannot interfere. The moment
a handler mutates shared state — the allocator's counter from Exercise 1, a
shared map — running it on `W` goroutines reintroduces the data race the single
actor eliminated. Stateless handlers fan out; stateful handlers stay single-
goroutine or move to explicit synchronization.

Create `pool.go`:

```go
package pool

import (
	"errors"
	"sync"
	"time"
)

// ErrShuttingDown is returned once the pool has been shut down.
var ErrShuttingDown = errors.New("pool: shutting down")

type request struct {
	n     int
	reply chan response
}

type response struct {
	value int
}

// Service is a pool of stateless workers sharing one inbox. Because the handler
// is stateless, running it on many goroutines needs no synchronization.
type Service struct {
	inbox chan request
	work  time.Duration
	quit  chan struct{}
	wg    sync.WaitGroup
}

// New returns a started pool of the given size. work is the per-request latency.
func New(workers int, work time.Duration) *Service {
	s := &Service{
		inbox: make(chan request),
		work:  work,
		quit:  make(chan struct{}),
	}
	s.wg.Add(workers)
	for range workers {
		go s.worker()
	}
	return s
}

// handle is stateless: it reads only its argument. That is what makes fan-out safe.
func (s *Service) handle(n int) int {
	time.Sleep(s.work)
	return n * 2
}

func (s *Service) worker() {
	defer s.wg.Done()
	for {
		select {
		case req := <-s.inbox:
			req.reply <- response{value: s.handle(req.n)}
		case <-s.quit:
			return
		}
	}
}

// Shutdown stops the pool and waits for every worker to exit.
func (s *Service) Shutdown() {
	close(s.quit)
	s.wg.Wait()
}

// Call sends n and returns the doubled value, handled by whichever worker is free.
func (s *Service) Call(n int) (int, error) {
	reply := make(chan response, 1)
	select {
	case s.inbox <- request{n: n, reply: reply}:
	case <-s.quit:
		return 0, ErrShuttingDown
	}
	select {
	case resp := <-reply:
		return resp.value, nil
	case <-s.quit:
		return 0, ErrShuttingDown
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"time"

	"example.com/pool"
)

func main() {
	s := pool.New(4, 5*time.Millisecond)
	defer s.Shutdown()

	const n = 6
	results := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _ := s.Call(i)
			results[i] = v
		}()
	}
	wg.Wait()

	fmt.Println(results)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[0 2 4 6 8 10]
```

### Tests

`TestAllServedOnce` submits many distinct values concurrently and asserts each
comes back doubled, exactly once, by checking index alignment. `TestThroughput`
runs a fixed sleep-bound batch through a single-worker pool and an eight-worker
pool and asserts the pool finishes faster — the point of fanning out. The
workload is latency-bound (each request sleeps), so parallelism helps by a wide
margin, which keeps the timing assertion robust.

Create `pool_test.go`:

```go
package pool

import (
	"sync"
	"testing"
	"time"
)

func TestAllServedOnce(t *testing.T) {
	t.Parallel()
	const n = 200
	s := New(8, 0)
	defer s.Shutdown()

	results := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.Call(i)
			if err != nil {
				t.Errorf("Call(%d) error = %v", i, err)
				return
			}
			results[i] = v
		}()
	}
	wg.Wait()

	for i := range n {
		if results[i] != i*2 {
			t.Fatalf("result[%d] = %d, want %d", i, results[i], i*2)
		}
	}
}

func runBatch(workers int, jobs int, work time.Duration) time.Duration {
	s := New(workers, work)
	defer s.Shutdown()

	start := time.Now()
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Call(i)
		}()
	}
	wg.Wait()
	return time.Since(start)
}

func TestThroughput(t *testing.T) {
	t.Parallel()
	const jobs = 16
	const work = 10 * time.Millisecond

	single := runBatch(1, jobs, work)
	many := runBatch(8, jobs, work)

	if many >= single {
		t.Fatalf("pool not faster: 8 workers took %v, 1 worker took %v", many, single)
	}
}
```

## Review

The pool is correct when every request is answered exactly once with the right
value, which `TestAllServedOnce` checks across 200 concurrent calls by index
alignment — a duplicate or a dropped answer would corrupt the slice. `TestThroughput`
proves the reason to fan out: a latency-bound batch that takes about `jobs * work`
on one worker takes roughly `jobs/W * work` on `W` workers, so eight workers
finish a 16-job batch far faster than one. The reply channel travelling with each
request is what preserves per-request semantics through the fan-out.

The mistake this exercise is built to prevent: fanning out a *stateful* handler.
`handle` here is a pure function of its argument, so `W` workers are safe. Replace
it with something that mutates a shared counter or map, and `go test -race` will
report the very race the single-goroutine actor was designed to avoid — the fix
then is not more workers but keeping the stateful part on one goroutine. Run
`go test -race` to confirm the shared-inbox handoff is race-free.

## Resources

- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) — worker pools over a shared channel.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — multiple receivers on one channel.
- [Go Memory Model](https://go.dev/ref/mem) — why a stateless handler is safe to run concurrently and a stateful one is not.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — joining the worker pool at shutdown.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-batch-fan-out-collect.md](08-batch-fan-out-collect.md)
