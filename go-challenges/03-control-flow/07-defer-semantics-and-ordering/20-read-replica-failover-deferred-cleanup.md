# Exercise 20: Read Replica Failover — Deferred Cleanup and Primary Switchover

**Nivel: Intermedio** — validacion rapida (un test corto).

A read path that tries a replica first and falls back to the primary on
failure has two connections in play across the life of one call, and it is
easy to leak the replica's connection while writing the fallback branch —
especially once a second early return is added later and nobody remembers
to release the first connection on that new path too. This module builds a
`Read` function that acquires the replica connection and defers its release
immediately, before any branching happens, so the fallback to primary can
never forget to release it. The module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
dbread/                     independent module: example.com/read-replica-failover-deferred-cleanup
  go.mod                     go 1.24
  dbread.go                  Tracker, Conn, Read(tracker, replicaRead, primaryRead) (string, error)
  cmd/
    demo/
      main.go                runnable demo: replica healthy, then replica down and falling back
  dbread_test.go             table test over replica-healthy/replica-down/both-down cases
```

- Files: `dbread.go`, `cmd/demo/main.go`, `dbread_test.go`.
- Implement: `Tracker` (`AcquireReplica`, `AcquirePrimary`, `Stats`), `Conn` (`Close`), and `Read(tracker *Tracker, replicaRead, primaryRead func(*Conn) (string, error)) (string, error)`.
- Test: a table over a healthy replica, a down replica with a healthy primary, and both down, asserting the value/error and that neither connection kind leaks.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/read-replica-failover-deferred-cleanup/cmd/demo
cd ~/go-exercises/read-replica-failover-deferred-cleanup
go mod init example.com/read-replica-failover-deferred-cleanup
go mod edit -go=1.24
```

### Why the defer goes right after the acquire, not after the branch

`Read` acquires `replicaConn` and defers its `Close` on the very next line,
before the `if val, err := replicaRead(...)` branch even exists in the code.
That ordering is what makes the fallback safe to add: whether `Read` returns
immediately from inside the `if` (replica healthy) or falls through to
acquire a primary connection and return from there instead, the replica's
`defer` fires either way, because both are just different paths to the same
function returning. If the `defer` were written after the `if` block instead
— reachable only on the fallback path — the fast path's early `return`
inside the `if` would skip it entirely, leaking the replica connection on
every successful replica read, which is the overwhelmingly common case in
production. Pairing acquire with defer-release on adjacent lines removes the
question of which branches can reach the cleanup: all of them can, always.

Create `dbread.go`:

```go
package dbread

// Tracker counts how many replica and primary connections have been opened
// and closed, standing in for a real connection pool's metrics.
type Tracker struct {
	replicaOpened int
	replicaClosed int
	primaryOpened int
	primaryClosed int
}

// Stats reports the current open/close counts for both connection kinds.
func (t *Tracker) Stats() (replicaOpened, replicaClosed, primaryOpened, primaryClosed int) {
	return t.replicaOpened, t.replicaClosed, t.primaryOpened, t.primaryClosed
}

// AcquireReplica checks out a connection to the read replica.
func (t *Tracker) AcquireReplica() *Conn {
	t.replicaOpened++
	return &Conn{tracker: t, kind: "replica"}
}

// AcquirePrimary checks out a connection to the primary.
func (t *Tracker) AcquirePrimary() *Conn {
	t.primaryOpened++
	return &Conn{tracker: t, kind: "primary"}
}

// Conn is a pooled connection to either the replica or the primary.
type Conn struct {
	tracker *Tracker
	kind    string
	closed  bool
}

// Close releases the connection back to its pool. Safe to call more than
// once.
func (c *Conn) Close() {
	if c.closed {
		return
	}
	c.closed = true
	switch c.kind {
	case "replica":
		c.tracker.replicaClosed++
	case "primary":
		c.tracker.primaryClosed++
	}
}

// Read tries the read replica first. If replicaRead fails, it falls back
// to the primary. The replica connection's defer is registered immediately
// after it is acquired, so it is released whether replicaRead succeeds
// (the fast path returns right there) or fails and Read goes on to acquire
// and query the primary. Falling back never leaks the replica connection,
// because its cleanup does not depend on which branch Read ultimately
// takes to return.
func Read(tracker *Tracker, replicaRead func(*Conn) (string, error), primaryRead func(*Conn) (string, error)) (string, error) {
	replicaConn := tracker.AcquireReplica()
	defer replicaConn.Close()

	if val, err := replicaRead(replicaConn); err == nil {
		return val, nil
	}

	primaryConn := tracker.AcquirePrimary()
	defer primaryConn.Close()

	return primaryRead(primaryConn)
}
```

