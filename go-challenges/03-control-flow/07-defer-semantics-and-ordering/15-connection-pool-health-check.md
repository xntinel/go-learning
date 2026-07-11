# Exercise 15: Connection Pool — Health Check and Quarantine via Deferred Return

**Nivel: Intermedio** — validacion rapida (un test corto).

A pooled database client that hands a connection back to the available list
without checking whether it survived the caller's use of it will eventually
hand that same broken connection to the next request, which fails for a
reason that has nothing to do with its own query. Production pools run a
liveness probe on every return path and file the connection into quarantine
instead of available when it fails. This module builds that return path with
two stacked defers and shows why their LIFO order is not incidental. The
module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
connpool/                   independent module: example.com/connection-pool-health-check
  go.mod                     go 1.24
  connpool.go                Conn, Pool (Use, Stats); quarantine-on-failed-health-check
  cmd/
    demo/
      main.go                runnable demo: one healthy use, one that kills the connection
  connpool_test.go           table test over healthy/unhealthy returns; exhausted-pool case
```

- Files: `connpool.go`, `cmd/demo/main.go`, `connpool_test.go`.
- Implement: `Conn`, `Pool` with `Use(fn func(*Conn) error) error` and `Stats() (available, quarantined, active int)`.
- Test: a table over a healthy return, an unhealthy return, and a pool-exhausted case.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/connection-pool-health-check/cmd/demo
cd ~/go-exercises/connection-pool-health-check
go mod init example.com/connection-pool-health-check
go mod edit -go=1.24
```

### Why the defer order matters

`Use` checks out a connection, then registers two defers before running the
caller's function: `releaseActive` first, `placeAfterHealthCheck` second.
LIFO means `placeAfterHealthCheck` runs first, at return — it pings the
connection and files it into `available` if the probe succeeds or
`quarantine` if it does not — and only after that has happened does
`releaseActive` run, dropping the active count. That ordering is what a
naive single-defer or wrong-order version gets wrong: if the active count
dropped *before* the connection was filed, a concurrent caller reacting to
"a connection just freed up" could observe a window where the connection is
counted as free but is sitting in neither list yet. Registering the count
decrement first so it fires last closes that window structurally, without a
lock spanning both steps.

Create `connpool.go`:

```go
package connpool

import "errors"

// ErrPoolExhausted means no connection was available to check out.
var ErrPoolExhausted = errors.New("connpool: no available connections")

// Conn is a pooled connection. alive is toggled by the health check that
// runs when the connection is returned to the pool.
type Conn struct {
	ID    int
	alive bool
}

// Ping simulates a liveness probe against the underlying connection.
func (c *Conn) Ping() bool { return c.alive }

// MarkDead simulates the connection observing a fatal I/O error while in use.
func (c *Conn) MarkDead() { c.alive = false }

// Pool hands out connections and quarantines any that fail their health
// check on return instead of putting them back in the available list.
type Pool struct {
	available   []*Conn
	quarantine  []*Conn
	activeCount int
}

// NewPool builds a pool seeded with conns, all starting healthy.
func NewPool(conns []*Conn) *Pool {
	for _, c := range conns {
		c.alive = true
	}
	return &Pool{available: append([]*Conn(nil), conns...)}
}

func (p *Pool) checkout() (*Conn, error) {
	if len(p.available) == 0 {
		return nil, ErrPoolExhausted
	}
	c := p.available[len(p.available)-1]
	p.available = p.available[:len(p.available)-1]
	p.activeCount++
	return c, nil
}

// placeAfterHealthCheck runs the liveness probe and files the connection
// into the available list or the quarantine list accordingly.
func (p *Pool) placeAfterHealthCheck(c *Conn) {
	if c.Ping() {
		p.available = append(p.available, c)
	} else {
		p.quarantine = append(p.quarantine, c)
	}
}

func (p *Pool) releaseActive() {
	p.activeCount--
}

// Use checks out a connection and runs fn against it. Two defers register
// the return path: placeAfterHealthCheck (registered second, so it runs
// first) files the connection into available or quarantine, and
// releaseActive (registered first, so it runs last) drops the active count.
// That LIFO order matters: by the time activeCount reflects the connection
// as no longer active, it has already been placed somewhere valid, so a
// concurrent caller that reacts to activeCount dropping never observes a
// connection that is neither active nor filed into available/quarantine.
func (p *Pool) Use(fn func(*Conn) error) error {
	c, err := p.checkout()
	if err != nil {
		return err
	}
	defer p.releaseActive()
	defer p.placeAfterHealthCheck(c)
	return fn(c)
}

// Stats reports the current sizes of each list, for tests and monitoring.
func (p *Pool) Stats() (available, quarantined, active int) {
	return len(p.available), len(p.quarantine), p.activeCount
}
```

