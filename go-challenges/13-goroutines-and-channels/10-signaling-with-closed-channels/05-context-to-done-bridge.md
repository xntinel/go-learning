# Exercise 5: Bridging context.Context Cancellation Into A Stop Channel

Real handlers must stop on two independent signals: the request's own
cancellation (client disconnect, request deadline) delivered through
`context.Context`, and a local operational stop (server drain). Because
`ctx.Done()` is itself a closed-channel broadcast, both signals are the same
mechanism, and a single `select` handles them side by side. This exercise builds
a processor that exits on either, plus a helper that *derives* a stop channel
from a context using `context.AfterFunc`, unifying the two sources.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
ctxbridge/                   independent module: example.com/ctxbridge
  go.mod                     go mod init example.com/ctxbridge
  processor.go               type Processor; Run(ctx, in), Stop; StopChanFromContext
  cmd/
    demo/
      main.go                runnable demo: drain, local stop, ctx-derived stop
  processor_test.go          ctx-cancel path, local-stop path, AfterFunc bridge
```

Files: `processor.go`, `cmd/demo/main.go`, `processor_test.go`.
Implement: a `Processor` whose loop selects over `ctx.Done()`, a local `stop chan struct{}`, and the input; plus `StopChanFromContext(ctx)` that returns a channel closed exactly when `ctx` is cancelled, via `context.AfterFunc`, and a stop func that unregisters it.
Test: cancel via context so the loop exits with `context.Canceled`; trigger local `Stop()` so it exits with `ErrLocalStop`; verify `AfterFunc` closes the derived channel on cancel and that calling its stop func prevents the close.
Verify: `go test -count=1 -race ./...`

### One select, two cancellation sources

`ctx.Done()` returns a `<-chan struct{}` that the context closes on cancellation.
It is exactly the same primitive as a hand-rolled `stop chan struct{}`, so both
sit in the same `select`:

```go
select {
case <-ctx.Done():
	return sum, ctx.Err() // request cancelled / deadline
case <-p.stop:
	return sum, ErrLocalStop // operational drain
case v, ok := <-in:
	...
}
```

The loop terminates on whichever fires first and reports *why*: `ctx.Err()`
(`context.Canceled` or `context.DeadlineExceeded`) for the context path, a local
sentinel for the operational path, and `nil` for a clean drain when `in` closes.
Returning a distinguishable reason matters operationally — a metric that counts
"stopped by client disconnect" versus "stopped by drain" versus "finished
naturally" is built on exactly this distinction.

### Deriving a stop channel from a context

Sometimes you have a context but an API that wants a `chan struct{}`. `context.
AfterFunc(ctx, f)` (Go 1.21) runs `f` in its own goroutine when `ctx` is
cancelled, and returns a `stop func() bool` that unregisters `f`. Closing a
channel inside `f` gives you a channel that closes exactly when the context is
cancelled:

```go
func StopChanFromContext(ctx context.Context) (<-chan struct{}, func() bool) {
	ch := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { close(ch) })
	return ch, stop
}
```

`AfterFunc` guarantees `f` runs at most once, so the bare `close(ch)` needs no
`sync.Once` guard. The returned `stop` is the escape hatch: calling it before the
context is cancelled unregisters `f` (returns `true`) so the channel *never*
closes, which lets a caller who finished early stop leaking the watcher goroutine.
If `stop` returns `false`, `f` has already been scheduled or run and the close
will happen — that is the honest report that you were too late to cancel it.

Set up the module:

```bash
mkdir -p ~/go-exercises/ctxbridge/cmd/demo
cd ~/go-exercises/ctxbridge
go mod init example.com/ctxbridge
```

Create `processor.go`:

```go
package ctxbridge

import (
	"context"
	"errors"
	"sync"
)

// ErrLocalStop is the exit reason when Stop is called (as opposed to context
// cancellation or a clean drain).
var ErrLocalStop = errors.New("local stop requested")

// Processor consumes ints until its context is cancelled, a local Stop is
// requested, or the input channel is closed.
type Processor struct {
	stop chan struct{}
	once sync.Once
}

// New returns a ready Processor.
func New() *Processor {
	return &Processor{stop: make(chan struct{})}
}

// Run accumulates values from in and returns the running sum plus the reason it
// stopped: ctx.Err() on cancellation, ErrLocalStop on Stop, or nil when in is
// closed and fully drained.
func (p *Processor) Run(ctx context.Context, in <-chan int) (int, error) {
	var sum int
	for {
		select {
		case <-ctx.Done():
			return sum, ctx.Err()
		case <-p.stop:
			return sum, ErrLocalStop
		case v, ok := <-in:
			if !ok {
				return sum, nil
			}
			sum += v
		}
	}
}

