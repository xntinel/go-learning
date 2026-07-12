# Exercise 24: Write-Ahead Log — Deferred Flush with Named Return Error Merge

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A transaction that writes its data and then flushes a write-ahead log has
two independent facts to report when the flush fails: the data write itself
already happened — a caller can read it back right now — and the WAL never
durably confirmed it, which matters enormously if the process crashes before
the next successful flush. A `WriteTx` that only returns the flush error
loses the first fact; one that swallows the flush error to keep returning
`nil` loses the second, far more dangerous, fact. This module builds
`WriteTx` so both facts stay visible, and a WAL whose failed flushes retain
their entries for the next flush to retry, so no write is ever silently
dropped. The module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
wal/                        independent module: example.com/wal-deferred-flush-named-return
  go.mod                     go 1.24
  wal.go                     Store, WAL (Append, Pending, Flush), WriteTx(store, log, key, value, sink) error
  cmd/
    demo/
      main.go                runnable demo: flush fails on tx1, retried flush succeeds on tx2
  wal_test.go                table over write/flush outcomes; retry-across-transactions edge case
```

- Files: `wal.go`, `cmd/demo/main.go`, `wal_test.go`.
- Implement: `Store` (`Put`, `Get`), `WAL` (`Append`, `Pending`, `Flush`), and `WriteTx(store *Store, log *WAL, key, value string, sink func([]string) error) (err error)`.
- Test: a table over successful/failed writes crossed with successful/failed flushes, plus a case proving a failed flush's entries are retried (not dropped) on the next transaction's flush.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/07-defer-semantics-and-ordering/24-wal-deferred-flush-named-return/cmd/demo
cd go-solutions/03-control-flow/07-defer-semantics-and-ordering/24-wal-deferred-flush-named-return
go mod edit -go=1.24
```

### Why the flush has to merge into err, not replace it, and why entries survive a failed flush

`WriteTx`'s deferred closure runs last, after the write itself has either
succeeded (`store.Put` and `log.Append` both ran) or failed validation
(`err` already holds `ErrInvalidKey`). It calls `log.Flush(sink)`
regardless of which happened, because whatever is genuinely pending is
worth trying to flush either way. If that flush also fails, `err =
errors.Join(err, ...)` combines it with whatever `err` already was — an
earlier validation failure and a flush failure are both real, independent
problems, and joining preserves both instead of the second overwriting the
first. Separately, `WAL.Flush` only advances its `flushedUpTo` watermark
*after* `sink` returns successfully: a failed `sink` call leaves every
pending entry exactly where it was, so the very next `Flush` call — from
the next transaction, or a background retry loop — sends them again,
together with whatever new entry has since been appended. Losing that
retry behavior (advancing the watermark unconditionally, or only ever
sending the newest entry) is how a single transient disk-full error turns
into a permanently lost write.

Create `wal.go`:

