# Exercise 9: Emit a stack dump when graceful shutdown blows its deadline

"Why won't my service exit?" is the shutdown that hangs on `SIGTERM` with no clue
why. This exercise builds a bounded graceful-shutdown path: cancel the context, wait
for the pool with a timeout, and — if the wait times out — capture a full goroutine
dump so the on-call engineer sees exactly which goroutines refused to stop.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
shutdown/                 independent module: example.com/shutdown
  go.mod
  shutdown.go             Manager: StartWorkers, StartStuck, Shutdown (bounded, dumps on timeout)
  cmd/demo/main.go        fast path writes nothing; stuck path dumps the culprit
  shutdown_test.go        fast path returns nil + no dump; timeout returns error + names stuck worker
```

- Files: `shutdown.go`, `cmd/demo/main.go`, `shutdown_test.go`.
- Implement: a `Manager` with well-behaved workers (stop on `ctx.Done`) and a stuck worker (ignores context); `Shutdown(cancel, timeout, dumpTo)` that cancels, waits with a timeout, and on timeout writes `runtime.Stack(all=true)` to `dumpTo` and returns `ErrShutdownTimeout`.
- Test: the fast path returns nil and writes zero bytes; a stuck worker makes `Shutdown` return `ErrShutdownTimeout` (asserted with `errors.Is`) and the dump names the stuck worker.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/16-goroutine-debugging-under-load/09-dump-stacks-on-shutdown-timeout/cmd/demo
cd go-solutions/13-goroutines-and-channels/16-goroutine-debugging-under-load/09-dump-stacks-on-shutdown-timeout
```

### Bounded wait, then dump

Graceful shutdown has a fixed shape: stop accepting new work, cancel the context so
in-flight workers wind down, and wait for them to finish. The mistake is waiting
*forever* — a plain `wg.Wait()` with no deadline. One worker stuck in a syscall, a
leaked goroutine, a missing `ctx.Done()` branch, and the process hangs on shutdown
with nothing to look at; eventually the orchestrator `SIGKILL`s it and you never learn
why.

`Shutdown` bounds the wait. It cancels the context, then races the pool's completion
against a timer: it starts a goroutine that calls `wg.Wait()` and closes a `done`
channel, then `select`s between `done` and `time.After(timeout)`. If `done` wins, the
pool stopped cleanly and `Shutdown` returns `nil` having written nothing. If the timer
wins, `Shutdown` captures a full `runtime.Stack(_, true)` dump to the caller's
`io.Writer` and returns `ErrShutdownTimeout`. That dump is the payoff: it names every
goroutine still running and the state it is parked in, so "which worker ignored
cancellation" is answered by reading it, not by guessing.

The dump uses the same growing-buffer discipline as a diagnostic dump — under a hung
shutdown there may be many goroutines, and a truncated dump would drop the culprit's
frame. The well-behaved workers park on `<-ctx.Done()`; the stuck worker parks on a
private channel it only leaves when explicitly released, modelling a worker that
ignores the context entirely. Because the stuck worker's frame is
`shutdown.stuckWorker`, a test can assert the dump named it.

Create `shutdown.go`:

```go
package shutdown

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"time"
)

// ErrShutdownTimeout is returned when workers do not stop before the deadline.
var ErrShutdownTimeout = errors.New("shutdown: workers did not stop before deadline")

// Manager runs workers and shuts them down with a bounded wait.
type Manager struct {
	wg sync.WaitGroup
}

func New() *Manager {
	return &Manager{}
}

// StartWorkers launches n well-behaved workers that stop when ctx is cancelled.
func (m *Manager) StartWorkers(ctx context.Context, n int) {
	for range n {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			<-ctx.Done()
		}()
	}
}

// StartStuck launches a worker that ignores context cancellation; it stops only
// when block is closed. This models a worker that will hang shutdown.
func (m *Manager) StartStuck(block <-chan struct{}) {
	m.wg.Add(1)
	go stuckWorker(&m.wg, block)
}

func stuckWorker(wg *sync.WaitGroup, block <-chan struct{}) {
	defer wg.Done()
	<-block // ignores ctx: only an explicit release stops it
}

// Shutdown cancels the context, waits up to timeout for workers to stop, and on
// timeout writes a full goroutine dump to dumpTo and returns ErrShutdownTimeout.
// On the fast path it returns nil and writes nothing.
func (m *Manager) Shutdown(cancel context.CancelFunc, timeout time.Duration, dumpTo io.Writer) error {
	cancel()

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		writeStacks(dumpTo)
		return ErrShutdownTimeout
	}
}

// wait blocks until every worker has returned (used by tests after releasing a
// stuck worker).
func (m *Manager) wait() {
	m.wg.Wait()
}

func writeStacks(w io.Writer) {
	size := 1 << 16
	for {
		buf := make([]byte, size)
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			_, _ = w.Write(buf[:n])
			return
		}
		size *= 2
	}
}
```