// Stop requests an operational shutdown. It is idempotent.
func (p *Processor) Stop() {
	p.once.Do(func() { close(p.stop) })
}

// StopChanFromContext returns a channel that is closed exactly when ctx is
// cancelled, together with a stop func. Calling the stop func before ctx is
// cancelled unregisters the watcher (returns true) so the channel never closes.
func StopChanFromContext(ctx context.Context) (<-chan struct{}, func() bool) {
	ch := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { close(ch) })
	return ch, stop
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/ctxbridge"
)

func main() {
	// Clean drain: producer closes the input.
	p := ctxbridge.New()
	in := make(chan int, 3)
	in <- 1
	in <- 2
	in <- 3
	close(in)
	sum, err := p.Run(context.Background(), in)
	fmt.Printf("drained sum=%d err=%v\n", sum, err)

	// Local operational stop.
	p2 := ctxbridge.New()
	live := make(chan int)
	go p2.Stop()
	sum2, err2 := p2.Run(context.Background(), live)
	fmt.Printf("stopped sum=%d reason=%v\n", sum2, err2)

	// Context-derived stop channel.
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := ctxbridge.StopChanFromContext(ctx)
	cancel()
	<-ch
	fmt.Println("bridged stop channel closed on cancel")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
drained sum=6 err=<nil>
stopped sum=0 reason=local stop requested
bridged stop channel closed on cancel
```

### Tests

Create `processor_test.go`:

```go
package ctxbridge

import (
	"context"
	"errors"
	"testing"
	"time"
)

type result struct {
	sum int
	err error
}

func TestRunExitsOnContextCancel(t *testing.T) {
	t.Parallel()

	p := New()
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan int) // nothing sent

	done := make(chan result, 1)
	go func() {
		s, e := p.Run(ctx, in)
		done <- result{s, e}
	}()

	cancel()
	r := <-done
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", r.err)
	}
}

func TestRunExitsOnLocalStop(t *testing.T) {
	t.Parallel()

	p := New()
	in := make(chan int) // nothing sent

	done := make(chan error, 1)
	go func() {
		_, e := p.Run(context.Background(), in)
		done <- e
	}()

	p.Stop()
	if err := <-done; !errors.Is(err, ErrLocalStop) {
		t.Fatalf("err = %v, want ErrLocalStop", err)
	}
}

func TestRunDrainsThenNil(t *testing.T) {
	t.Parallel()

	p := New()
	in := make(chan int, 3)
	in <- 10
	in <- 20
	in <- 30
	close(in)

	sum, err := p.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if sum != 60 {
		t.Fatalf("sum = %d, want 60", sum)
	}
}

func TestBridgeClosesOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := StopChanFromContext(ctx)

	select {
	case <-ch:
		t.Fatal("derived channel closed before cancel")
	default:
	}

	cancel()
	select {
	case <-ch:
		// expected: AfterFunc closed it
	case <-time.After(time.Second):
		t.Fatal("derived channel not closed after cancel")
	}
}

func TestBridgeStopPreventsClose(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	ch, stop := StopChanFromContext(ctx)

	if !stop() {
		t.Fatal("stop() returned false before cancel; want true")
	}
	cancel() // watcher unregistered, so f never runs

	select {
	case <-ch:
		t.Fatal("derived channel closed despite stop() unregistering it")
	case <-time.After(20 * time.Millisecond):
		// expected: never closes
	}
}
```

## Review

The processor is correct when its exit reason is a faithful function of which
signal fired: `context.Canceled` on `cancel()`, `ErrLocalStop` on `Stop()`, `nil`
on a drained input — the three `Run` tests pin each path. The bridge is correct
when the derived channel closes exactly on cancellation and `stop()` truly
prevents that close; `TestBridgeStopPreventsClose` catches the common misreading
of `AfterFunc` semantics (thinking `stop()` cancels the *context* rather than
unregistering the callback). The failure mode without a context arm is a
goroutine that only exits on a local stop nobody wires, so it outlives the request
that spawned it. Run `go test -race`.

## Resources

- [pkg.go.dev: context.AfterFunc](https://pkg.go.dev/context#AfterFunc) — runs a func on cancellation and returns a stop that unregisters it.
- [pkg.go.dev: context.Context](https://pkg.go.dev/context#Context) — `Done` returns the closed-channel broadcast; `Err` reports `Canceled`/`DeadlineExceeded`.
- [pkg.go.dev: context.WithCancel](https://pkg.go.dev/context#WithCancel) — deriving a cancellable context.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-first-error-abort-fanout.md](06-first-error-abort-fanout.md)
