# Exercise 5: Retryable initialization that swaps in a fresh Once after failure

Here is the outage the panic-is-done contract causes and how to fix it. A naive
`sync.Once` around a network dial caches the *first* result forever — including a
transient failure — leaving the singleton dead on arrival with no retry. The
production fix is a small wrapper that installs a *fresh* `Once` after a failed
attempt, giving bounded retry without ever breaking exactly-once on success. This
exercise builds that wrapper.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
resettable-init-retry/        module: example.com/resettable-init-retry
  go.mod
  dialer.go                    type Conn, LazyDialer; New, Get, Attempts
  cmd/
    demo/
      main.go                  runnable demo: fail-twice-then-succeed
  dialer_test.go               retry-to-success, cap on permanent failure, concurrency
```

- Files: `dialer.go`, `cmd/demo/main.go`, `dialer_test.go`.
- Implement: a `LazyDialer` holding a `sync.Mutex`, a `*sync.Once`, the captured result/error, an attempt counter, an attempt cap, and a pluggable `dial` func; `Get()` runs `dial` once per `Once` and, on failure below the cap, installs a fresh `Once` so the next attempt retries.
- Test: a dial that fails twice then succeeds returns the resource and runs `dial` exactly 3 times; a permanently-failing dial stops at the cap and returns the last error; concurrent `Get` during the failing window never double-runs a successful attempt.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p resettable-init-retry/cmd/demo
cd resettable-init-retry
go mod init example.com/resettable-init-retry
```

### Retry requires a new Once instance, guarded

A `Once` cannot be un-done; retry-after-failure therefore means throwing the
failed `Once` away and installing a fresh one for the next attempt. The wrapper
holds the current `Once` behind a pointer under a `Mutex`. `Get` snapshots the
current `*sync.Once`, calls `Do` on it (so exactly one goroutine per generation
runs `dial`), then takes the mutex to inspect the outcome. On success it returns
the resource. On failure, and only if this goroutine is the one whose snapshot
still matches the current pointer (so exactly one goroutine per failed generation
acts) and the attempt cap is not yet reached, it swaps in a fresh `Once`. The
pointer-equality guard is what bounds retries under concurrency: without it, every
goroutine that saw the failure would install its own fresh `Once`, and a hundred
concurrent callers could trigger a hundred dials. With it, generations advance in
lockstep and the total number of dials is bounded by the cap.

`Get` loops: after installing a fresh `Once`, it re-reads the current one and
tries again, so a single `Get` call drives the retries to completion (success or
cap). The captured result and error are written *inside* the closure under the
same mutex the readers use, so there is no race between a slow waiter of an old
generation reading the fields and a new generation's closure writing them.

Create `dialer.go`:

```go
package dialer

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ErrExhausted is returned when the attempt cap is reached without success. The
// underlying dial error is wrapped so callers can inspect the real cause.
var ErrExhausted = errors.New("dialer: attempts exhausted")

// Conn is the resource the dialer produces.
type Conn struct {
	Addr string
}

// LazyDialer builds a Conn lazily with bounded retry. A failed attempt installs a
// fresh Once so the next Get retries; a successful attempt freezes forever. Hold
// it behind a pointer.
type LazyDialer struct {
	mu          sync.Mutex
	once        *sync.Once
	res         *Conn
	err         error
	maxAttempts int
	attempts    atomic.Int64
	dial        func() (*Conn, error)
}

// New returns a dialer that calls dial (up to maxAttempts) to build the Conn.
func New(maxAttempts int, dial func() (*Conn, error)) *LazyDialer {
	return &LazyDialer{once: &sync.Once{}, maxAttempts: maxAttempts, dial: dial}
}

// Get returns the Conn, retrying a failed dial with a fresh Once up to the cap.
// Once a dial succeeds, no further dials run and every caller gets that Conn.
func (d *LazyDialer) Get() (*Conn, error) {
	for {
		d.mu.Lock()
		o := d.once
		d.mu.Unlock()

		o.Do(func() {
			d.attempts.Add(1)
			res, err := d.dial()
			d.mu.Lock()
			d.res, d.err = res, err
			d.mu.Unlock()
		})

		d.mu.Lock()
		res, err := d.res, d.err
		if err == nil {
			d.mu.Unlock()
			return res, nil
		}
		// This generation failed. Only the goroutine whose snapshot still
		// matches the current Once installs a fresh one, and only under the cap.
		if o == d.once {
			if d.attempts.Load() >= int64(d.maxAttempts) {
				d.mu.Unlock()
				return nil, errors.Join(ErrExhausted, err)
			}
			d.once = &sync.Once{}
		}
		d.mu.Unlock()
	}
}

// Attempts reports how many times dial has been invoked.
func (d *LazyDialer) Attempts() int64 {
	return d.attempts.Load()
}
```

