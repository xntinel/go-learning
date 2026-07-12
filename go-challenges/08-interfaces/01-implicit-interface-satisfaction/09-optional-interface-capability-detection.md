# Exercise 9: Capability Detection: Closing Dependencies Only If They Implement io.Closer

A graceful-shutdown path holds its dependencies as narrow interfaces, but only
some of them own a resource that must be closed. Widening the primary interface to
add `Close` on every implementation is wrong; the right tool is an optional-
interface type assertion: `if c, ok := dep.(io.Closer); ok { c.Close() }`. This
module builds a `Service.Shutdown` that closes exactly the closable dependencies
and aggregates their errors.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests.

## What you'll build

```text
shutdown/                     independent module: example.com/shutdown
  go.mod                      go 1.26
  service.go                  Store interface; Service.Shutdown probing io.Closer; errors.Join
  cmd/
    demo/
      main.go                 runnable demo: mix of closable and non-closable deps
  service_test.go             closer gets Close; non-closer skipped; errors aggregated
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: a `Service` holding several `Store` dependencies whose `Shutdown` closes only those that also implement `io.Closer`, aggregating errors with `errors.Join`.
- Test: one fake that implements `io.Closer` and one that does not; assert the closer's `Close` ran, the non-closer was skipped without panic, and a close error surfaces via `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/01-implicit-interface-satisfaction/09-optional-interface-capability-detection/cmd/demo
cd go-solutions/08-interfaces/01-implicit-interface-satisfaction/09-optional-interface-capability-detection
```

### Optional interfaces keep the primary interface narrow

`Store` is the narrow interface the service uses at runtime — one method, `Get`.
Some `Store` implementations own a resource: a database-backed store holds a
connection pool, a file-backed store holds a file handle. Those must be closed at
shutdown. An in-memory store owns nothing and has no `Close`.

The wrong fix is to add `Close() error` to `Store`, forcing every implementation
(including the in-memory one and every test fake) to grow a `Close` method it does
not need — that is exactly the interface-widening the whole lesson warns against.
The right fix uses the fact that satisfaction is structural: a value may implement
*more* than the interface it is typed as. `Shutdown` probes for the extra
capability with a comma-ok type assertion:

```go
if c, ok := dep.(io.Closer); ok {
	err := c.Close()
}
```

`dep` is a `Store`; the assertion asks "does the concrete value behind this Store
also implement `io.Closer`?" If yes, `ok` is true and `c` is the value viewed as an
`io.Closer`, so `c.Close()` calls the real method. If no, `ok` is false and the
dependency is skipped — no panic, because the comma-ok form never panics (the bare
form `dep.(io.Closer)` would). This is exactly how the standard library probes for
`http.Flusher`, `http.Hijacker`, and `io.ReaderFrom` without widening the base
interface.

`Shutdown` collects each `Close` error and returns them combined with
`errors.Join`, which produces a single error wrapping all non-nil inputs (and
returns nil if all are nil). A caller can still match any individual failure with
`errors.Is`, because `errors.Join`'s result reports true for `Is` against any of
its wrapped errors.

Create `service.go`:

```go
package shutdown

import (
	"errors"
	"io"
)

// Store is the narrow runtime interface: one method. Some implementations also
// own a resource and implement io.Closer; the interface deliberately does not
// require Close.
type Store interface {
	Get(key string) (string, error)
}

// Service holds its dependencies as narrow Stores. Only some are closable.
type Service struct {
	deps []Store
}

// NewService constructs a Service over the given dependencies.
func NewService(deps ...Store) *Service {
	return &Service{deps: deps}
}

// Shutdown closes every dependency that also implements io.Closer, skipping the
// rest, and returns the combined error via errors.Join (nil if none failed).
func (s *Service) Shutdown() error {
	var errs []error
	for _, dep := range s.deps {
		if c, ok := dep.(io.Closer); ok {
			if err := c.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/shutdown"
)

// dbStore owns a "connection" and is closable.
type dbStore struct{ closed bool }

func (d *dbStore) Get(string) (string, error) { return "row", nil }
func (d *dbStore) Close() error               { d.closed = true; return nil }

// memStore owns nothing and does not implement io.Closer.
type memStore struct{}

func (memStore) Get(string) (string, error) { return "cached", nil }

func main() {
	db := &dbStore{}
	svc := shutdown.NewService(db, memStore{})

	if err := svc.Shutdown(); err != nil {
		fmt.Println("shutdown error:", err)
		return
	}

	fmt.Printf("db closed: %v\n", db.closed)
	fmt.Println("mem store skipped without panic")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
db closed: true
mem store skipped without panic
```

### Tests

`closableStore` implements `io.Closer` and records that `Close` ran;
`plainStore` does not. `TestShutdownClosesOnlyClosers` asserts the closer was
closed and the non-closer was skipped without panic. `TestShutdownAggregatesErrors`
gives one closer a failing `Close` and asserts the combined error surfaces it via
`errors.Is`.

Create `service_test.go`:

```go
package shutdown

import (
	"errors"
	"testing"
)

type closableStore struct {
	closed   bool
	closeErr error
}

func (c *closableStore) Get(string) (string, error) { return "v", nil }
func (c *closableStore) Close() error {
	c.closed = true
	return c.closeErr
}

type plainStore struct{}

func (plainStore) Get(string) (string, error) { return "v", nil }

func TestShutdownClosesOnlyClosers(t *testing.T) {
	t.Parallel()

	closer := &closableStore{}
	svc := NewService(closer, plainStore{})

	if err := svc.Shutdown(); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	if !closer.closed {
		t.Fatal("closable dependency was not closed")
	}
	// The plain store has no Close and must have been skipped without panic;
	// reaching here proves no panic occurred.
}

func TestShutdownAggregatesErrors(t *testing.T) {
	t.Parallel()

	errDB := errors.New("db close failed")
	failing := &closableStore{closeErr: errDB}
	ok := &closableStore{}
	svc := NewService(failing, plainStore{}, ok)

	err := svc.Shutdown()
	if err == nil {
		t.Fatal("Shutdown = nil, want aggregated error")
	}
	if !errors.Is(err, errDB) {
		t.Fatalf("errors.Is(err, errDB) = false; err = %v", err)
	}
	if !ok.closed {
		t.Fatal("the healthy closer should still have been closed")
	}
}

func TestShutdownNoClosersReturnsNil(t *testing.T) {
	t.Parallel()

	svc := NewService(plainStore{}, plainStore{})
	if err := svc.Shutdown(); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
}
```

## Review

The shutdown path is correct when it closes exactly the dependencies that
implement `io.Closer`, skips the rest without panicking, and aggregates errors so
one failing `Close` neither hides the others nor stops them from running. The
comma-ok assertion is load-bearing: the bare form `dep.(io.Closer)` panics on a
non-closer, so the `, ok` is mandatory. The mistake this module inoculates against
is widening `Store` to include `Close`, which forces every in-memory
implementation and test fake to add a `Close` they do not need — the optional-
interface probe keeps the runtime interface at one method while still cleaning up
resources at shutdown. `errors.Join` returns nil when nothing failed, so the happy
path is unchanged. Run `go test -race` to confirm.

## Resources

- [`io.Closer`](https://pkg.go.dev/io#Closer) — the one-method close contract.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining multiple errors, with `errors.Is` matching any wrapped one.
- [Go Specification: Type assertions](https://go.dev/ref/spec#Type_assertions) — the comma-ok form that never panics.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-decorator-preserves-interface.md](10-decorator-preserves-interface.md)