### The runnable demo

The demo runs both paths. First, well-behaved workers shut down inside the deadline:
`Shutdown` returns nil and the dump buffer is empty. Then a stuck worker misses the
deadline: `Shutdown` returns the timeout error and the dump names the stuck worker.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"example.com/shutdown"
)

func main() {
	// Fast path: workers stop before the deadline.
	ctx, cancel := context.WithCancel(context.Background())
	m := shutdown.New()
	m.StartWorkers(ctx, 4)
	var fast bytes.Buffer
	err := m.Shutdown(cancel, time.Second, &fast)
	fmt.Println("graceful err:", err)
	fmt.Println("graceful dumped bytes:", fast.Len())

	// Stuck path: a worker ignores cancellation.
	_, cancel2 := context.WithCancel(context.Background())
	m2 := shutdown.New()
	block := make(chan struct{})
	m2.StartStuck(block)
	var slow bytes.Buffer
	err2 := m2.Shutdown(cancel2, 50*time.Millisecond, &slow)
	fmt.Println("stuck err:", err2)
	fmt.Println("dump names stuck worker:", strings.Contains(slow.String(), "shutdown.stuckWorker"))
	close(block)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
graceful err: <nil>
graceful dumped bytes: 0
stuck err: shutdown: workers did not stop before deadline
dump names stuck worker: true
```

### Tests

`TestFastPathNoDump` asserts a well-behaved pool shuts down within the deadline,
returns nil, and writes nothing to the dump writer — the fast path must be silent.
`TestTimeoutDumpsStuckWorker` starts a worker that ignores cancellation, asserts
`Shutdown` returns `ErrShutdownTimeout` via `errors.Is`, and asserts the captured dump
names the stuck worker's function.

Create `shutdown_test.go`:

```go
package shutdown

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestFastPathNoDump(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	m := New()
	m.StartWorkers(ctx, 5)

	var buf bytes.Buffer
	if err := m.Shutdown(cancel, time.Second, &buf); err != nil {
		t.Fatalf("Shutdown returned %v; want nil", err)
	}
	if buf.Len() != 0 {
		t.Errorf("fast path wrote %d bytes; want 0", buf.Len())
	}
}

func TestTimeoutDumpsStuckWorker(t *testing.T) {
	t.Parallel()
	_, cancel := context.WithCancel(context.Background())
	m := New()
	block := make(chan struct{})
	m.StartStuck(block)

	var buf bytes.Buffer
	err := m.Shutdown(cancel, 50*time.Millisecond, &buf)
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Shutdown err = %v; want ErrShutdownTimeout", err)
	}
	if !strings.Contains(buf.String(), "shutdown.stuckWorker") {
		t.Errorf("dump should name the stuck worker function")
	}

	close(block) // release so the goroutine exits
	m.wait()
}

func ExampleManager_Shutdown() {
	ctx, cancel := context.WithCancel(context.Background())
	m := New()
	m.StartWorkers(ctx, 3)
	fmt.Println(m.Shutdown(cancel, time.Second, io.Discard))
	// Output: <nil>
}
```

## Review

The path is correct when it is fast and silent on the happy path and bounded-plus-
diagnostic on the bad one. `TestFastPathNoDump` pins the crucial property that the
dump is written *only* on timeout — a shutdown that dumps stacks every time would bury
the signal in noise. `TestTimeoutDumpsStuckWorker` proves the dump earns its place: it
names `shutdown.stuckWorker`, which in a real incident is the difference between "the
process won't exit" and "this specific worker ignored cancellation, here is its
stack." The growing-buffer dump matters under a real hang where hundreds of goroutines
may be live. Note the `select` races `done` against `time.After`, so a worker that
finishes one nanosecond before the deadline still takes the fast path. Run `go test
-race` to confirm the `done`-closing goroutine and the `WaitGroup` are used safely.

## Resources

- [`runtime.Stack`](https://pkg.go.dev/runtime#Stack) — the full dump captured on timeout.
- [`context.CancelFunc`](https://pkg.go.dev/context#CancelFunc) — the cancellation Shutdown triggers before waiting.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching the sentinel `ErrShutdownTimeout`.

---

Prev: [08-goroutine-count-regression-watchdog.md](08-goroutine-count-regression-watchdog.md) | Back to [00-concepts.md](00-concepts.md) | Next: [10-execution-trace-scheduler-stalls.md](10-execution-trace-scheduler-stalls.md)
