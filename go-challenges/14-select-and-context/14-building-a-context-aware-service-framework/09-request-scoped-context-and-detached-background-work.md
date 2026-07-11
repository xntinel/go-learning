# Exercise 9: Propagating Request IDs and Detaching Background Work from Shutdown

Two context features finish the framework. First, request-scoped metadata — a
request ID injected by middleware, keyed by an unexported type so it can never
collide with another package's key. Second, `context.WithoutCancel` and
`context.AfterFunc`, which let a final audit flush complete *even though* the
context that triggered it is being cancelled during shutdown.

## What you'll build

```text
reqctx/                       independent module: example.com/reqctx
  go.mod                      go 1.26
  reqctx.go                   typed request-id key + middleware; StartAuditFlush (WithoutCancel/AfterFunc)
  reqctx_test.go              typed-key round-trip + no cross-type collision; detached flush survives cancel
  cmd/
    demo/
      main.go                 a request carries an id; shutdown flushes on a detached context
```

Files: `reqctx.go`, `cmd/demo/main.go`, `reqctx_test.go`.
Implement: `WithRequestID`/`RequestID` over an unexported `ctxKey`; `RequestIDMiddleware`; `StartAuditFlush(ctx, flush)` that runs `flush` on `context.WithoutCancel(ctx)` when `ctx` is cancelled, via `context.AfterFunc`, returning the `stop` deregister func.
Test: the typed key round-trips and a same-underlying-type key from "another package" cannot collide; a `WithoutCancel` child stays live after the parent is cancelled; `AfterFunc`'s callback fires exactly once on cancellation, and `stop` prevents it if called first.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/reqctx/cmd/demo
cd ~/go-exercises/reqctx
go mod init example.com/reqctx
```

### Typed keys and detached work

The context value key is an *unexported named type*, `type ctxKey int`, with a
constant of that type. Its type is part of its identity: another package that
also defines `type ctxKey int` and keys on it produces a *different* key, because
`reqctx.ctxKey` and `otherpkg.ctxKey` are distinct types even though both have
underlying type `int`. A bare `string` or `int` key would collide silently. The
accessor returns `(value, ok)`, so a missing value is a clean miss rather than a
panic on a failed type assertion. Context values are for request-scoped metadata
that crosses API boundaries — a request ID, a trace ID — never for mandatory
inputs, which belong in function signatures where the compiler checks them.

The detached-flush machinery solves a specific shutdown hazard. A final audit
write or metrics flush must run *because* the service is shutting down, but the
context carrying that shutdown is cancelled — so if the flush uses that context,
it is aborted mid-write. `context.WithoutCancel(parent)` returns a child that
keeps the parent's *values* (so the request ID still propagates into the audit
record) but is severed from its *cancellation*: the parent can be cancelled and
the child's `Err()` stays `nil`. `context.AfterFunc(ctx, f)` registers `f` to run
in its own goroutine once `ctx` is done, and returns a `stop` function that
deregisters `f` — `stop()` returns `true` if it prevented `f` from running. That
is the idiomatic cancellation hook: no hand-rolled `go func(){ <-ctx.Done(); ... }()`
to manage and leak.

Create `reqctx.go`:

```go
package reqctx

import (
	"context"
	"net/http"
)

// ctxKey is unexported, so its identity cannot collide with any key defined in
// another package, even one whose underlying type is also int.
type ctxKey int

const requestIDKey ctxKey = iota

// WithRequestID returns a child context carrying id as request-scoped metadata.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID reports the request id carried by ctx, if any.
func RequestID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey).(string)
	return id, ok
}

