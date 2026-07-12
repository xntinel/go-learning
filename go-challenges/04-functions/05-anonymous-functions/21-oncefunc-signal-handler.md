# Exercise 21: Signal Handler Registration Wrapped in sync.OnceFunc

**Nivel: Intermedio** — validacion rapida (un test corto).

Registering an OS signal handler must happen exactly once no matter how many
independent packages or goroutines all want it present — calling `signal.Notify`
twice does not crash, but coordinating "who registers it" by hand across a codebase
is exactly the kind of bookkeeping `sync.OnceFunc` (Go 1.21) exists to remove. This
module wraps a registration closure in `sync.OnceFunc` so any number of concurrent
callers can call `EnsureRegistered` and the real setup still runs exactly once.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
signalinit/                   module example.com/signalinit
  go.mod
  signalinit.go                 Registry, New (wraps setup in sync.OnceFunc), EnsureRegistered
  signalinit_test.go             runs once sequentially, runs once under concurrent callers, independent registries
  cmd/demo/main.go              call EnsureRegistered three times
```

- Files: `signalinit.go`, `signalinit_test.go`, `cmd/demo/main.go`.
- Implement: `Registry` holding a `sync.OnceFunc`-wrapped setup closure; `New(setup)` wrapping it; `EnsureRegistered()` calling the wrapped function.
- Test: calling `EnsureRegistered` several times sequentially runs `setup` exactly once; calling it from many concurrent goroutines still runs `setup` exactly once; two separate `Registry` values register independently. Under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/21-oncefunc-signal-handler/cmd/demo
cd go-solutions/04-functions/05-anonymous-functions/21-oncefunc-signal-handler
go mod edit -go=1.24
```

### Wrapping the closure once, at construction time

`sync.OnceFunc(f func()) func()` — added in Go 1.21 — takes an ordinary function and
returns a new one that runs `f` exactly once, however many times the returned
function is called, and however many goroutines call it concurrently; every caller,
including the ones that lose the race to be first, blocks until that one run
completes. `New` does the wrapping exactly once, at construction: `setup` — the
anonymous closure a caller supplies, which in production would call
`signal.Notify(ch, os.Interrupt)` and stash `ch` somewhere — is wrapped
immediately and stored, so `EnsureRegistered` is just `r.setup()` forever after.
This is the same shape as `sync.Once.Do(f)`, but as a value instead of a method
call, which is what lets `Registry` store "the one-time setup" as a single field
instead of a `sync.Once` plus a separate function to guard.

Create `signalinit.go`:

```go
package signalinit

import "sync"

// Registry wraps a one-time setup routine — in production, registering an
// os/signal handler with signal.Notify — behind sync.OnceFunc so it runs
// exactly once no matter how many goroutines call EnsureRegistered
// concurrently. Every library that needs the handler present can call
// EnsureRegistered on its own initialization path without any of them
// having to coordinate who "owns" doing the actual registration.
type Registry struct {
	setup func()
}

// New wraps setup (an anonymous function performing the real, one-time
// registration) with sync.OnceFunc. sync.OnceFunc itself handles the
// concurrency: only the first call to the returned function actually runs
// setup, and every call — concurrent or not — blocks until that first run
// completes.
func New(setup func()) *Registry {
	return &Registry{setup: sync.OnceFunc(setup)}
}

// EnsureRegistered runs the wrapped setup exactly once across the
// Registry's lifetime, regardless of how many times or how many goroutines
// call it.
func (r *Registry) EnsureRegistered() {
	r.setup()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync/atomic"

	"example.com/signalinit"
)

func main() {
	var registered atomic.Int32

	reg := signalinit.New(func() {
		// In production: signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		registered.Add(1)
	})

	reg.EnsureRegistered()
	reg.EnsureRegistered()
	reg.EnsureRegistered()

	fmt.Println("registration count:", registered.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
registration count: 1
```

### Tests

`TestEnsureRegisteredRunsSetupOnce` calls `EnsureRegistered` three times sequentially
and checks the counter is 1. `TestEnsureRegisteredIsSafeUnderConcurrentCallers` is
the load-bearing test: a hundred goroutines all call `EnsureRegistered` at once, and
the counter must still land on exactly 1 — the case `sync.OnceFunc` exists to
guarantee under `-race`. `TestSeparateRegistriesRegisterIndependently` checks that
two `Registry` values each get their own one-time run.

Create `signalinit_test.go`:

```go
package signalinit

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestEnsureRegisteredRunsSetupOnce(t *testing.T) {
	t.Parallel()
	var count atomic.Int32
	reg := New(func() { count.Add(1) })

	reg.EnsureRegistered()
	reg.EnsureRegistered()
	reg.EnsureRegistered()

	if got := count.Load(); got != 1 {
		t.Fatalf("setup ran %d times, want exactly 1", got)
	}
}

func TestEnsureRegisteredIsSafeUnderConcurrentCallers(t *testing.T) {
	t.Parallel()
	var count atomic.Int32
	reg := New(func() { count.Add(1) })

	const callers = 100
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			reg.EnsureRegistered()
		}()
	}
	wg.Wait()

	if got := count.Load(); got != 1 {
		t.Fatalf("setup ran %d times across %d concurrent callers, want exactly 1", got, callers)
	}
}

func TestSeparateRegistriesRegisterIndependently(t *testing.T) {
	t.Parallel()
	var countA, countB atomic.Int32
	regA := New(func() { countA.Add(1) })
	regB := New(func() { countB.Add(1) })

	regA.EnsureRegistered()
	regA.EnsureRegistered()
	regB.EnsureRegistered()

	if countA.Load() != 1 || countB.Load() != 1 {
		t.Fatalf("countA=%d countB=%d, want 1 and 1", countA.Load(), countB.Load())
	}
}
```

## Review

The registry is correct when `setup` runs exactly once per `Registry`, whether
`EnsureRegistered` is called once, three times sequentially, or from a hundred
goroutines at once — the concurrent-callers test is the one that would fail first if
`New` used a plain `sync.Once` incorrectly (for example, checking a bool without a
lock) instead of delegating entirely to `sync.OnceFunc`. Wrapping the closure inside
`New`, rather than requiring every caller to remember to guard their own call to
`setup`, is what makes the "exactly once" guarantee a property of the `Registry`
itself instead of a convention callers have to uphold.

## Resources

- [sync.OnceFunc](https://pkg.go.dev/sync#OnceFunc)
- [os/signal package](https://pkg.go.dev/os/signal)
- [Go 1.21 release notes: sync package additions](https://go.dev/doc/go1.21#sync)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-webhook-retry-callback.md](20-webhook-retry-callback.md) | Next: [22-stream-buffer-iife.md](22-stream-buffer-iife.md)
