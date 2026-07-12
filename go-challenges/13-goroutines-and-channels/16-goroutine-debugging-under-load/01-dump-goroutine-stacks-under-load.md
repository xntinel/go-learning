# Exercise 1: Capture a full goroutine stack dump from a running worker service

The first move when a concurrent service misbehaves is to capture the state of
every goroutine. This exercise builds a small worker service and the `Dump`
helper an on-call engineer calls to get a complete, service-wide goroutine dump —
the artifact that tells you the *shape* of a bug before you have guessed anything.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
gdebug/                          independent module: example.com/gdebug
  go.mod
  internal/service/service.go    Service (WaitGroup + ticker workers + cancel); Dump; NumGoroutines
  cmd/demo/main.go               start workers, dump, show the dump names the worker frame
  internal/service/service_test.go  dump-content + blocked-goroutine + Wait-after-cancel tests
```

- Files: `internal/service/service.go`, `cmd/demo/main.go`, `internal/service/service_test.go`.
- Implement: a `Service` that starts `n` ticker workers under a context, a `Dump()` that returns a complete goroutine dump via `runtime.Stack(buf, true)` with a growing buffer, and a `StartBlocked` that parks workers on a channel receive.
- Test: assert the dump contains `goroutine `, `service.worker`, and (for blocked workers) `chan receive`; assert `Wait` returns after cancel.
- Verify: `go test -count=1 -race ./...`

### Why the dump is the artifact

A goroutine dump is the concurrency equivalent of a core dump: one call freezes the
runtime, walks every goroutine's stack, and hands you a text snapshot of what the
whole program is doing at that instant. `runtime.Stack(buf []byte, all bool) int`
writes into `buf` and returns the number of bytes written. Two decisions make it
reliable.

First, `all` must be `true`. With `false` you get only the goroutine that called
`Dump`, which is never the stuck one. With `true` you get every goroutine, each
with its state (`chan receive`, `select`, `running`, ...) and full stack — that
state line is the whole point of taking the dump.

Second, the buffer must be large enough. `runtime.Stack` truncates silently: if the
dump is bigger than `buf`, it fills `buf` and returns `len(buf)` with no error, and
the frames it drops are the deep ones you needed. `Dump` therefore grows the buffer
and retries until the returned length is strictly less than the buffer size, which
is the only signal that the whole dump fit.

The `Service` itself is deliberately ordinary: a `sync.WaitGroup` and `n` workers,
each looping on a `time.Ticker` until its context is cancelled. That is the shape of
a real background pool, and it gives the dump something real to show — several
`service.worker` frames parked in a `select`. `StartBlocked` provides the contrast:
workers blocked on a receive from an unbuffered channel, which the dump reports as
`chan receive`, so a test can assert that the dump distinguishes *what* each
goroutine is waiting on.

Create `internal/service/service.go`:

```go
package service

import (
	"context"
	"runtime"
	"sync"
	"time"
)

// Service runs a pool of background workers under a cancellable context.
type Service struct {
	wg sync.WaitGroup
}

// New returns an idle Service.
func New() *Service {
	return &Service{}
}

// Start launches n ticker workers that run until ctx is cancelled.
func (s *Service) Start(ctx context.Context, n int) {
	for range n {
		s.wg.Add(1)
		go worker(ctx, &s.wg)
	}
}