```go
package wal

import (
	"errors"
	"fmt"
)

// ErrInvalidKey means the transaction was given an empty key.
var ErrInvalidKey = errors.New("wal: invalid key")

// Store is the in-memory data the transaction actually writes to. In a real
// system this would be the durable data files; here it stands in for
// "the write that already happened," independent of whether the WAL
// confirms it durably.
type Store struct {
	data map[string]string
}

// NewStore returns an empty Store.
func NewStore() *Store { return &Store{data: map[string]string{}} }

// Put writes a key/value pair.
func (s *Store) Put(key, value string) { s.data[key] = value }

// Get reads a key/value pair.
func (s *Store) Get(key string) (string, bool) {
	v, ok := s.data[key]
	return v, ok
}

// WAL is a write-ahead log: transactions append entries to it, and Flush
// sends whatever has not yet been durably confirmed to sink. Entries that
// fail to flush stay pending and are retried on the next Flush call --
// they are not lost, and they are not re-sent once a flush of them has
// already succeeded.
type WAL struct {
	entries     []string
	flushedUpTo int
}

// Append records one WAL entry for a transaction that has already written
// its data to the Store.
func (w *WAL) Append(entry string) {
	w.entries = append(w.entries, entry)
}

// Pending returns the entries appended since the last successful flush.
func (w *WAL) Pending() []string {
	return append([]string(nil), w.entries[w.flushedUpTo:]...)
}

// Flush sends every pending entry to sink. If sink succeeds, those entries
// are marked flushed and will not be resent. If sink fails, every pending
// entry -- including ones from earlier transactions that never got
// confirmed -- stays pending for the next Flush call to retry.
func (w *WAL) Flush(sink func([]string) error) error {
	pending := w.Pending()
	if len(pending) == 0 {
		return nil
	}
	if err := sink(pending); err != nil {
		return err
	}
	w.flushedUpTo += len(pending)
	return nil
}

// WriteTx writes key/value to store, appends a WAL entry for it, and
// defers flushing the WAL as the very last thing it does.
//
// The deferred closure reads and writes the function's named return err.
// If the write itself failed validation, err already holds that error, and
// a subsequent flush failure is joined onto it -- the caller learns about
// both. If the write succeeded, wal.Flush's own error becomes err's only
// content: the transaction's data is already sitting in store (the caller
// can Get it right now), but the caller also learns the WAL never durably
// confirmed it, a real durability gap after a crash. Either way, the
// transaction's data write is never rolled back by a flush failure -- WAL
// durability and in-memory commit are reported as two separate facts, both
// visible to the caller through the single returned err plus a direct
// Store.Get.
func WriteTx(store *Store, log *WAL, key, value string, sink func([]string) error) (err error) {
	defer func() {
		if ferr := log.Flush(sink); ferr != nil {
			err = errors.Join(err, fmt.Errorf("wal flush failed: %w", ferr))
		}
	}()

	if key == "" {
		err = ErrInvalidKey
		return
	}

	store.Put(key, value)
	log.Append(fmt.Sprintf("PUT %s=%s", key, value))
	return nil
}
```

### The runnable demo

The demo runs two transactions against one WAL and one sink function that
fails on its first call: the first transaction's flush fails, but the data
is already in the store; the second transaction's flush succeeds and
durably sends both the retried first entry and the new second entry
together.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/wal-deferred-flush-named-return"
)

func main() {
	store := wal.NewStore()
	log := &wal.WAL{}

	failOnce := true
	sink := func(entries []string) error {
		if failOnce {
			failOnce = false
			return errors.New("disk full")
		}
		fmt.Printf("durably flushed %d entries: %v\n", len(entries), entries)
		return nil
	}

	fmt.Println("-- transaction 1: commits, but the WAL flush fails --")
	err := wal.WriteTx(store, log, "order-1", "shipped", sink)
	fmt.Println("error:", err)
	val, ok := store.Get("order-1")
	fmt.Printf("store has order-1: %v value=%q (data committed despite flush failure)\n", ok, val)

	fmt.Println("-- transaction 2: commits, and the retried flush now succeeds --")
	err = wal.WriteTx(store, log, "order-2", "shipped", sink)
	fmt.Println("error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
-- transaction 1: commits, but the WAL flush fails --
error: wal flush failed: disk full
store has order-1: true value="shipped" (data committed despite flush failure)
-- transaction 2: commits, and the retried flush now succeeds --
durably flushed 2 entries: [PUT order-1=shipped PUT order-2=shipped]
error: <nil>
```

### Tests

`TestWriteTxOutcomes` is a table crossing a valid/invalid key with a
succeeding/failing flush, checking both the merged error (via `errors.Is`)
and whether the store actually has the key — the two independent facts a
caller needs. `TestWALRetriesPendingEntriesAcrossTransactions` is the edge
case that matters most for a real WAL: it drives a failing flush on
transaction one, then a succeeding flush on transaction two, and asserts
the second successful `sink` call carries *both* transactions' entries,
proving the first one was retried rather than dropped.

Create `wal_test.go`:

```go
package wal

import (
	"errors"
	"testing"
)

var errDiskFull = errors.New("disk full")

func TestWriteTxOutcomes(t *testing.T) {
	tests := []struct {
		name           string
		key            string
		sinkErr        error
		wantErr        error
		wantWrapsDisk  bool
		wantStoreHasIt bool
	}{
		{
			name:           "successful write, successful flush",
			key:            "order-1",
			sinkErr:        nil,
			wantErr:        nil,
			wantStoreHasIt: true,
		},
		{
			name:           "successful write, flush fails: caller sees both facts",
			key:            "order-2",
			sinkErr:        errDiskFull,
			wantErr:        errDiskFull,
			wantWrapsDisk:  true,
			wantStoreHasIt: true,
		},
		{
			name:           "invalid key: no write, flush of nothing pending is a no-op",
			key:            "",
			sinkErr:        nil,
			wantErr:        ErrInvalidKey,
			wantStoreHasIt: false,
		},
		{
			name:           "invalid key AND flush fails: both errors are visible",
			key:            "",
			sinkErr:        errDiskFull,
			wantErr:        ErrInvalidKey,
			wantWrapsDisk:  false, // nothing was appended, so Flush has nothing to send and returns nil
			wantStoreHasIt: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore()
			log := &WAL{}
			sink := func(entries []string) error { return tc.sinkErr }

			err := WriteTx(store, log, tc.key, "shipped", sink)

			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want it to wrap %v", err, tc.wantErr)
			}
			if tc.wantWrapsDisk && !errors.Is(err, errDiskFull) {
				t.Errorf("err = %v, want it to also wrap %v", err, errDiskFull)
			}
			if tc.wantErr == nil && !tc.wantWrapsDisk && err != nil {
				t.Errorf("err = %v, want nil", err)
			}

			_, ok := store.Get(tc.key)
			if ok != tc.wantStoreHasIt {
				t.Errorf("store has key = %v, want %v", ok, tc.wantStoreHasIt)
			}
		})
	}
}

