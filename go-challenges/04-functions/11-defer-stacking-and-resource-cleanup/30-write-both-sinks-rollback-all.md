# Exercise 30: Dual-Sink Write Atomicity — Rollback All If Primary Fails

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Writing the same data to a primary store and a replica only counts as one
atomic operation if a failure anywhere in the sequence — including a
post-write consistency check — discards *both* writes, not just the one
that happened to fail. This module writes to primary, then replica, in
that order, and stacks one rollback defer immediately after each successful
write, so any later failure unwinds both in the reverse of the order they
were acquired.

## What you'll build

```text
dualwrite/                   independent module: example.com/dualwrite
  go.mod
  dualwrite/dualwrite.go       Sink interface; MemSink; WriteAll (write, write, verify, rollback)
  cmd/demo/main.go              both writes succeed, verify fails, both roll back
  dualwrite/dualwrite_test.go   full success; replica write fails; verify fails; rollback error
```

- Files: `dualwrite/dualwrite.go`, `cmd/demo/main.go`, `dualwrite/dualwrite_test.go`.
- Implement: a `Sink` interface with `Write(data string) error` and `Rollback() error`; a `MemSink` test double that traces its own writes and rollbacks; and `WriteAll(primary, replica Sink, data string, verify func() error) (err error)`, which writes to `primary`, defers its rollback, writes to `replica`, defers its rollback, then runs `verify` — any failure triggers whichever rollbacks are armed, in reverse acquisition order.
- Test: both writes and verify succeed and nothing rolls back; the replica write fails and only the primary rolls back (the replica never wrote anything to undo); `verify` fails after both writes succeed and both roll back, replica first; a rollback that itself errors is surfaced via `errors.Join` while the other rollback still runs.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Acquisition order in, reverse order out

`WriteAll` acquires two resources in a fixed order — primary first, then
replica — and defers each one's rollback the instant it succeeds, right
next to the code that acquired it. That placement matters: the `defer` for
`primary`'s rollback is registered before `replica.Write` is even
attempted, so if `replica.Write` fails, `primary`'s rollback is already
armed and ready without `WriteAll` needing a separate branch for "which
resources actually got acquired before things went wrong." Because Go runs
defers LIFO, the two rollbacks — if both end up armed — fire in the reverse
of their acquisition order: replica (acquired second) rolls back first,
then primary. That reverse ordering is not cosmetic; it is what "undo in
the opposite order you did things" means for two resources whose
consistency depends on each other, the same discipline this chapter applies
to locks, transactions, and multi-step setups elsewhere.

The `verify` step is what makes this genuinely "atomic" rather than just
"handle write failures": even when *both* writes succeed, a downstream
consistency check can still fail, and by that point both rollbacks are
armed, so both are undone — the two sinks never end up holding data that
passed neither sink's own write path nor the check meant to confirm they
agree.

Create `dualwrite/dualwrite.go`:

```go
package dualwrite

import (
	"errors"
	"fmt"
)

// Sink is a write target that can also undo its most recent write.
type Sink interface {
	Write(data string) error
	Rollback() error
}

// WriteAll writes data to primary, then replica, in that acquisition order,
// then runs verify (a post-write consistency check). If any step fails, the
// writes already made are rolled back in the reverse of the order they were
// acquired in -- replica before primary -- via defers stacked immediately
// after each successful write, so a failure at any point discards both
// writes atomically instead of leaving the sinks disagreeing.
func WriteAll(primary, replica Sink, data string, verify func() error) (err error) {
	if err = primary.Write(data); err != nil {
		return fmt.Errorf("primary write: %w", err)
	}
	defer func() {
		if err != nil {
			if rbErr := primary.Rollback(); rbErr != nil {
				err = errors.Join(err, fmt.Errorf("primary rollback: %w", rbErr))
			}
		}
	}()

	if err = replica.Write(data); err != nil {
		return fmt.Errorf("replica write: %w", err)
	}
	defer func() {
		if err != nil {
			if rbErr := replica.Rollback(); rbErr != nil {
				err = errors.Join(err, fmt.Errorf("replica rollback: %w", rbErr))
			}
		}
	}()

	if verify != nil {
		if vErr := verify(); vErr != nil {
			err = fmt.Errorf("verify: %w", vErr)
			return err
		}
	}
	return nil
}

// MemSink is an in-memory Sink that records its writes and rollbacks into a
// shared trace, for demos and tests. WriteErr and RollbackErr, if set, make
// the corresponding operation fail.
type MemSink struct {
	Name        string
	Trace       *[]string
	WriteErr    error
	RollbackErr error

	written bool
}

func (m *MemSink) Write(data string) error {
	if m.WriteErr != nil {
		*m.Trace = append(*m.Trace, "write-"+m.Name+"-FAIL")
		return m.WriteErr
	}
	m.written = true
	*m.Trace = append(*m.Trace, "write-"+m.Name)
	return nil
}

func (m *MemSink) Rollback() error {
	if !m.written {
		return nil
	}
	if m.RollbackErr != nil {
		*m.Trace = append(*m.Trace, "rollback-"+m.Name+"-FAIL")
		return m.RollbackErr
	}
	m.written = false
	*m.Trace = append(*m.Trace, "rollback-"+m.Name)
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/dualwrite/dualwrite"
)

func main() {
	var trace []string
	primary := &dualwrite.MemSink{Name: "primary", Trace: &trace}
	replica := &dualwrite.MemSink{Name: "replica", Trace: &trace}

	err := dualwrite.WriteAll(primary, replica, "order-42", func() error {
		return fmt.Errorf("checksum mismatch between primary and replica")
	})

	fmt.Println("err:", err)
	for _, e := range trace {
		fmt.Println(e)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
err: verify: checksum mismatch between primary and replica
write-primary
write-replica
rollback-replica
rollback-primary
```

