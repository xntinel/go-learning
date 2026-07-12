# Exercise 25: Database Snapshot Restore on Transaction Abort

Rolling back a small in-memory transaction one write at a time (an undo log)
is more machinery than the job needs when the whole working set is cheap to
copy up front. This exercise builds a `Transact` helper that snapshots the
store before running the transaction and, via a deferred closure keyed on a
named `committed bool`, swaps the whole map back in if the transaction
aborts — a single restore step instead of tracking every individual write.

**Nivel: Intermedio** — validacion rapida (dos pruebas cortas).

## What you'll build

```text
snaptx/                    independent module: example.com/snaptx
  go.mod
  snaptx.go                 DB; Transact (named committed, deferred snapshot restore)
  cmd/demo/
    main.go                 runnable demo: a committed transaction and an aborted one
  snaptx_test.go             commits on success, restores exact snapshot on abort
```

- Files: `snaptx.go`, `cmd/demo/main.go`, `snaptx_test.go`.
- Implement: `(*DB) Transact(fn func(data map[string]string) error) (committed bool, err error)` that snapshots `db.data` before calling `fn` and restores it via a deferred closure unless `fn` succeeded.
- Test: a successful transaction's writes persist; a failing transaction's writes (including a brand-new key) are fully rolled back to the pre-transaction snapshot.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/25-transaction-snapshot-restore-on-abort/cmd/demo
cd go-solutions/04-functions/02-named-return-values/25-transaction-snapshot-restore-on-abort
go mod edit -go=1.24
```

### One snapshot, one restore, keyed on the named result

```go
snap := db.snapshot()
defer func() {
    if !committed {
        db.data = snap
    }
}()

if err = fn(db.data); err != nil {
    return
}
committed = true
return
```

`fn` is handed `db.data` directly and mutates it in place — no method calls
required for each write, since Go maps are reference types. If `fn` returns
an error, `committed` is still false when the deferred closure runs, so
`db.data` is replaced wholesale with the snapshot taken before `fn` ever ran:
every mutation `fn` made, including a key it created that never existed
before, disappears in one assignment. This is the cheap-rollback trade-off
this exercise is about: for a small working set, copying the whole map once
up front and swapping it back is simpler and just as correct as recording
every individual write to undo.

Create `snaptx.go`:

```go
package snaptx

import "sync"

// DB is a toy in-memory key-value store used to demonstrate a snapshot-based
// rollback: cheaper than an undo log when the working set is small.
type DB struct {
	mu   sync.Mutex
	data map[string]string
}

// NewDB returns an empty store.
func NewDB() *DB {
	return &DB{data: make(map[string]string)}
}

// Get reads a key.
func (db *DB) Get(key string) (string, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	v, ok := db.data[key]
	return v, ok
}

// Snapshot returns an independent copy of the current data.
func (db *DB) snapshot() map[string]string {
	snap := make(map[string]string, len(db.data))
	for k, v := range db.data {
		snap[k] = v
	}
	return snap
}

// Transact runs fn with exclusive access to db's data. If fn returns an
// error, every mutation fn made is rolled back by restoring the snapshot
// taken before fn ran.
//
// committed is a named result: a deferred closure checks it after fn
// returns (or panics) and, whenever it is still false, restores db.data to
// the pre-transaction snapshot. Snapshotting up front and swapping the whole
// map back on abort is a cheap rollback for a small working set — no
// per-write undo log is needed, only ever one restore step keyed on the
// named result.
func (db *DB) Transact(fn func(data map[string]string) error) (committed bool, err error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	snap := db.snapshot()
	defer func() {
		if !committed {
			db.data = snap
		}
	}()

	if err = fn(db.data); err != nil {
		return
	}
	committed = true
	return
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/snaptx"
)

func main() {
	db := snaptx.NewDB()

	committed, err := db.Transact(func(data map[string]string) error {
		data["balance"] = "100"
		return nil
	})
	v, _ := db.Get("balance")
	fmt.Printf("committed transaction: committed=%v err=%v balance=%s\n", committed, err, v)

	committed, err = db.Transact(func(data map[string]string) error {
		data["balance"] = "0"
		return errors.New("insufficient funds")
	})
	v, _ = db.Get("balance")
	fmt.Printf("aborted transaction: committed=%v err=%v balance=%s\n", committed, err, v)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
committed transaction: committed=true err=<nil> balance=100
aborted transaction: committed=false err=insufficient funds balance=100
```

### Tests

Create `snaptx_test.go`:

```go
package snaptx

import (
	"errors"
	"testing"
)

func TestTransactCommitsOnSuccess(t *testing.T) {
	t.Parallel()

	db := NewDB()
	committed, err := db.Transact(func(data map[string]string) error {
		data["a"] = "1"
		return nil
	})
	if err != nil {
		t.Fatalf("Transact: unexpected error: %v", err)
	}
	if !committed {
		t.Fatal("committed = false, want true on success")
	}
	if v, ok := db.Get("a"); !ok || v != "1" {
		t.Fatalf("Get(a) = (%q, %v), want (1, true)", v, ok)
	}
}

func TestTransactRestoresSnapshotOnAbort(t *testing.T) {
	t.Parallel()

	db := NewDB()
	_, _ = db.Transact(func(data map[string]string) error {
		data["a"] = "original"
		return nil
	})

	wantErr := errors.New("boom")
	committed, err := db.Transact(func(data map[string]string) error {
		data["a"] = "corrupted"
		data["b"] = "new-but-should-vanish"
		return wantErr
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if committed {
		t.Fatal("committed = true, want false on abort")
	}
	if v, ok := db.Get("a"); !ok || v != "original" {
		t.Fatalf("Get(a) after abort = (%q, %v), want (original, true)", v, ok)
	}
	if _, ok := db.Get("b"); ok {
		t.Fatal("Get(b) after abort: key should not exist, snapshot restore should have removed it")
	}
}
```

## Review

`Transact` is correct when a successful transaction's writes persist exactly,
and a failing transaction leaves the store byte-for-byte identical to how it
looked before `fn` ran — no partial writes, no orphaned new keys. The named
result `committed` is what a single deferred closure checks to decide between
"leave `db.data` as `fn` left it" and "swap the whole map back to the
snapshot," so the rollback logic exists in exactly one place regardless of
how `fn` fails. The mistake to avoid is mutating a copy of the map inside
`fn` and only swapping it in on success (the inverse strategy) — that also
works, but it means every write must be buffered rather than applied
directly, which is more code for the same guarantee this snapshot-on-entry
approach gets for free.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Go Spec: Map types](https://go.dev/ref/spec#Map_types)
- [Effective Go: Named result parameters](https://go.dev/doc/effective_go#named-results)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-rate-limiter-token-return-on-abort.md](24-rate-limiter-token-return-on-abort.md) | Next: [26-graceful-shutdown-worker-drain-with-timeout.md](26-graceful-shutdown-worker-drain-with-timeout.md)
