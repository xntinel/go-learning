# Exercise 1: Context-Aware Worker Loop (Cancel vs Channel-Close Exits)

Every backend consumer goroutine that pulls work off a channel has the same
skeleton: a `select` that watches `ctx.Done()` on one arm and the work channel on
the other. Get that skeleton right and the goroutine tears down cleanly on both a
cancel and a producer close; get it wrong and you leak a goroutine on shutdown.
This module builds that canonical loop and pins its two exits with tests.

This module is fully self-contained: its own `go mod init`, its own package, its
own demo, and its own tests. Nothing here imports another exercise.

## What you'll build

```text
worker/                    independent module: example.com/worker
  go.mod                   module example.com/worker
  worker.go                ErrClosed sentinel; Worker(ctx, ticks, onTick) error
  cmd/
    demo/
      main.go              feeds ticks, cancels, prints the exit error
  worker_test.go           the two exits + a tick-delivery test, all -race safe
```

- Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
- Implement: `Worker(ctx, ticks <-chan int, onTick func(int)) error` that returns `ctx.Err()` on cancel and a sentinel `ErrClosed` when the producer closes `ticks`.
- Test: cancel returns `context.Canceled`; close returns `ErrClosed`; ticks are delivered before a later cancel propagates.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p worker/cmd/demo
cd worker
go mod init example.com/worker
```

### Why the loop has exactly two exits

A long-running consumer must distinguish two fundamentally different shutdowns.
The first is *cancellation*: the caller aborted, the request context was
cancelled, and the right thing is to stop immediately and report why — which is
`ctx.Err()`, normally `context.Canceled`. The second is *end of input*: the
producer finished and closed the channel, and the right thing is to drain what is
left and report a clean, distinguishable end — here a package sentinel,
`ErrClosed`. Collapsing these two into one "the loop ended" signal loses
information the caller needs: a cancelled worker and an exhausted worker demand
different handling upstream (retry vs done).

The `select` expresses both. The `case <-ctx.Done()` arm fires on cancel and
returns `ctx.Err()`. The `case t, ok := <-ticks` arm fires on every delivered
value and, critically, on close: a receive from a closed channel returns the zero
value with `ok == false`, which is the loop's cue to return `ErrClosed`. Without
the `ok` check the loop would spin forever on a closed channel, receiving zero
values at full speed — a classic busy-loop bug.

One subtlety the tests pin down: when both arms are ready at once — the context is
cancelled *and* a value is waiting — `select` picks one pseudo-randomly. That is
fine for correctness here (either exit is valid the instant both are true), but it
is why the tick-delivery test feeds and drains its ticks *before* cancelling,
rather than racing a cancel against a pending send.

Create `worker.go`:

```go
package worker

import (
	"context"
	"errors"
)

// ErrClosed is returned by Worker when the producer closes the ticks channel
// and the worker has drained it. It is distinct from context.Canceled so a
// caller can tell "input exhausted" apart from "aborted".
var ErrClosed = errors.New("worker: ticks channel closed")

// Worker is the canonical context-aware consumer loop. It calls onTick for each
// value received on ticks and returns:
//   - ctx.Err() (normally context.Canceled) when ctx is cancelled, or
//   - ErrClosed when the producer closes ticks.
func Worker(ctx context.Context, ticks <-chan int, onTick func(int)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t, ok := <-ticks:
			if !ok {
				return ErrClosed
			}
			onTick(t)
		}
	}
}
```

### The runnable demo

The demo feeds three ticks through an unbuffered channel (each send blocks until
the worker receives it), then cancels. Because `main` blocks on the worker's
result channel, which the worker sends to only after processing tick 3, the three
`processed` lines are guaranteed to print before the exit line.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/worker"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	ticks := make(chan int)
	done := make(chan error, 1)
	go func() {
		done <- worker.Worker(ctx, ticks, func(t int) {
			fmt.Println("processed tick", t)
		})
	}()

	for i := 1; i <= 3; i++ {
		ticks <- i
	}

	cancel()
	fmt.Println("worker stopped:", <-done)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed tick 1
processed tick 2
processed tick 3
worker stopped: context canceled
```

### Tests

`TestWorkerStopsOnCancel` and `TestWorkerStopsOnClosedChannel` pin the two exits.
`TestWorkerInvokesOnTickUntilCancel` proves the worker actually delivers ticks
before a later cancel propagates. Every receive from the worker is guarded by
`time.After` so a wedged goroutine fails the test fast instead of hanging it, and
the whole file is race-clean under `-race`.

Create `worker_test.go`:

```go
package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWorkerStopsOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan int)

	done := make(chan error, 1)
	go func() {
		done <- Worker(ctx, ticks, func(int) {})
	}()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Worker err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Worker did not return within 1s of cancel")
	}
}

func TestWorkerStopsOnClosedChannel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan int)

	done := make(chan error, 1)
	go func() {
		done <- Worker(ctx, ticks, func(int) {})
	}()

	close(ticks)

	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Worker err = %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Worker did not return within 1s of close")
	}
}

func TestWorkerInvokesOnTickUntilCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan int, 3)

	got := make(chan int, 3)
	done := make(chan error, 1)
	go func() {
		done <- Worker(ctx, ticks, func(v int) { got <- v })
	}()

	for i := 1; i <= 3; i++ {
		ticks <- i
	}

	for drained := 0; drained < 3; drained++ {
		select {
		case <-got:
		case <-time.After(time.Second):
			t.Fatalf("Worker missed tick %d", drained+1)
		}
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Worker err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Worker did not return after cancel")
	}
}
```

## Review

The loop is correct when its two arms map to the two shutdowns and nothing else:
`ctx.Done()` returns `ctx.Err()`, and a closed `ticks` — detected by the `ok`
being false — returns `ErrClosed`. The bug to watch for is omitting the `ok`
check, which turns a closed channel into an infinite stream of zero values and a
pinned CPU. Assert both exits with `errors.Is`, never `==`, so a wrapped
`context.Canceled` still matches. The tick-delivery test deliberately drains all
three values before cancelling, because a cancel racing a pending send is a
coin-flip on which `select` arm wins — a real property of `select`, not a flake
to paper over. Run `go test -race` to confirm the goroutine handoffs are clean.

## Resources

- [context package](https://pkg.go.dev/context) — `Context.Done`, `Context.Err`, `context.Canceled`.
- [The Go Programming Language Specification: Select statements](https://go.dev/ref/spec#Select_statements) — how `select` chooses among ready cases.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) — the cancellation-propagation model.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-context-aware-generator.md](02-context-aware-generator.md)