`errors.Join(ErrExhausted, err)` produces an error that matches both
`errors.Is(_, ErrExhausted)` and `errors.Is(_, err)`, so a caller can detect
exhaustion and still see the underlying cause.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/resettable-init-retry"
)

func main() {
	fails := 2
	d := dialer.New(5, func() (*dialer.Conn, error) {
		if fails > 0 {
			fails--
			return nil, errors.New("connection refused")
		}
		return &dialer.Conn{Addr: "db:5432"}, nil
	})

	conn, err := d.Get()
	fmt.Println("err:", err)
	fmt.Println("addr:", conn.Addr)
	fmt.Println("dials:", d.Attempts())

	// A second Get after success does not dial again.
	_, _ = d.Get()
	fmt.Println("dials after 2nd Get:", d.Attempts())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
err: <nil>
addr: db:5432
dials: 3
dials after 2nd Get: 3
```

### Tests

`TestRetryToSuccess` injects a dial that fails twice then succeeds and asserts the
returned `Conn` plus exactly 3 dials. `TestExhaustion` injects a permanently
failing dial and asserts `Get` stops at the cap and returns an error that is both
`ErrExhausted` and the underlying cause. `TestConcurrentGet` fans many goroutines
at a fail-twice-then-succeed dial (its counter guarded by a mutex) and asserts the
total dial count is exactly 3 — generations advanced in lockstep, no double-run.

Create `dialer_test.go`:

```go
package dialer

import (
	"errors"
	"sync"
	"testing"
)

func TestRetryToSuccess(t *testing.T) {
	t.Parallel()

	fails := 2
	d := New(5, func() (*Conn, error) {
		if fails > 0 {
			fails--
			return nil, errors.New("refused")
		}
		return &Conn{Addr: "ok:1"}, nil
	})

	conn, err := d.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if conn.Addr != "ok:1" {
		t.Fatalf("Addr = %q, want ok:1", conn.Addr)
	}
	if got := d.Attempts(); got != 3 {
		t.Fatalf("Attempts() = %d, want 3", got)
	}
}

func TestExhaustion(t *testing.T) {
	t.Parallel()

	cause := errors.New("always refused")
	d := New(3, func() (*Conn, error) {
		return nil, cause
	})

	_, err := d.Get()
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want it to wrap the cause", err)
	}
	if got := d.Attempts(); got != 3 {
		t.Fatalf("Attempts() = %d, want cap 3", got)
	}
}

func TestConcurrentGet(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	fails := 2
	d := New(10, func() (*Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			return nil, errors.New("refused")
		}
		return &Conn{Addr: "shared:1"}, nil
	})

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			conn, err := d.Get()
			if err != nil || conn.Addr != "shared:1" {
				t.Errorf("Get = %v, %v; want shared:1, nil", conn, err)
			}
		}()
	}
	wg.Wait()

	if got := d.Attempts(); got != 3 {
		t.Fatalf("Attempts() = %d, want exactly 3 (2 fail + 1 success)", got)
	}
}
```

## Review

The dialer is correct when a transient failure retries but a success freezes.
`TestRetryToSuccess` proves fail-fail-succeed costs exactly 3 dials;
`TestExhaustion` proves a permanent failure stops at the cap and returns an error
that is both `ErrExhausted` and the cause via `errors.Join`. The hard property is
`TestConcurrentGet`: 50 goroutines racing through the failing window still produce
exactly 3 dials, because the pointer-equality guard (`o == d.once`) lets only one
goroutine per failed generation install a fresh `Once`. Drop that guard and each
failed generation would fan out into many dials. This is the antidote to the
"`Once` cached a dead connection forever" trap from the concepts file: retry lives
in the wrapper, not in re-calling `Do` on a spent `Once`. Run `go test -race`.

## Resources

- [sync.Once — pkg.go.dev](https://pkg.go.dev/sync#Once)
- [errors.Join — pkg.go.dev](https://pkg.go.dev/errors#Join)
- [The Go Memory Model: Once](https://go.dev/ref/mem#once)

---

Prev: [04-lazy-pool-provider.md](04-lazy-pool-provider.md) | Back to [00-concepts.md](00-concepts.md) | Next: [06-panic-safe-init.md](06-panic-safe-init.md)
