# Exercise 6: OrDone — A Context-Aware Wrapper Around a Receive-Only Stream

Every long-lived consumer needs to abandon a stream on cancellation without
leaking the goroutine reading it. `OrDone(ctx, in <-chan T) <-chan T` wraps a
stream so consumers drain it normally but stop cleanly when the context is
cancelled. It composes two receive-only channels: your data channel and
`ctx.Done()`, itself a `<-chan struct{}`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
ordone/                      independent module: example.com/ordone
  go.mod                     go 1.26
  ordone.go                  OrDone[T any](ctx context.Context, in <-chan T) <-chan T
  cmd/
    demo/
      main.go                runnable demo: cancel a stream mid-drain
  ordone_test.go             pass-through, stop-on-cancel, stop-on-upstream-close
```

Files: `ordone.go`, `cmd/demo/main.go`, `ordone_test.go`.
Implement: `OrDone(ctx context.Context, in <-chan T) <-chan T` that forwards values until either `in` closes or `ctx` is cancelled, then closes its output and exits its goroutine.
Test: pass-through with no cancellation, stop-on-cancel closes the output and the goroutine exits, stop-on-upstream-close.
Verify: `go test -count=1 -race ./...`

### Why the doubled select

`OrDone` runs a goroutine that owns `out` and closes it on exit. The body
`select`s on two receive-only channels: `ctx.Done()` and `in`. If the context is
cancelled, the goroutine returns and `defer close(out)` fires. If `in` closes,
the two-value receive `v, ok := <-in` reports `ok == false` and the goroutine
returns the same way.

There is a second, inner `select` around the *send* to `out`, and it is the part
people forget. After receiving a value from `in`, sending it to `out` can itself
block if the downstream consumer has stopped reading. If you only checked
`ctx.Done()` at the top and then did a bare `out <- v`, a cancellation that
arrives while you are parked on that send would never be observed — the goroutine
leaks. So the send is also a `select` between `out <- v` and `<-ctx.Done()`. Now
every blocking point in the goroutine is cancellable, and the goroutine is
guaranteed to exit when the context is done, whether it is waiting to receive or
waiting to send.

Because the returned type is `<-chan T`, a consumer can only drain it; the
`OrDone` goroutine is the sole owner and closer of `out`. Proving the goroutine
actually exits is important — a cancellation wrapper that leaks is worse than no
wrapper — so the tests assert the output channel closes, which can only happen
via the goroutine's `defer close(out)`.

Create `ordone.go`:

```go
package ordone

import "context"

// OrDone forwards values from in to the returned channel until in closes or ctx
// is cancelled, whichever comes first. It then closes the returned channel and
// its goroutine exits. Both the input and ctx.Done() are receive-only channels;
// the returned channel is receive-only to the caller.
func OrDone[T any](ctx context.Context, in <-chan T) <-chan T {
	out := make(chan T)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case v, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- v:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}
```

### The runnable demo

The demo starts an effectively-infinite source, wraps it with a context, and
cancels after reading three values. Without `OrDone` the reader goroutine would
block forever on the never-closing source; with it, cancellation closes the
wrapped stream and the loop ends.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/ordone"
)

func counter(ctx context.Context) <-chan int {
	ch := make(chan int)
	go func() {
		defer close(ch)
		for i := 0; ; i++ {
			select {
			case ch <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := ordone.OrDone(ctx, counter(ctx))
	seen := 0
	for v := range stream {
		fmt.Printf("got %d\n", v)
		seen++
		if seen == 3 {
			cancel()
		}
	}
	fmt.Println("stream drained after cancel")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
got 0
got 1
got 2
stream drained after cancel
```

### Tests

`TestOrDonePassesThroughAllValues` uses `t.Context()` (never cancelled during the
test) and asserts every value arrives in order. `TestOrDoneStopsOnCancel` cancels
mid-stream and asserts the wrapped output closes — which proves the internal
goroutine exited via its `defer`. `TestOrDoneStopsOnUpstreamClose` closes the
input and asserts the output closes in turn.

Create `ordone_test.go`:

```go
package ordone

import (
	"context"
	"testing"
	"time"
)

func TestOrDonePassesThroughAllValues(t *testing.T) {
	t.Parallel()

	in := make(chan int)
	go func() {
		defer close(in)
		for i := range 5 {
			in <- i
		}
	}()

	out := OrDone(t.Context(), in)
	var got []int
	for v := range out {
		got = append(got, v)
	}
	if len(got) != 5 {
		t.Fatalf("got %d values, want 5", len(got))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("got[%d] = %d, want %d", i, v, i)
		}
	}
}

func TestOrDoneStopsOnCancel(t *testing.T) {
	t.Parallel()

	// An infinite source that only stops when out is closed downstream.
	in := make(chan int)
	go func() {
		for i := 0; ; i++ {
			in <- i
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	out := OrDone(ctx, in)

	// Read a few, then cancel.
	for range 3 {
		<-out
	}
	cancel()

	// Drain until the wrapper closes out. If it never closes, the goroutine
	// leaked and this test times out.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range out {
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("OrDone output never closed after cancel; goroutine leaked")
	}
}

func TestOrDoneStopsOnUpstreamClose(t *testing.T) {
	t.Parallel()

	in := make(chan int)
	close(in)

	out := OrDone(t.Context(), in)
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("output delivered a value from a closed input")
		}
	case <-time.After(time.Second):
		t.Fatal("output never closed after upstream close")
	}
}
```

## Review

`OrDone` is correct when the goroutine it launches always exits — on upstream
close or on cancellation — and never leaks. The stop-on-cancel test is the one
that earns its keep: it cancels while the source is still producing and asserts
the wrapped output eventually closes, which is only possible if the goroutine
returned. The inner `select` around `out <- v` is the easy thing to omit and the
reason the goroutine can leak; without it, a cancellation arriving during a
blocked send is never seen. Run `go test -race` to confirm the cancellation
handoff is clean.

## Resources

- [`context` package](https://pkg.go.dev/context) — `Context.Done()` returns `<-chan struct{}`; `WithCancel` and the cancellation model.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the or-done idiom for abandoning a stream without leaking goroutines.
- [`testing.T.Context`](https://pkg.go.dev/testing#T.Context) — the per-test context (Go 1.24+) used in the pass-through test.

---

Prev: [05-tee-audit-split.md](05-tee-audit-split.md) | Back to [00-concepts.md](00-concepts.md) | Next: [07-broker-pubsub-directional.md](07-broker-pubsub-directional.md)