// worker is a package-level function so it appears as service.worker in a dump.
func worker(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// StartBlocked launches n workers that block on a receive from ch, so a dump
// reports them as "chan receive". Closing ch releases them.
func (s *Service) StartBlocked(n int, ch <-chan struct{}) {
	for range n {
		s.wg.Add(1)
		go blockedWorker(ch, &s.wg)
	}
}

func blockedWorker(ch <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	<-ch
}

// Wait blocks until every worker has returned.
func (s *Service) Wait() {
	s.wg.Wait()
}

// NumGoroutines reports the number of goroutines that currently exist.
func (s *Service) NumGoroutines() int {
	return runtime.NumGoroutine()
}

// Dump returns a complete goroutine stack dump for every goroutine. It grows the
// buffer until runtime.Stack no longer truncates (returns fewer bytes than the
// buffer holds), so no frames are lost under load.
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
```

### The runnable demo

The demo starts four workers, takes a dump, and prints two derived facts rather
than the raw (non-deterministic) dump text: that the dump carries the standard
`goroutine ` header and that it names the `service.worker` frame. Then it cancels
and waits so the process exits cleanly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"example.com/gdebug/internal/service"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	s := service.New()
	s.Start(ctx, 4)
	time.Sleep(50 * time.Millisecond)

	dump := service.Dump()
	fmt.Println("has goroutine header:", strings.Contains(dump, "goroutine "))
	fmt.Println("has worker frame:", strings.Contains(dump, "service.worker"))

	cancel()
	s.Wait()
	fmt.Println("stopped")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
has goroutine header: true
has worker frame: true
stopped
```

### Tests

The tests pin the dump contract. `TestDumpContainsGoroutines` proves the dump has
the standard header. `TestDumpContainsWorkerFunction` proves it reaches into the
worker's stack. `TestDumpShowsBlockedGoroutines` parks workers on an unbuffered
channel and asserts the dump reports `chan receive` — the property that a dump
tells you *what* each goroutine waits on. `TestWaitBlocksUntilDone` guards that
`Wait` returns only after cancellation propagates. Everything runs under `-race`.

Create `internal/service/service_test.go`:

```go
package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestStartAndStop(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	s := New()
	s.Start(ctx, 5)
	time.Sleep(20 * time.Millisecond)
	cancel()
	s.Wait()
}

func TestDumpContainsGoroutines(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New()
	s.Start(ctx, 3)
	time.Sleep(20 * time.Millisecond)

	if dump := Dump(); !strings.Contains(dump, "goroutine ") {
		t.Fatal("dump should contain 'goroutine '")
	}
	cancel()
	s.Wait()
}

func TestDumpContainsWorkerFunction(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New()
	s.Start(ctx, 3)
	time.Sleep(20 * time.Millisecond)

	if dump := Dump(); !strings.Contains(dump, "service.worker") {
		t.Fatalf("dump should contain 'service.worker'")
	}
	cancel()
	s.Wait()
}

func TestDumpShowsBlockedGoroutines(t *testing.T) {
	t.Parallel()
	ch := make(chan struct{}) // unbuffered: receivers block
	s := New()
	s.StartBlocked(5, ch)

	// Poll until the blocked workers have actually parked on the receive. We look
	// for the worker frame itself, not the generic "chan receive" state, because
	// unrelated runtime/test goroutines can also sit in "chan receive" before our
	// workers are scheduled.
	var dump string
	for range 200 {
		dump = Dump()
		if strings.Contains(dump, "service.blockedWorker") {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(dump, "service.blockedWorker") {
		t.Fatalf("dump should name the blocked worker function")
	}
	if !strings.Contains(dump, "chan receive") {
		t.Fatalf("dump should show blocked workers on 'chan receive'")
	}
	close(ch) // release the workers
	s.Wait()
}

func TestWaitBlocksUntilDone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	s := New()
	s.Start(ctx, 3)

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		s.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after cancel")
	}
}

func Example() {
	dump := Dump()
	fmt.Println(strings.Contains(dump, "goroutine "))
	// Output: true
}
```

## Review

The dump is correct when it is complete and service-wide. Completeness comes from
the growing-buffer loop: if you had used a fixed buffer and it truncated, the
`service.worker` and `chan receive` assertions would flake precisely under the load
that makes a dump worth taking. Service-wide comes from `all=true`; with `false`
the dump would show only the test goroutine and every content assertion would fail.
`TestDumpShowsBlockedGoroutines` is the load-bearing test: it proves the dump
reports the *state* a goroutine is parked in, which is the single most useful field
when you open a real dump. Because worker and blockedWorker are package-level
functions, their frames appear as `service.worker` and `service.blockedWorker`,
which the tests match literally. Run `go test -race` to confirm the shared
`WaitGroup` is used correctly under concurrent workers.

## Resources

- [`runtime.Stack`](https://pkg.go.dev/runtime#Stack) — writes the goroutine dump; the `all` parameter and the return-length contract.
- [`runtime.NumGoroutine`](https://pkg.go.dev/runtime#NumGoroutine) — the live goroutine count used for leak triage.
- [Diagnostics](https://go.dev/doc/diagnostics) — the Go team's overview of dumps, profiling, and tracing.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-expose-pprof-behind-admin-auth.md](02-expose-pprof-behind-admin-auth.md)