### The runnable demo

The demo seeds a two-connection pool, runs one healthy use and one that kills
the connection mid-use, and prints the pool's final shape.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/connection-pool-health-check"
)

func main() {
	pool := connpool.NewPool([]*connpool.Conn{{ID: 1}, {ID: 2}})

	err := pool.Use(func(c *connpool.Conn) error {
		fmt.Printf("using conn %d\n", c.ID)
		return nil
	})
	fmt.Println("healthy use error:", err)

	err = pool.Use(func(c *connpool.Conn) error {
		fmt.Printf("using conn %d, then it dies\n", c.ID)
		c.MarkDead()
		return errors.New("write failed")
	})
	fmt.Println("unhealthy use error:", err)

	available, quarantined, active := pool.Stats()
	fmt.Printf("available=%d quarantined=%d active=%d\n", available, quarantined, active)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
using conn 2
healthy use error: <nil>
using conn 2, then it dies
unhealthy use error: write failed
available=1 quarantined=1 active=0
```

### Tests

`TestUseFilesConnectionByHealth` drives `Use` over a healthy and an
unhealthy return and asserts the pool's final shape; a second test confirms
an exhausted pool returns `ErrPoolExhausted` without ever calling `fn`.

Create `connpool_test.go`:

```go
package connpool

import (
	"errors"
	"testing"
)

func TestUseFilesConnectionByHealth(t *testing.T) {
	tests := []struct {
		name            string
		markDead        bool
		fnErr           error
		wantAvailable   int
		wantQuarantined int
	}{
		{"healthy connection returns to available", false, nil, 1, 0},
		{"unhealthy connection is quarantined", true, errors.New("write failed"), 0, 1},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pool := NewPool([]*Conn{{ID: 1}})

			err := pool.Use(func(c *Conn) error {
				if tc.markDead {
					c.MarkDead()
				}
				return tc.fnErr
			})

			if !errors.Is(err, tc.fnErr) {
				t.Fatalf("Use error = %v, want %v", err, tc.fnErr)
			}

			available, quarantined, active := pool.Stats()
			if available != tc.wantAvailable {
				t.Errorf("available = %d, want %d", available, tc.wantAvailable)
			}
			if quarantined != tc.wantQuarantined {
				t.Errorf("quarantined = %d, want %d", quarantined, tc.wantQuarantined)
			}
			if active != 0 {
				t.Errorf("active = %d, want 0", active)
			}
		})
	}
}

func TestUseReturnsErrPoolExhausted(t *testing.T) {
	pool := NewPool(nil)

	err := pool.Use(func(c *Conn) error {
		t.Fatal("fn should not run against an exhausted pool")
		return nil
	})

	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("err = %v, want %v", err, ErrPoolExhausted)
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The pool is correct when every returned connection ends up in exactly one of
three states — active, available, or quarantined — and never in a
transiently invalid fourth state. The two defers in `Use` produce that
guarantee for free: registering the active-count release *before* the
health-check placement means LIFO order runs placement first, so the count
never drops while the connection is still unfiled. The common mistake this
avoids is writing a single combined defer (or the two defers in the wrong
order) that decrements the count and then decides where the connection
goes — which briefly reports a connection as free before it is anywhere a
subsequent checkout could find it.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — deferred functions execute in LIFO order.
- [database/sql: connection pooling](https://pkg.go.dev/database/sql#DB.SetMaxIdleConns) — the production pool this exercise's shape is modeled on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-audit-event-pointer-capture.md](14-audit-event-pointer-capture.md) | Next: [16-event-ledger-deferred-flush.md](16-event-ledger-deferred-flush.md)
