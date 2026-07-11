# Exercise 1: Lazy service connect with a captured init error

A service that dials its backend the first time it is used, exactly once, no
matter how many request goroutines race to trigger it, and surfaces any init
error to every caller. This is the lazy-singleton shape in its smallest honest
form, and it forces the two things `Once` cannot do for you: capture an error
(because `Do` returns nothing) and observe that the *first* caller wins.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
once-init-service/            module: example.com/once-init-service
  go.mod
  service.go                  ErrEmptyAddress; type Service; Connect, Addr
  cmd/
    demo/
      main.go                 runnable demo: connect, second connect is a no-op
  service_test.go             first-caller-wins, error path, 100-goroutine race
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: a `Service` holding `sync.Once` plus a `conn` and an `err` field; `Connect(addr)` runs the init closure once and every caller reads `err`; `Addr()` exposes the unexported connection string.
- Test: first-caller-wins (a second `Connect` with a different address is a no-op), the empty-address path returns `ErrEmptyAddress` via `errors.Is`, and 100 concurrent `Connect` calls all observe the same address.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p once-init-service/cmd/demo
cd once-init-service
go mod init example.com/once-init-service
```

### Why capture the error in a field

`Once.Do` takes a `func()` and returns nothing, so a fallible init cannot hand an
error back the ordinary way. The pattern is to write the outcome — the connection
string on success, or a sentinel error on failure — into fields of the `Service`
inside the closure, and have every caller read those fields after `Do` returns.
That read is race-free because the Go memory model guarantees the closure's writes
"synchronize before" the return from any `Do` call on the same `Once`. The
first-caller-wins property falls straight out of the contract: whichever goroutine
runs the closure fixes `conn` and `err`, and every later `Connect` — even one
passing a different address — is a no-op that returns the already-captured `err`
and leaves the original address standing. That is often surprising to people new
to `Once`: the argument to the second `Connect` is silently ignored, because `Do`
keys on the instance, not on what you pass it.

`ErrEmptyAddress` is a package-level sentinel wrapped is unnecessary here (the
closure returns it directly), but it is an `errors.New` value so callers assert it
with `errors.Is`. The `Addr` accessor exists because `cmd/demo` is a separate
`package main` and cannot read the unexported `conn`; a small exported getter is
the right escape hatch rather than exporting the raw field.

Create `service.go`:

```go
package service

import (
	"errors"
	"sync"
)

// ErrEmptyAddress is returned by Connect when the address is empty. It is
// captured once inside the init closure and observed by every caller.
var ErrEmptyAddress = errors.New("service: empty address")

// Service lazily connects to its backend exactly once. The zero value is not
// usable across a copy: hold it behind a pointer (go vet copylocks enforces it).
type Service struct {
	once sync.Once
	conn string
	err  error
}

// Connect runs the init closure exactly once. The first caller fixes the
// outcome; later calls, even with a different addr, are no-ops that return the
// captured error. An empty addr captures ErrEmptyAddress and leaves conn empty.
func (s *Service) Connect(addr string) error {
	s.once.Do(func() {
		if addr == "" {
			s.err = ErrEmptyAddress
			return
		}
		s.conn = "connected-to-" + addr
	})
	return s.err
}

// Addr reports the connection string set by the winning Connect, or "" if none
// succeeded. The read is safe because it only follows a Do call on s.once.
func (s *Service) Addr() string {
	return s.conn
}
```

### The runnable demo

The demo connects once, then connects again with a different address to show the
second call is a no-op, then connects an empty address on a fresh service to show
the captured error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/once-init-service"
)

func main() {
	s := &service.Service{}
	fmt.Println("connect ok:", s.Connect("db:5432"))
	fmt.Println("addr:", s.Addr())

	// A second Connect with a different address is a no-op.
	fmt.Println("second ok:", s.Connect("other:9090"))
	fmt.Println("addr still:", s.Addr())

	failing := &service.Service{}
	fmt.Println("empty err:", failing.Connect(""))
	fmt.Printf("empty addr: %q\n", failing.Addr())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
connect ok: <nil>
addr: connected-to-db:5432
second ok: <nil>
addr still: connected-to-db:5432
empty err: service: empty address
empty addr: ""
```

### Tests

The hard test is `TestConcurrentConnect`: 100 goroutines each call `Connect` with
a *different* address, and the assertion is that every goroutine observes the same
final address — whichever closure won. An `atomic.Pointer[string]` records the
first address seen with `CompareAndSwap(nil, ...)`, and every goroutine checks
that what it reads matches. If `Once` did not publish the winner's write to all
callers, this would tear under `-race`.

Create `service_test.go`:

```go
package service

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConnectFirstCallerWins(t *testing.T) {
	t.Parallel()

	s := &Service{}
	if err := s.Connect("db:5432"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := s.Addr(); got != "connected-to-db:5432" {
		t.Fatalf("Addr() = %q, want connected-to-db:5432", got)
	}
	if err := s.Connect("other:9090"); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if got := s.Addr(); got != "connected-to-db:5432" {
		t.Fatalf("Addr() = %q, second Connect overwrote it", got)
	}
}

func TestConnectEmptyAddress(t *testing.T) {
	t.Parallel()

	s := &Service{}
	if err := s.Connect(""); !errors.Is(err, ErrEmptyAddress) {
		t.Fatalf("err = %v, want ErrEmptyAddress", err)
	}
	if got := s.Addr(); got != "" {
		t.Fatalf("Addr() = %q, want empty", got)
	}
	// The empty result is cached: a later good address is still a no-op.
	if err := s.Connect("db:5432"); !errors.Is(err, ErrEmptyAddress) {
		t.Fatalf("err = %v, want the cached ErrEmptyAddress", err)
	}
}

func TestConcurrentConnect(t *testing.T) {
	t.Parallel()

	const goroutines = 100
	s := &Service{}
	var wg sync.WaitGroup
	var seen atomic.Pointer[string]

	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			if err := s.Connect(fmt.Sprintf("host-%d", i)); err != nil {
				t.Errorf("Connect: %v", err)
				return
			}
			cur := s.Addr()
			seen.CompareAndSwap(nil, &cur)
			if prev := *seen.Load(); prev != cur {
				t.Errorf("Addr changed across goroutines: %q vs %q", prev, cur)
			}
		}()
	}
	wg.Wait()
}

func ExampleService() {
	s := &Service{}
	fmt.Println(s.Connect("a:1"))
	fmt.Println(s.Addr())
	// Output:
	// <nil>
	// connected-to-a:1
}
```

## Review

The service is correct when the outcome is fixed by the first caller and observed
identically by all. `Connect` writes `conn`/`err` only inside the closure, so a
second `Connect` with any argument returns the cached result and never overwrites
`Addr` — that is the first-caller-wins property, and it is what
`TestConnectFirstCallerWins` and the empty-address caching in
`TestConnectEmptyAddress` pin down. The concurrency test proves the happens-before
edge: 100 goroutines see one agreed address. The common trap this exercise guards
against is expecting the second `Connect(other)` to reconnect — it cannot, because
`Do` is keyed on the instance. Hold `Service` behind a pointer; a value copy would
carry a zeroed `Once` and re-dial. Run `go test -race` to confirm the unsynchronized
read in `Addr` is legal only because it follows `Do`.

## Resources

- [sync.Once — pkg.go.dev](https://pkg.go.dev/sync#Once)
- [The Go Memory Model: Once](https://go.dev/ref/mem#once)
- [errors.Is — pkg.go.dev](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-lazy-counter-start.md](02-lazy-counter-start.md)