// RequestIDMiddleware injects a request id into the request context: it reuses
// an inbound X-Request-Id header when present, otherwise it uses gen(). The id
// is echoed on the response and made available to downstream handlers via
// RequestID.
func RequestIDMiddleware(gen func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = gen()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

// StartAuditFlush registers a final flush that survives cancellation of ctx.
// When ctx is cancelled, flush runs on a context detached from that
// cancellation (context.WithoutCancel), so the flush completes even though the
// shutdown context is done. It preserves ctx's values (e.g. the request id).
// The returned stop function deregisters the hook; stop() returns true if it
// prevented the flush from running.
func StartAuditFlush(ctx context.Context, flush func(context.Context)) (stop func() bool) {
	return context.AfterFunc(ctx, func() {
		flush(context.WithoutCancel(ctx))
	})
}
```

### The runnable demo

The demo sends a request through the middleware (the handler reads the id back out
of the context and records it), then registers an audit flush against a
cancellable context, cancels it, and shows the flush ran on a live detached
context even though the parent was cancelled.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http/httptest"

	"example.com/reqctx"
)

func main() {
	// Request path: middleware injects an id the handler reads back.
	handler := reqctx.RequestIDMiddleware(
		func() string { return "generated-id" },
		reqctx.HandlerFunc(func(ctx context.Context) {
			if id, ok := reqctx.RequestID(ctx); ok {
				fmt.Println("handler saw request id:", id)
			}
		}),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-Id", "req-42")
	handler.ServeHTTP(rec, req)
	fmt.Println("response header:", rec.Header().Get("X-Request-Id"))

	// Shutdown path: a final flush that survives the parent's cancellation.
	ctx, cancel := context.WithCancel(reqctx.WithRequestID(context.Background(), "req-42"))
	flushed := make(chan string, 1)
	reqctx.StartAuditFlush(ctx, func(detached context.Context) {
		id, _ := reqctx.RequestID(detached)
		flushed <- fmt.Sprintf("flush live=%v id=%s", detached.Err() == nil, id)
	})

	cancel() // parent cancelled: the detached flush still runs
	fmt.Println(<-flushed)
}
```

The demo needs a tiny `HandlerFunc` adapter that turns a `func(context.Context)`
into an `http.Handler`. Add to `reqctx.go`:

```go
// HandlerFunc adapts a context-only function into an http.Handler, for demos and
// tests that care only about the request context.
func HandlerFunc(fn func(ctx context.Context)) http.Handler {
	return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		fn(r.Context())
	})
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
handler saw request id: req-42
response header: req-42
flush live=true id=req-42
```

### Tests

`TestRequestIDRoundTrip` asserts the value round-trips and — crucially — that a
key of the *same underlying type* declared in "another package" (simulated by a
second unexported type in the test) does not collide. `TestWithoutCancelStaysLive`
cancels a parent and asserts the `WithoutCancel` child's `Err()` is still `nil`.
`TestAfterFuncFiresOnce` asserts the callback fires exactly once on cancellation,
and `TestAfterFuncStopPreventsRun` asserts calling `stop` before cancellation
returns `true` and the callback never runs.

Create `reqctx_test.go`:

```go
package reqctx

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// otherKey mimics a DIFFERENT package's key with the same underlying type (int).
// Its distinct type is what prevents a collision with requestIDKey.
type otherKey int

const otherRequestIDKey otherKey = iota

func TestRequestIDRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := WithRequestID(context.Background(), "req-7")

	got, ok := RequestID(ctx)
	if !ok || got != "req-7" {
		t.Fatalf("RequestID = %q,%v, want %q,true", got, ok, "req-7")
	}

	// A same-underlying-type key from "another package" must not collide.
	if v := ctx.Value(otherRequestIDKey); v != nil {
		t.Fatalf("otherKey collided: got %v, want nil", v)
	}
}

func TestRequestIDMissing(t *testing.T) {
	t.Parallel()
	if _, ok := RequestID(context.Background()); ok {
		t.Fatal("RequestID reported present on a bare context")
	}
}

func TestWithoutCancelStaysLive(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(WithRequestID(context.Background(), "req-9"))
	detached := context.WithoutCancel(parent)

	cancel()

	if parent.Err() == nil {
		t.Fatal("parent should be cancelled")
	}
	if detached.Err() != nil {
		t.Fatalf("detached.Err() = %v, want nil (severed from parent cancellation)", detached.Err())
	}
	// Values still propagate through WithoutCancel.
	if id, ok := RequestID(detached); !ok || id != "req-9" {
		t.Fatalf("detached RequestID = %q,%v, want %q,true", id, ok, "req-9")
	}
}

func TestAfterFuncFiresOnce(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64
	fired := make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	StartAuditFlush(ctx, func(detached context.Context) {
		if detached.Err() == nil { // detached must be live during the flush
			runs.Add(1)
		}
		fired <- struct{}{}
	})

	cancel()

	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("flush never ran after cancellation")
	}
	if runs.Load() != 1 {
		t.Fatalf("flush ran %d times, want exactly 1", runs.Load())
	}
}

func TestAfterFuncStopPreventsRun(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	stop := StartAuditFlush(ctx, func(context.Context) { runs.Add(1) })

	if !stop() {
		t.Fatal("stop() = false, want true when called before cancellation")
	}
	cancel()

	time.Sleep(20 * time.Millisecond) // give any (erroneous) callback a chance
	if runs.Load() != 0 {
		t.Fatalf("flush ran %d times after stop(), want 0", runs.Load())
	}
}
```

## Review

The module is correct when the request id round-trips through the context and a
same-underlying-type key from another package cannot read it — that is the entire
reason the key type is unexported, and the `otherKey` assertion pins it. The
detached-flush half proves the shutdown-safety property: `context.WithoutCancel`
yields a child whose `Err()` stays `nil` after the parent is cancelled, so a final
flush launched by `context.AfterFunc` runs on a live context and completes,
carrying the parent's request id along for the audit record. `AfterFunc` fires at
most once, and its `stop` deregisters the hook — returning `true` only when it
actually prevented the run. The mistakes this guards against: a bare-string key
that collides across packages, and a final flush that reuses the cancelled
context and is aborted mid-write. Run `go test -race`; the `fired` channel and
atomic counter keep the callback observation race-free.

## Resources

- [context.WithValue](https://pkg.go.dev/context#WithValue) — and the documented rule that keys should be an unexported type.
- [context.WithoutCancel](https://pkg.go.dev/context#WithoutCancel) — a child detached from the parent's cancellation.
- [context.AfterFunc](https://pkg.go.dev/context#AfterFunc) — a cancellation hook with a deregister function.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) — request-scoped values and propagation.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-errgroup-fan-out-supervision.md](08-errgroup-fan-out-supervision.md) | Next: [10-inflight-drain-tracking.md](10-inflight-drain-tracking.md)