// TestWALRetriesPendingEntriesAcrossTransactions is the edge case that
// matters most for a real WAL: a flush failure must not drop the entry --
// it has to stay pending and go out with the next successful flush,
// alongside whatever new entry that next transaction appended.
func TestWALRetriesPendingEntriesAcrossTransactions(t *testing.T) {
	store := NewStore()
	log := &WAL{}

	var sunk [][]string
	callCount := 0
	sink := func(entries []string) error {
		callCount++
		if callCount == 1 {
			return errDiskFull
		}
		sunk = append(sunk, entries)
		return nil
	}

	err1 := WriteTx(store, log, "order-1", "shipped", sink)
	if !errors.Is(err1, errDiskFull) {
		t.Fatalf("tx1 err = %v, want it to wrap %v", err1, errDiskFull)
	}
	if val, ok := store.Get("order-1"); !ok || val != "shipped" {
		t.Fatalf("store missing order-1 after tx1 despite the write itself succeeding")
	}

	err2 := WriteTx(store, log, "order-2", "shipped", sink)
	if err2 != nil {
		t.Fatalf("tx2 err = %v, want nil", err2)
	}

	if len(sunk) != 1 {
		t.Fatalf("sink was durably called %d times, want 1", len(sunk))
	}
	if len(sunk[0]) != 2 {
		t.Fatalf("second successful flush carried %d entries, want 2 (the retried tx1 entry plus tx2's)", len(sunk[0]))
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

`WriteTx` is correct when a flush failure never causes either kind of data
loss: the in-memory write is never rolled back just because durability
confirmation failed, and the WAL entry describing that write is never
dropped just because its first flush attempt failed. The named-return
`err` merge handles the first property — the caller always learns about a
flush failure even when the write itself succeeded — and the
flush-only-advances-the-watermark-on-success design handles the second. The
mistake this pair of decisions avoids is treating "the write succeeded" and
"the write is durable" as the same fact: they are not, and a `WriteTx` that
collapses them into a single boolean either hides a real durability gap
from the caller or discards a perfectly good write over a transient disk
error.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — combining independent errors so `errors.Is` still finds each one.
- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — deferred functions can read and modify named result parameters.
- [PostgreSQL: Write-Ahead Logging (WAL)](https://www.postgresql.org/docs/current/wal-intro.html) — the durability contract this exercise's `WAL` type is a minimal model of.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-cascading-timeout-nested-defers.md](23-cascading-timeout-nested-defers.md) | Next: [25-request-coalescing-singleflight-deferred.md](25-request-coalescing-singleflight-deferred.md)
