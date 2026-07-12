# Exercise 3: A Batch Reader That Stops Cleanly on Cancel or Missing Row

Fetching many rows one key at a time is a real pattern — hydrating a list of ids,
a fan-out warmup, a backfill. The trap is that once the context fires mid-loop,
every remaining call returns a driver-internal cancellation error with no
context. This exercise builds the loop that pre-checks `ctx.Err()` and returns a
single progress-bearing error plus the partial results collected so far.

## What you'll build

```text
batchread/                   independent module: example.com/batchread
  go.mod                     go 1.25
  batchread.go               DB with Get/Set; FindMultiple and BatchInsert loops
  cmd/
    demo/
      main.go                a batch read that completes and one that aborts on timeout
  batchread_test.go          stop-on-cancel, stop-on-not-found, batch-insert abort, -race
```

Files: `batchread.go`, `cmd/demo/main.go`, `batchread_test.go`.
Implement: `FindMultiple(ctx, ids)` that pre-checks `ctx.Err()` before each read and returns partial results plus a progress-bearing error on cancellation or a wrapped `ErrNotFound` on the first missing key; and `BatchInsert(ctx, kvs)` with the same pre-check on writes.
Test: a budget that only allows a few of five reads returns `DeadlineExceeded` with fewer than five results; the first missing key returns `ErrNotFound` with exactly the preceding successes; a 20-write batch under a tight budget inserts only a handful.
Verify: `go test -count=1 -race ./...`

### Why pre-check `ctx.Err()` at the top of each iteration

Consider a loop that reads five keys, each taking 15ms, under a 50ms budget.
Without a pre-check, the first three reads succeed, the fourth read starts, and
the context fires while it is waiting — so the fourth call returns a
driver-internal cancellation, and if you did not stop, the fifth would too. You
end up with two nearly identical low-information errors and no clear statement of
how far you got. Pre-checking `ctx.Err()` at the top of each iteration turns that
into one deliberate error:

```
for _, id := range ids {
    if err := ctx.Err(); err != nil {
        return out, fmt.Errorf("find multiple aborted after %d/%d: %w", len(out), len(ids), err)
    }
    v, err := d.Get(ctx, id)   // still context-aware; the pre-check is belt-and-suspenders
    ...
}
```

The error message carries progress ("aborted after 3/5"), and the function
returns the partial `out` map so the caller can use what was fetched or log the
gap. The pre-check does not *replace* the context-aware `Get` — the read itself
must still respect cancellation — it makes the *loop* short-circuit cleanly at a
known boundary instead of paying for one more doomed round trip. The same shape
applies to a write batch: `BatchInsert` pre-checks before each `Set`, so a
cancelled bulk load stops at a countable point and reports how many rows landed.

The not-found path is the other early exit. On the first missing key,
`FindMultiple` returns the results gathered before it plus a wrapped
`ErrNotFound`, so the caller both learns which contract failed (via `errors.Is`)
and keeps the partial data.

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/10-context-aware-database-queries/03-batch-read-loop-cancellation/cmd/demo
cd go-solutions/14-select-and-context/10-context-aware-database-queries/03-batch-read-loop-cancellation
go mod edit -go=1.25
```

Create `batchread.go`:

```go
package batchread

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNotFound is the sentinel for a missing key.
var ErrNotFound = errors.New("batchread: key not found")

// DB is a concurrency-safe in-memory store whose operations race a simulated
// latency against ctx.Done, modeling context-aware driver methods.
type DB struct {
	mu      sync.RWMutex
	data    map[string]string
	latency time.Duration
}

func New(latency time.Duration, kvs map[string]string) *DB {
	cp := make(map[string]string, len(kvs))
	for k, v := range kvs {
		cp[k] = v
	}
	return &DB{data: cp, latency: latency}
}

func (d *DB) Get(ctx context.Context, key string) (string, error) {
	select {
	case <-time.After(d.latency):
	case <-ctx.Done():
		return "", fmt.Errorf("batchread.Get(%s): %w", key, ctx.Err())
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	v, ok := d.data[key]
	if !ok {
		return "", fmt.Errorf("batchread.Get(%s): %w", key, ErrNotFound)
	}
	return v, nil
}

func (d *DB) Set(ctx context.Context, key, value string) error {
	select {
	case <-time.After(d.latency):
	case <-ctx.Done():
		return fmt.Errorf("batchread.Set(%s): %w", key, ctx.Err())
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data[key] = value
	return nil
}

// Len reports how many keys are stored.
func (d *DB) Len() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.data)
}

// FindMultiple reads ids one by one, pre-checking ctx before each read. It
// returns the values gathered so far plus a progress-bearing error on
// cancellation, or a wrapped ErrNotFound on the first missing key.
func (d *DB) FindMultiple(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("find multiple aborted after %d/%d: %w", len(out), len(ids), err)
		}
		v, err := d.Get(ctx, id)
		if err != nil {
			return out, fmt.Errorf("find %s: %w", id, err)
		}
		out[id] = v
	}
	return out, nil
}

