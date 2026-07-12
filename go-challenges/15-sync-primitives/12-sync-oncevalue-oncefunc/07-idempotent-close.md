# Exercise 7: Idempotent Close: OnceValue on the Graceful-Shutdown Path

Shutdown code closes resources from more than one path — a `defer` in `main`,
a signal handler, a health-check-driven teardown — and real `Close`
implementations are not idempotent; this exercise builds the once-wrapped
closer that makes double-close safe and gives every caller the same error.

## What you'll build

```text
idemclose/                 independent module: example.com/idemclose
  go.mod                   go mod init example.com/idemclose
  idemclose.go             type Closer; Wrap(io.Closer), Close() error; WrapDiscard variant
  idemclose_test.go        table-driven double-close from goroutines + defer, identical error, Example
  cmd/
    demo/
      main.go              runnable demo: signal path and defer path close the same conn once
```

- Files: `idemclose.go`, `idemclose_test.go`, `cmd/demo/main.go`.
- Implement: `Wrap(c io.Closer) *Closer` whose `Close() error` is `sync.OnceValue(c.Close)` — the real close runs once, and every caller gets the identical error; `WrapDiscard(c io.Closer) func()` via `sync.OnceFunc` for when the error is intentionally dropped.
- Test: `Close` called from 2 goroutines plus a defer runs the real close exactly once and every caller observes the identical error value, table-driven over nil and non-nil close errors, under `-race`.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

### Why double-close is a real bug, not a nit

`io.Closer`'s documentation is explicit: "The behavior of Close after the
first call is undefined. Specific implementations may document their own
behavior." In practice, closing a `*net.TCPConn` twice returns
`use of closed network connection`; closing an `*os.File` twice returns
`file already closed`; some wrappers panic. The second error is pure noise —
the resource *was* released — but it lands in shutdown logs, trips
error-rate alerts during deploys, and in the worst pattern gets returned from
`Shutdown()` masking a real flush error from the first close.

And double-close is structural in shutdown code, not sloppiness. The robust
pattern is belt-and-braces: `defer conn.Close()` right after acquisition (so
early returns and panics release the resource) *plus* an explicit close on
the signal-driven graceful path (so shutdown is orderly and errors are
observed). Both paths fire in a normal shutdown. The fix is not to carefully
ensure only one path closes — that reintroduces leak-on-early-return — but to
make close idempotent.

### Why OnceValue and not OnceFunc — and when the reverse

`Close() error` returns a single value, so `sync.OnceValue(c.Close)` is the
exact fit: `c.Close` is a method value (a closure bound to this specific
resource), the real close runs once, and the `error` — nil or not — is cached
and handed to every caller. That last property is the design point: the
*first* close's error is the truth about the resource (did the final flush
make it to disk?), and both the defer path and the signal path observe that
same truth instead of one of them seeing `file already closed`. If the close
returned `(stats, error)`, `sync.OnceValues` would be the same pattern one
arity up.

`WrapDiscard` is the `OnceFunc` variant for the deliberately fire-and-forget
case — closing a read side you never wrote to, where the error carries no
information. Making the discard explicit in the type (`func()`, no error to
check) is more honest than `_ = c.Close()` scattered at call sites. Choose it
consciously; a buffered writer's close error is a data-loss signal and must
not go through `WrapDiscard`.

Note what the wrapper does *not* promise: it deduplicates calls through
*itself* only. If some code path holds the raw `io.Closer` and calls its
`Close` directly, the underlying close runs again — the deduplication-scope
rule from the concepts file. Wrap at acquisition and hand out only the
wrapper.

Create `idemclose.go`:

```go
// Package idemclose makes Close idempotent: the real Close runs exactly
// once, and every caller -- defer, signal handler, teardown chain --
// observes the same cached error.
package idemclose

import (
	"io"
	"sync"
)

// Closer wraps an io.Closer with an idempotent Close. Construct with Wrap;
// share by pointer and route every close through it.
type Closer struct {
	close func() error
}

// Wrap returns a Closer whose Close invokes c.Close at most once. The
// error of that single real close (nil or not) is returned to every
// caller, on every call.
func Wrap(c io.Closer) *Closer {
	return &Closer{close: sync.OnceValue(c.Close)}
}

// Close releases the underlying resource on first call and returns the
// cached result thereafter. Safe for concurrent use.
func (c *Closer) Close() error {
	return c.close()
}

// WrapDiscard is the fire-and-forget variant: the real Close runs at most
// once and its error is deliberately discarded. Use it only where the close
// error carries no information; a buffered writer's close error does.
func WrapDiscard(c io.Closer) func() {
	return sync.OnceFunc(func() { _ = c.Close() })
}
```

### The demo

The demo stages a miniature graceful shutdown with both classic paths wired
to one wrapped connection: `main` defers `Close` at acquisition, and a
"signal handler" goroutine (triggered through a channel standing in for
`signal.Notify`, to keep the run deterministic) closes explicitly. The
underlying close is rigged to fail with a flush error — and both paths report
the identical error while the real close runs once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"sync/atomic"

	"example.com/idemclose"
)