### The runnable demo

The demo runs `Read` twice: once with a healthy replica (the primary is
never touched), once with a down replica that falls back to a healthy
primary, printing connection stats after each.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/read-replica-failover-deferred-cleanup"
)

func main() {
	fmt.Println("-- replica healthy --")
	tracker1 := &dbread.Tracker{}
	val, err := dbread.Read(tracker1,
		func(c *dbread.Conn) (string, error) { return "replica-row", nil },
		func(c *dbread.Conn) (string, error) { return "primary-row", nil },
	)
	fmt.Println("value:", val, "error:", err)
	ro, rc, po, pc := tracker1.Stats()
	fmt.Printf("replica opened=%d closed=%d, primary opened=%d closed=%d\n", ro, rc, po, pc)

	fmt.Println("-- replica down, falls back to primary --")
	tracker2 := &dbread.Tracker{}
	val, err = dbread.Read(tracker2,
		func(c *dbread.Conn) (string, error) { return "", errors.New("replica unreachable") },
		func(c *dbread.Conn) (string, error) { return "primary-row", nil },
	)
	fmt.Println("value:", val, "error:", err)
	ro, rc, po, pc = tracker2.Stats()
	fmt.Printf("replica opened=%d closed=%d, primary opened=%d closed=%d\n", ro, rc, po, pc)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
-- replica healthy --
value: replica-row error: <nil>
replica opened=1 closed=1, primary opened=0 closed=0
-- replica down, falls back to primary --
value: primary-row error: <nil>
replica opened=1 closed=1, primary opened=1 closed=1
```

### Tests

`TestReadFailsOverWithoutLeakingConnections` drives `Read` over a healthy
replica, a down replica with a healthy primary, and both down, asserting the
returned value/error and that `Tracker.Stats()` shows the replica connection
always opened exactly once and closed exactly once, with the primary only
touched when the replica actually failed.

Create `dbread_test.go`:

```go
package dbread

import (
	"errors"
	"testing"
)

func TestReadFailsOverWithoutLeakingConnections(t *testing.T) {
	errReplicaDown := errors.New("replica unreachable")
	errPrimaryDown := errors.New("primary unreachable")

	tests := []struct {
		name          string
		replicaErr    error
		primaryErr    error
		wantVal       string
		wantErr       error
		wantPrimaryOK bool
	}{
		{"replica healthy, primary never touched", nil, nil, "replica-row", nil, false},
		{"replica down, primary serves the read", errReplicaDown, nil, "primary-row", nil, true},
		{"replica down, primary also down", errReplicaDown, errPrimaryDown, "", errPrimaryDown, true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tracker := &Tracker{}

			replicaRead := func(c *Conn) (string, error) {
				if tc.replicaErr != nil {
					return "", tc.replicaErr
				}
				return "replica-row", nil
			}
			primaryRead := func(c *Conn) (string, error) {
				if tc.primaryErr != nil {
					return "", tc.primaryErr
				}
				return "primary-row", nil
			}

			val, err := Read(tracker, replicaRead, primaryRead)

			if val != tc.wantVal {
				t.Errorf("val = %q, want %q", val, tc.wantVal)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}

			ro, rc, po, pc := tracker.Stats()
			if ro != 1 || rc != 1 {
				t.Errorf("replica opened=%d closed=%d, want 1 and 1 (no leak)", ro, rc)
			}
			if tc.wantPrimaryOK {
				if po != 1 || pc != 1 {
					t.Errorf("primary opened=%d closed=%d, want 1 and 1 (no leak)", po, pc)
				}
			} else if po != 0 {
				t.Errorf("primary opened=%d, want 0 (replica should have served the read)", po)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Read` is correct when the replica connection is always opened exactly once
and closed exactly once, on every combination of replica and primary
outcomes, and the primary is only ever touched when the replica genuinely
failed. That property holds regardless of how many more fallback tiers get
added later, because each acquired connection's release is pinned to its
own acquisition with an adjacent `defer`, not to a particular return
statement. The mistake this design avoids is deferring the replica's
release only inside the fallback branch, or after the fast-path `return` —
either shape leaks the replica connection on exactly the success case that
happens on almost every request.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — a deferred call fires on every return path out of the function, not just the one visible at the point it was written.
- [database/sql: connection pooling](https://pkg.go.dev/database/sql#DB.SetMaxIdleConns) — the production replica/primary shape this exercise's `Conn` stands in for.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-metrics-aggregator-deferred-flush.md](19-metrics-aggregator-deferred-flush.md) | Next: [21-worker-pool-deferred-cleanup-signal.md](21-worker-pool-deferred-cleanup-signal.md)