// BatchInsert writes kvs one by one, pre-checking ctx before each write. It
// returns the number of rows written and a progress-bearing error if cancelled.
func (d *DB) BatchInsert(ctx context.Context, kvs []struct{ Key, Value string }) (int, error) {
	written := 0
	for _, kv := range kvs {
		if err := ctx.Err(); err != nil {
			return written, fmt.Errorf("batch insert aborted after %d/%d: %w", written, len(kvs), err)
		}
		if err := d.Set(ctx, kv.Key, kv.Value); err != nil {
			return written, fmt.Errorf("batch insert %s: %w", kv.Key, err)
		}
		written++
	}
	return written, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/batchread"
)

func main() {
	db := batchread.New(10*time.Millisecond, map[string]string{
		"u1": "a", "u2": "b", "u3": "c", "u4": "d", "u5": "e",
	})

	full, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := db.FindMultiple(full, []string{"u1", "u2", "u3"})
	fmt.Printf("complete: n=%d err=%v\n", len(got), err)

	tight, cancelTight := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelTight()
	got, err = db.FindMultiple(tight, []string{"u1", "u2", "u3", "u4", "u5"})
	fmt.Printf("aborted: n=%d deadline=%v\n", len(got), errors.Is(err, context.DeadlineExceeded))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
complete: n=3 err=<nil>
aborted: n=2 deadline=true
```

### Tests

`TestBatchInsertAbortsOnCancel` is the exercise's "your turn" prompt turned into a
real assertion: 20 writes at 30ms each under a 100ms budget must stop after only
a handful, proving the loop noticed the deadline instead of grinding through all
20.

Create `batchread_test.go`:

```go
package batchread

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestFindMultipleStopsOnCancel(t *testing.T) {
	t.Parallel()
	db := New(15*time.Millisecond, map[string]string{"u1": "a", "u2": "b", "u3": "c", "u4": "d", "u5": "e"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	got, err := db.FindMultiple(ctx, []string{"u1", "u2", "u3", "u4", "u5"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FindMultiple: err = %v, want DeadlineExceeded", err)
	}
	if len(got) >= 5 {
		t.Fatalf("FindMultiple returned %d values, want < 5 (loop did not stop early)", len(got))
	}
}

func TestFindMultipleStopsOnNotFound(t *testing.T) {
	t.Parallel()
	db := New(time.Millisecond, map[string]string{"u1": "a"})

	got, err := db.FindMultiple(context.Background(), []string{"u1", "missing", "u3"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindMultiple: err = %v, want ErrNotFound", err)
	}
	if len(got) != 1 {
		t.Fatalf("FindMultiple returned %d values, want exactly 1 (the preceding success)", len(got))
	}
}

func TestFindMultipleAllPresent(t *testing.T) {
	t.Parallel()
	db := New(time.Millisecond, map[string]string{"u1": "a", "u2": "b"})
	got, err := db.FindMultiple(context.Background(), []string{"u1", "u2"})
	if err != nil {
		t.Fatalf("FindMultiple: err = %v, want nil", err)
	}
	if len(got) != 2 || got["u1"] != "a" || got["u2"] != "b" {
		t.Fatalf("FindMultiple = %v, want {u1:a u2:b}", got)
	}
}

func TestBatchInsertAbortsOnCancel(t *testing.T) {
	t.Parallel()
	db := New(30*time.Millisecond, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	recs := make([]struct{ Key, Value string }, 20)
	for i := range recs {
		recs[i] = struct{ Key, Value string }{Key: fmt.Sprintf("k%d", i), Value: "v"}
	}

	n, err := db.BatchInsert(ctx, recs)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("BatchInsert: err = %v, want DeadlineExceeded", err)
	}
	if n < 1 || n > 6 {
		t.Fatalf("BatchInsert wrote %d rows, want a small handful (1-6) before the deadline", n)
	}
	if db.Len() != n {
		t.Fatalf("store has %d rows but BatchInsert reported %d written", db.Len(), n)
	}
}

func ExampleDB_FindMultiple() {
	db := New(time.Millisecond, map[string]string{"u1": "alice", "u2": "bob"})
	got, _ := db.FindMultiple(context.Background(), []string{"u1", "u2"})
	fmt.Println(got["u1"], got["u2"])
	// Output: alice bob
}
```

## Review

The loop is correct when a cancelled batch stops at a countable boundary and says
where it stopped. `FindMultiple` returns the partial `out` and an error whose
message carries "aborted after k/n"; `BatchInsert` returns the count actually
written and — the cross-check the test makes — that count matches `db.Len()`, so
"reported written" never diverges from "actually written". The most common bug
this guards against is omitting the per-iteration `ctx.Err()` pre-check: the
function would still terminate (each `Get`/`Set` is context-aware) but it would
pay for one extra doomed round trip and return a lower-information error than a
clean progress boundary. The not-found path is equally important: it returns
exactly the successes gathered before the missing key, so partial data is never
thrown away. Run `-race` for the concurrent access on the shared map.

## Resources

- [database/sql: Rows.Err](https://pkg.go.dev/database/sql#Rows.Err) — where a real row-iteration cancellation surfaces.
- [context: Context.Err](https://pkg.go.dev/context#Context.Err) — the value the loop pre-checks each iteration.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) — cancellation across a sequence of operations.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-sql-repository-not-found-mapping.md](04-sql-repository-not-found-mapping.md)