// conn stands in for any socket/file-holding resource whose Close is not
// idempotent and whose close error matters (a final flush).
type conn struct {
	closes atomic.Int64
}

func (c *conn) Close() error {
	c.closes.Add(1)
	return errors.New("flush buffers: disk full")
}

func main() {
	raw := &conn{}
	wrapped := idemclose.Wrap(raw)
	defer func() {
		fmt.Println("defer path close:", wrapped.Close())
		fmt.Println("real closes:", raw.closes.Load())
	}()

	sig := make(chan struct{})
	done := make(chan struct{})
	go func() { // stands in for the signal.Notify handler
		<-sig
		fmt.Println("signal received, shutting down")
		fmt.Println("signal path close:", wrapped.Close())
		close(done)
	}()

	fmt.Println("serving")
	close(sig) // deliver the "signal"
	<-done
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
serving
signal received, shutting down
signal path close: flush buffers: disk full
defer path close: flush buffers: disk full
real closes: 1
```

Both paths saw the flush failure — the real one — and the resource was
released exactly once.

### Tests

The table covers the two error cases (nil and a sentinel wrapped with `%w`).
Each case closes from two concurrent goroutines plus a third sequential call
standing in for the defer, then asserts one real close and — the strong
property — the *identical* error value for every caller, checked with both
`errors.Is` and `==`. `TestWrapDiscard` pins the once property of the
fire-and-forget variant.

Create `idemclose_test.go`:

```go
package idemclose

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

var errDiskFull = errors.New("disk full")

type fakeConn struct {
	err    error
	closes atomic.Int64
}

func (f *fakeConn) Close() error {
	f.closes.Add(1)
	return f.err
}

func TestCloseOnceAllCallersSameError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		closeErr error
	}{
		{name: "clean close", closeErr: nil},
		{name: "failing close", closeErr: fmt.Errorf("flush buffers: %w", errDiskFull)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw := &fakeConn{err: tt.closeErr}
			wrapped := Wrap(raw)

			errs := make([]error, 3)
			var wg sync.WaitGroup
			wg.Add(2)
			for i := range 2 { // signal path and teardown chain, racing
				go func() {
					defer wg.Done()
					errs[i] = wrapped.Close()
				}()
			}
			wg.Wait()
			errs[2] = wrapped.Close() // the defer path, last

			if got := raw.closes.Load(); got != 1 {
				t.Fatalf("real Close ran %d times, want 1", got)
			}
			for i, err := range errs {
				if !errors.Is(err, tt.closeErr) {
					t.Errorf("caller %d: err = %v, want %v", i, err, tt.closeErr)
				}
				if err != errs[0] {
					t.Errorf("caller %d observed a different error value than caller 0", i)
				}
			}
			if tt.closeErr != nil && !errors.Is(errs[2], errDiskFull) {
				t.Errorf("defer path lost the sentinel: %v", errs[2])
			}
		})
	}
}

func TestWrapDiscard(t *testing.T) {
	t.Parallel()

	raw := &fakeConn{err: errors.New("ignored")}
	closeFn := WrapDiscard(raw)

	var wg sync.WaitGroup
	wg.Add(10)
	for range 10 {
		go func() {
			defer wg.Done()
			closeFn()
		}()
	}
	wg.Wait()
	if got := raw.closes.Load(); got != 1 {
		t.Fatalf("real Close ran %d times, want 1", got)
	}
}

func ExampleWrap() {
	raw := &fakeConn{}
	c := Wrap(raw)
	fmt.Println(c.Close())
	fmt.Println(c.Close())
	fmt.Println("real closes:", raw.closes.Load())
	// Output:
	// <nil>
	// <nil>
	// real closes: 1
}
```

Run the suite:

```bash
go test -count=1 -race ./...
```

## Review

The wrapper is doing its job when the table test shows, under `-race`, one
real close and error-value identity across a racing pair of goroutines plus
a late sequential call — for both the nil and non-nil error rows. The
production mistakes this module guards: returning the *second* close's
`file already closed` from a shutdown function (masking a real flush error),
"fixing" double-close by removing the defer (reintroducing leaks on early
return), and wrapping the resource but leaking the raw `io.Closer` to code
that closes it directly — the once only covers calls through the wrapper.
Also resist the temptation to use `WrapDiscard` everywhere because `func()`
is convenient: an error-returning close on a write path is a data-integrity
signal, and this package's whole point is that every path can afford to
observe it, because they all get the same one.

## Resources

- [io.Closer](https://pkg.go.dev/io#Closer) — "behavior of Close after the first call is undefined", the contract that motivates the wrapper.
- [sync.OnceValue](https://pkg.go.dev/sync#OnceValue) — caching the single close error for every caller.
- [net.Conn](https://pkg.go.dev/net#Conn) — a real Closer whose double-close returns "use of closed network connection".

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-per-key-template-cache.md](06-per-key-template-cache.md) | Next: [08-http-client-singleton.md](08-http-client-singleton.md)
