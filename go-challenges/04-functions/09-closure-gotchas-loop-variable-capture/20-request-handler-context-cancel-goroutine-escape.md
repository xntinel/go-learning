# Exercise 20: Request Handler Setup: Context CancelFunc Captured in Goroutine Loop

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A request handler setup loop builds a `context.CancelFunc` for each handler
and launches a cleanup goroutine meant to cancel that handler's own context.
The trap: if every cleanup goroutine closes over a single shared variable
holding "the current handler's cancel func" instead of its own, they all end
up calling the SAME (last) handler's cancel func — leaving every earlier
handler's context never cancelled at all.

## What you'll build

```text
reqsetup/                    independent module: example.com/reqsetup
  go.mod                     go 1.24
  reqsetup.go                  Handler, SetupHandlers, SetupHandlersBuggy
  cmd/
    demo/
      main.go                runnable demo: set up handlers, print cancelled count
  reqsetup_test.go             all-cancelled vs. only-last-cancelled, single-handler edge case
```

- Files: `reqsetup.go`, `cmd/demo/main.go`, `reqsetup_test.go`.
- Implement: `SetupHandlers(ids)` launching one cleanup goroutine per handler that receives its own `context.CancelFunc` as a parameter; `SetupHandlersBuggy` launching goroutines that all read a single shared `currentCancel` variable instead.
- Test: assert every handler's context is cancelled for the correct version; assert the buggy version only cancels the last handler's context and leaves the rest live, deterministically, using a barrier so the test never flakes.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/20-request-handler-context-cancel-goroutine-escape/cmd/demo
cd go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/20-request-handler-context-cancel-goroutine-escape
go mod edit -go=1.24
```

### Why the cancel func must be a parameter, not a shared variable

`SetupHandlers` passes each handler's own `cancel` to its cleanup goroutine
as an argument, so that goroutine always cancels the context it belongs to,
regardless of what the setup loop does afterward.

`SetupHandlersBuggy` recreates the classic mistake deliberately: `current
Cancel` is declared once, outside the loop, and every cleanup goroutine reads
it instead of receiving its own handler's cancel func. A cleanup goroutine
never runs before the setup loop finishes registering the rest, so this
exercise makes the worst case *deterministic* instead of just possible: every
goroutine blocks on a `release` channel until the setup loop has finished
overwriting `currentCancel` with the last handler's cancel func, then they
all read it. `context.CancelFunc` is itself safe to call concurrently and
repeatedly by design, so the bug is not in calling it from many goroutines —
it is entirely in *which* cancel func every goroutine ends up holding. The
result: the last handler's context gets cancelled once per handler in the
batch, and every earlier handler's context is never cancelled — a real
context leak, since nothing downstream watching those contexts ever sees
them finish.

Create `reqsetup.go`:

```go
package reqsetup

import (
	"context"
	"sync"
)

// Handler models one request's setup: its own context and the cancel func
// that releases it.
type Handler struct {
	ID     string
	Ctx    context.Context
	Cancel context.CancelFunc
}

// SetupHandlersBuggy builds one context.CancelFunc per handler and, for each,
// launches a cleanup goroutine meant to cancel THAT handler's own context
// once cleanup runs. But every goroutine closes over a SINGLE shared
// `currentCancel` variable declared outside the loop instead of its own
// handler's cancel func. A cleanup goroutine never runs before the setup
// loop finishes registering the rest, so this exercise makes that worst case
// deterministic with a barrier: every cleanup goroutine blocks until the
// setup loop has finished (closing `release`), then they all read
// `currentCancel` -- which by then holds only the LAST handler's cancel
// func. context.CancelFunc is itself safe to call concurrently and
// repeatedly, so calling it from many goroutines is not the bug; reading the
// wrong one is. The result: the last handler's context is cancelled
// (len(ids) times over), and every earlier handler's context is NEVER
// cancelled at all.
func SetupHandlersBuggy(ids []string) (handlers []*Handler, runCleanup func()) {
	handlers = make([]*Handler, len(ids))
	var currentCancel context.CancelFunc // BUG: shared across every handler
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i, id := range ids {
		ctx, cancel := context.WithCancel(context.Background())
		currentCancel = cancel
		handlers[i] = &Handler{ID: id, Ctx: ctx, Cancel: cancel}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-release
			currentCancel() // BUG: reads whatever currentCancel holds NOW
		}()
	}
	close(release) // only now do cleanup goroutines read currentCancel
	return handlers, wg.Wait
}

