# Exercise 16: Connection Pool Leaks Handles When Validation Errors Occur Before Defer Registration

**Nivel: Intermedio** — validacion rapida (un test corto).

A pooled-connection wrapper that acquires a handle, runs a health check
against it, and only *then* schedules `defer pool.Release(handle)` has a
bug that only shows up under load: every handle that fails the health
check is acquired and never returned, so the pool's free list shrinks by
one on every rejected connection until every caller blocks or gets
`ErrPoolExhausted`, even though nothing actually crashed. This module is
fully self-contained: its own `go mod init`, all code inline, its own demo
and tests.

## What you'll build

```text
connpool/                   independent module: example.com/connection-pool-defer-before-validation
  go.mod
  connpool.go                Pool, Handle, WithValidatedConnection
  cmd/
    demo/
      main.go                runnable demo: failed validation, then a successful reuse
  connpool_test.go            failed validation still frees the handle, success path frees it too
```

- Files: `connpool.go`, `cmd/demo/main.go`, `connpool_test.go`.
- Implement: `Pool` (`Acquire`, `Release`, `Available`) and `WithValidatedConnection(*Pool, validate, use func(*Handle) error) error` that always returns the acquired handle to the pool.
- Test: force `validate` to fail and assert `Pool.Available()` is unchanged and a subsequent `Acquire` still succeeds; a second test asserts the same on the success path.
- Verify: `go test -count=1 ./...`.

### Why the defer has to be registered right after Acquire, not after validate

`Acquire` hands ownership of a `*Handle` to the caller the instant it
returns without an error; from that point on, *something* on every code
path has to give it back, or the pool's free list only ever shrinks. The
tempting-looking bug puts the release after the health check, because
"only release handles that passed validation" reads like reasonable
intent:

```go
func WithValidatedConnection(p *Pool, validate func(*Handle) error, use func(*Handle) error) error {
	h, err := p.Acquire()
	if err != nil {
		return err
	}

	if err := validate(h); err != nil {
		return err // BUG: h was already acquired, but no defer has been registered yet
	}
	defer p.Release(h)

	return use(h)
}
```

The mistake is treating `Release` as if it were about the handle's
*health* rather than about the pool's own bookkeeping. A pool's `Release`
is "I am done borrowing this," not "this connection is fine" — those are
two separate concerns, and only the caller's application logic (a
reconnect, a circuit breaker) should decide what to do with an unhealthy
handle *after* it has been returned. Registering `defer p.Release(h)`
immediately after a successful `Acquire`, before any code that can return
early, guarantees the pool's accounting is correct on every exit path —
validation failure, `use` failure, or success — while `validate`'s error is
still propagated to the caller exactly as before.

Create `connpool.go`:

```go
package connpool

import "errors"

// ErrPoolExhausted is returned when Acquire is called and no handle is
// currently available.
var ErrPoolExhausted = errors.New("connection pool exhausted")

// Handle is a stand-in for a pooled database connection.
type Handle struct {
	ID int
}

// Pool is a fixed-size pool of handles backed by a buffered channel.
type Pool struct {
	handles chan *Handle
}

// NewPool creates a pool pre-populated with size handles.
func NewPool(size int) *Pool {
	p := &Pool{handles: make(chan *Handle, size)}
	for i := 0; i < size; i++ {
		p.handles <- &Handle{ID: i}
	}
	return p
}

// Acquire takes a handle from the pool without blocking, returning
// ErrPoolExhausted if none is available.
func (p *Pool) Acquire() (*Handle, error) {
	select {
	case h := <-p.handles:
		return h, nil
	default:
		return nil, ErrPoolExhausted
	}
}

// Release returns a handle to the pool.
func (p *Pool) Release(h *Handle) {
	p.handles <- h
}

// Available reports how many handles are currently free.
func (p *Pool) Available() int { return len(p.handles) }

// WithValidatedConnection acquires a handle, validates it, and runs use
// against it -- always releasing the handle back to the pool, including
// when validate itself reports the handle unhealthy.
func WithValidatedConnection(p *Pool, validate func(*Handle) error, use func(*Handle) error) error {
	h, err := p.Acquire()
	if err != nil {
		return err
	}
	defer p.Release(h)

	if err := validate(h); err != nil {
		return err
	}
	return use(h)
}
```

### The runnable demo

The demo runs a validation failure against a single-handle pool, then a
successful call, printing `Available()` after each to show the handle
count never drops.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/connection-pool-defer-before-validation"
)

func main() {
	p := connpool.NewPool(1)
	fmt.Println("available before:", p.Available())

	err := connpool.WithValidatedConnection(p,
		func(h *connpool.Handle) error {
			return fmt.Errorf("handle %d failed health check", h.ID)
		},
		func(h *connpool.Handle) error { return nil },
	)
	fmt.Println("first call error:", err)
	fmt.Println("available after failed validation:", p.Available())

	err = connpool.WithValidatedConnection(p,
		func(h *connpool.Handle) error { return nil },
		func(h *connpool.Handle) error {
			fmt.Println("using handle", h.ID)
			return nil
		},
	)
	fmt.Println("second call error:", err)
	fmt.Println("available after success:", p.Available())
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
available before: 1
first call error: handle 0 failed health check
available after failed validation: 1
using handle 0
second call error: <nil>
available after success: 1
```

### Tests

Create `connpool_test.go`:

```go
package connpool

import (
	"errors"
	"testing"
)

func TestFailedValidationStillReleasesHandle(t *testing.T) {
	p := NewPool(1)

	wantErr := errors.New("unhealthy")
	err := WithValidatedConnection(p,
		func(h *Handle) error { return wantErr },
		func(h *Handle) error { return nil },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}

	if got := p.Available(); got != 1 {
		t.Fatalf("Available() = %d, want 1 (handle must be released even when validation fails)", got)
	}

	// The pool must still be usable: a second acquire must not see it exhausted.
	if _, err := p.Acquire(); err != nil {
		t.Fatalf("Acquire() after failed validation = %v, want nil", err)
	}
}

func TestSuccessfulUseReleasesHandle(t *testing.T) {
	p := NewPool(1)

	var usedID int
	err := WithValidatedConnection(p,
		func(h *Handle) error { return nil },
		func(h *Handle) error { usedID = h.ID; return nil },
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if usedID != 0 {
		t.Fatalf("usedID = %d, want 0", usedID)
	}
	if got := p.Available(); got != 1 {
		t.Fatalf("Available() = %d, want 1", got)
	}
}
```

Run: `go test -count=1 ./...`.

## Review

`WithValidatedConnection` is correct when `Pool.Available()` is unchanged
after every single call, whether validation fails, `use` fails, or the
whole thing succeeds — because exactly one `defer p.Release(h)` is
registered on every path that successfully acquired a handle, and it is
registered *before* any code that can return early. The mistake this
design avoids is reading `Release` as a verdict on the handle's health
rather than as returning a borrowed resource to its owner: those are
different responsibilities, and conflating them is what tempts the defer
into being written after the check it seems to depend on.

## Resources

- [Effective Go: Defer](https://go.dev/doc/effective_go#defer) — deferred calls execute in LIFO order when the surrounding function returns, regardless of which return statement fired.
- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — a `defer` is evaluated and registered at the point it is executed, not retroactively for earlier code.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-request-timeout-select-race.md](15-request-timeout-select-race.md) | Next: [17-polling-retry-unhandled-error-case.md](17-polling-retry-unhandled-error-case.md)