### Tests

Create `dualwrite/dualwrite_test.go`:

```go
package dualwrite

import (
	"errors"
	"slices"
	"testing"
)

func TestWriteAllSuccessNoRollback(t *testing.T) {
	t.Parallel()

	var trace []string
	primary := &MemSink{Name: "primary", Trace: &trace}
	replica := &MemSink{Name: "replica", Trace: &trace}

	err := WriteAll(primary, replica, "data", func() error { return nil })
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	want := []string{"write-primary", "write-replica"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

func TestWriteAllReplicaFailsRollsBackOnlyPrimary(t *testing.T) {
	t.Parallel()

	var trace []string
	writeErr := errors.New("replica unreachable")
	primary := &MemSink{Name: "primary", Trace: &trace}
	replica := &MemSink{Name: "replica", Trace: &trace, WriteErr: writeErr}

	err := WriteAll(primary, replica, "data", func() error { return nil })
	if !errors.Is(err, writeErr) {
		t.Fatalf("err = %v, want errors.Is %v", err, writeErr)
	}
	want := []string{"write-primary", "write-replica-FAIL", "rollback-primary"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

func TestWriteAllVerifyFailsRollsBackBothInReverse(t *testing.T) {
	t.Parallel()

	var trace []string
	verifyErr := errors.New("checksum mismatch")
	primary := &MemSink{Name: "primary", Trace: &trace}
	replica := &MemSink{Name: "replica", Trace: &trace}

	err := WriteAll(primary, replica, "data", func() error { return verifyErr })
	if !errors.Is(err, verifyErr) {
		t.Fatalf("err = %v, want errors.Is %v", err, verifyErr)
	}
	want := []string{"write-primary", "write-replica", "rollback-replica", "rollback-primary"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

func TestWriteAllSurfacesRollbackError(t *testing.T) {
	t.Parallel()

	var trace []string
	verifyErr := errors.New("checksum mismatch")
	rollbackErr := errors.New("replica rollback unreachable")
	primary := &MemSink{Name: "primary", Trace: &trace}
	replica := &MemSink{Name: "replica", Trace: &trace, RollbackErr: rollbackErr}

	err := WriteAll(primary, replica, "data", func() error { return verifyErr })

	if !errors.Is(err, verifyErr) {
		t.Errorf("err = %v, want errors.Is %v (verify)", err, verifyErr)
	}
	if !errors.Is(err, rollbackErr) {
		t.Errorf("err = %v, want errors.Is %v (rollback)", err, rollbackErr)
	}
	// The primary rollback still ran despite the replica rollback failing.
	want := []string{"write-primary", "write-replica", "rollback-replica-FAIL", "rollback-primary"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}
```

## Review

The write is atomic when every failure path — a failed replica write, or a
failed post-write verification — leaves neither sink holding the data, and
when a rollback that itself errors is surfaced without preventing the other
rollback from running. The mistake this pattern exists to prevent is
rolling back only the sink that reported the failure (e.g. only calling
`replica.Rollback()` when `verify` fails downstream of both writes, on the
assumption that only the thing that "caused" the error needs undoing) —
which leaves the primary silently holding data the replica no longer has,
exactly the inconsistency the whole exercise exists to prevent. Deferring
each rollback immediately after its matching write, rather than deciding at
the failure site what needs undoing, means every armed rollback fires
regardless of which later step actually failed.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join)
- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Two-phase commit protocol](https://en.wikipedia.org/wiki/Two-phase_commit_protocol) — the distributed-systems pattern this exercise is a miniature of.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-dns-cache-entry-ttl-cleanup.md](29-dns-cache-entry-ttl-cleanup.md) | Next: [31-context-value-isolation-restore.md](31-context-value-isolation-restore.md)
