# Exercise 3: Readiness Barrier: Gating Handlers On A Closed Channel

A server that finished binding its port but has not yet warmed its caches, opened
its DB pool, or loaded config must not serve requests. The idiomatic gate is a
closed channel: request handlers block on `ready` until `MarkReady()` closes it
once, releasing every waiting handler at the same instant. A closed channel is a
"permanently open gate" — after close, every future receive returns immediately,
so late-arriving requests pass straight through with no re-arm.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
readygate/                   independent module: example.com/readygate
  go.mod                     go mod init example.com/readygate
  server.go                  type Server; New, MarkReady, Wait(ctx) error
  cmd/
    demo/
      main.go                runnable demo: blocked waiters released by MarkReady
  server_test.go             M waiters released at once; ctx-cancel path; re-arm
```

Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
Implement: a `Server` with a `ready chan struct{}`; `Wait(ctx)` selects over `ready` and `ctx.Done()`; `MarkReady()` closes `ready` once via `sync.Once`.
Test: launch M goroutines blocked on `Wait` before ready, then `MarkReady()` releases all; a waiter whose context is cancelled before ready returns `ctx.Err()`; a `Wait` after ready returns instantly; `MarkReady` is safe to call twice.
Verify: `go test -count=1 -race ./...`

### The gate that opens once and stays open

`Wait(ctx)` is the barrier every handler calls before serving:

```go
select {
case <-s.ready:
	return nil // gate is open
case <-ctx.Done():
	return ctx.Err() // caller gave up (client disconnect, deadline)
}
```

While `ready` is open, the first case blocks and the handler waits — but it is
*not* a naive block, because the second case lets an individual request bail if
its own context is cancelled (a disconnected client should not be held hostage to
a slow startup). When `MarkReady()` closes `ready`, the first case becomes
permanently ready for every current and future waiter. The one close releases an
unbounded number of blocked handlers at once — that is the broadcast property,
applied to admission control.

The "stays open" half matters as much as the "opens once" half. After close, a
request that arrives an hour later still finds `ready` closed and returns
immediately from the `select`; the gate does not need to be re-armed per request,
and there is no window where a late request wrongly blocks. That is why a closed
channel, not a `sync.Cond` or a polled bool, is the right tool: the state is
terminal and shared.

`MarkReady()` wraps the close in `sync.Once` so multiple startup paths (a health
probe flipping ready, an init routine finishing) can all call it without a
double-close panic. Exactly one close happens.

Set up the module:

```bash
mkdir -p ~/go-exercises/readygate/cmd/demo
cd ~/go-exercises/readygate
go mod init example.com/readygate
```

Create `server.go`:

```go
package readygate

import (
	"context"
	"sync"
)

// Server gates request handling behind a readiness barrier. Handlers call Wait
// before serving; MarkReady closes the barrier once, releasing all of them.
type Server struct {
	ready chan struct{}
	once  sync.Once
}

// New returns a Server whose readiness gate is closed (not ready).
func New() *Server {
	return &Server{ready: make(chan struct{})}
}

// Wait blocks until the server is marked ready or the caller's context is
// cancelled. It returns nil once ready, or ctx.Err() if ctx is done first.
func (s *Server) Wait(ctx context.Context) error {
	select {
	case <-s.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// MarkReady opens the gate exactly once, releasing every waiting handler. It is
// safe to call from multiple goroutines and multiple times.
func (s *Server) MarkReady() {
	s.once.Do(func() { close(s.ready) })
}

// IsReady reports whether the gate is open without blocking.
func (s *Server) IsReady() bool {
	select {
	case <-s.ready:
		return true
	default:
		return false
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"

	"example.com/readygate"
)

func main() {
	s := readygate.New()
	fmt.Printf("ready before MarkReady: %v\n", s.IsReady())

	const waiters = 5
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for range waiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Wait(context.Background()); err == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}

	s.MarkReady()
	wg.Wait()

	fmt.Printf("ready after MarkReady: %v\n", s.IsReady())
	fmt.Printf("handlers admitted: %d\n", admitted)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ready before MarkReady: false
ready after MarkReady: true
handlers admitted: 5
```

### Tests

The barrier test launches many waiters *before* readiness, confirms none has
passed, then `MarkReady()` and asserts all are released. The cancellation test
gives one waiter a context that is cancelled before ready and asserts it returns
`context.Canceled`. The re-arm test confirms a `Wait` after ready returns
instantly and `MarkReady` twice is a no-op.

Create `server_test.go`:

```go
package readygate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestMarkReadyReleasesAllWaiters(t *testing.T) {
	t.Parallel()

	s := New()
	const waiters = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0

	for range waiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Wait(context.Background()); err == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}

	// Give the goroutines time to park on Wait, then open the gate once.
	time.Sleep(20 * time.Millisecond)
	if s.IsReady() {
		t.Fatal("IsReady true before MarkReady")
	}
	s.MarkReady()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if admitted != waiters {
		t.Fatalf("admitted = %d, want %d", admitted, waiters)
	}
}

func TestWaitReturnsCtxErrWhenCancelledFirst(t *testing.T) {
	t.Parallel()

	s := New() // never marked ready
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- s.Wait(ctx) }()

	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait err = %v, want context.Canceled", err)
	}
}

func TestWaitAfterReadyReturnsImmediately(t *testing.T) {
	t.Parallel()

	s := New()
	s.MarkReady()

	// A cancelled context must NOT win: ready is already open, so Wait returns
	// nil even though ctx is done, only because the ready case is also ready and
	// select picks fairly. To make this deterministic, use a live context.
	if err := s.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after ready = %v, want nil", err)
	}
}

func TestMarkReadyIdempotent(t *testing.T) {
	t.Parallel()

	s := New()
	s.MarkReady()
	s.MarkReady() // second close guarded by sync.Once; must not panic
	if !s.IsReady() {
		t.Fatal("IsReady false after MarkReady")
	}
}
```

## Review

The gate is correct when `MarkReady()` releases every blocked waiter with a single
close and a post-ready `Wait` returns without blocking — the 40-waiter test proves
the first, `TestWaitAfterReadyReturnsImmediately` the second. The cancellation arm
is what keeps a slow startup from pinning a disconnected client forever; without
it, `Wait` on an un-ready server would block until ready no matter what the caller
wants. The trap is trying to make readiness a *repeatable* event by
close-and-recreating `ready`: a closed channel is one-shot, and any goroutine
still holding the old reference would spin. Readiness happens once and stays; that
is exactly what a closed channel models. Run `go test -race`.

## Resources

- [pkg.go.dev: context.Context](https://pkg.go.dev/context#Context) — `Done` returns the same closed-channel broadcast; `Err` reports why.
- [pkg.go.dev: sync.Once](https://pkg.go.dev/sync#Once) — the idempotent-close guard behind `MarkReady`.
- [The Go Programming Language Specification: Select statements](https://go.dev/ref/spec#Select_statements) — how a `select` chooses among ready cases.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-idempotent-closer.md](04-idempotent-closer.md)