// SetupHandlers builds one context.CancelFunc per handler and launches a
// cleanup goroutine that receives its OWN cancel func as an argument, so
// cleanup always cancels the handler it belongs to.
func SetupHandlers(ids []string) (handlers []*Handler, runCleanup func()) {
	handlers = make([]*Handler, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		ctx, cancel := context.WithCancel(context.Background())
		handlers[i] = &Handler{ID: id, Ctx: ctx, Cancel: cancel}
		wg.Add(1)
		go func(c context.CancelFunc) {
			defer wg.Done()
			c() // cancels this handler's own context
		}(cancel)
	}
	return handlers, wg.Wait
}
```

### The runnable demo

The demo sets up three handlers with both variants, runs cleanup, and counts
how many contexts ended up cancelled.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/reqsetup"
)

func canceledCount(handlers []*reqsetup.Handler) int {
	n := 0
	for _, h := range handlers {
		if h.Ctx.Err() != nil {
			n++
		}
	}
	return n
}

func main() {
	ids := []string{"req-1", "req-2", "req-3"}

	handlers, cleanup := reqsetup.SetupHandlers(ids)
	cleanup()
	fmt.Println("correct cancelled count:", canceledCount(handlers), "of", len(handlers))

	buggyHandlers, buggyCleanup := reqsetup.SetupHandlersBuggy(ids)
	buggyCleanup()
	fmt.Println("buggy   cancelled count:", canceledCount(buggyHandlers), "of", len(buggyHandlers))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
correct cancelled count: 3 of 3
buggy   cancelled count: 1 of 3
```

### Tests

`TestSetupHandlers` asserts every handler's context is cancelled after
cleanup runs. `TestSetupHandlersBuggyOnlyCancelsLast` asserts every context
except the last stays live (`ctx.Err() == nil`) and the last one is
cancelled. `TestSetupHandlersSingleHandlerEdgeCase` covers the boundary where
the bug cannot manifest because there is no earlier handler to leak away
from.

Create `reqsetup_test.go`:

```go
package reqsetup

import (
	"context"
	"testing"
)

func TestSetupHandlers(t *testing.T) {
	ids := []string{"req-1", "req-2", "req-3"}
	handlers, cleanup := SetupHandlers(ids)
	cleanup()

	for i, h := range handlers {
		if h.Ctx.Err() != context.Canceled {
			t.Fatalf("handler %d (%s) ctx.Err() = %v, want context.Canceled", i, h.ID, h.Ctx.Err())
		}
	}
}

func TestSetupHandlersBuggyOnlyCancelsLast(t *testing.T) {
	ids := []string{"req-1", "req-2", "req-3"}
	handlers, cleanup := SetupHandlersBuggy(ids)
	cleanup()

	for i := 0; i < len(handlers)-1; i++ {
		if handlers[i].Ctx.Err() != nil {
			t.Fatalf("handler %d (%s) ctx.Err() = %v, want nil (never cancelled)", i, handlers[i].ID, handlers[i].Ctx.Err())
		}
	}
	last := handlers[len(handlers)-1]
	if last.Ctx.Err() != context.Canceled {
		t.Fatalf("handler %d (%s) ctx.Err() = %v, want context.Canceled", len(handlers)-1, last.ID, last.Ctx.Err())
	}
}

func TestSetupHandlersSingleHandlerEdgeCase(t *testing.T) {
	// With exactly one handler, the shared-variable bug has no earlier
	// handler to leak away from, so both variants must behave identically.
	ids := []string{"solo"}

	handlers, cleanup := SetupHandlers(ids)
	cleanup()
	if handlers[0].Ctx.Err() != context.Canceled {
		t.Fatalf("correct: ctx.Err() = %v, want context.Canceled", handlers[0].Ctx.Err())
	}

	buggyHandlers, buggyCleanup := SetupHandlersBuggy(ids)
	buggyCleanup()
	if buggyHandlers[0].Ctx.Err() != context.Canceled {
		t.Fatalf("buggy: ctx.Err() = %v, want context.Canceled", buggyHandlers[0].Ctx.Err())
	}
}
```

## Review

The setup is correct when running cleanup cancels every handler's own
context, no matter how many handlers exist — `TestSetupHandlers` is that
guarantee, and `TestSetupHandlersBuggyOnlyCancelsLast` is its mirror image: a
shared cancel-func variable means only the last handler's context is ever
cancelled, and every earlier one leaks. The mechanism to keep straight is
that a goroutine's parameters are copied at the `go` statement — passing
`cancel` freezes that goroutine's own func regardless of what the setup loop
does afterward, while closing over a variable means the goroutine reads
whatever it holds when cleanup actually runs. The barrier in the buggy
version does not change the bug, it only pins the outcome so the test is
deterministic instead of a flake. Run `go test -race`; the shared variable is
only written before the barrier and only read after it, so there is no data
race, just a logic bug.

## Resources

- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) — CancelFunc is safe to call from multiple goroutines and multiple times.
- [Go spec: Go statements](https://go.dev/ref/spec#Go_statements) — function arguments are evaluated when the `go` statement executes, not when the goroutine runs.
- [Go blog: Fixing for loops in Go 1.22](https://go.dev/blog/loopvar-preview) — the general shape of capturing something that keeps changing after registration.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-event-context-enrichment-shared-pointer-mutation.md](19-event-context-enrichment-shared-pointer-mutation.md) | Next: [21-metric-aggregator-per-key-buffer-write-race.md](21-metric-aggregator-per-key-buffer-write-race.md)
