# Exercise 7: Optimistic Concurrency: Version-Checked Repository Update

When two requests read the same row and both write it back, the second silently
overwrites the first — a lost update. Optimistic concurrency control prevents it with
a version column: a writer states the version it read, and the update only applies if
that version still matches. This module builds that write path as a repository
`Update`, three guard clauses reading top-to-bottom: exists, version matches, apply.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
optimistic/                 independent module: example.com/optimistic
  go.mod                    go 1.26
  repo.go                   Repo, Record, Insert, Get, Update(id, expectedVersion, mutate)
  cmd/
    demo/
      main.go               a successful update, then a stale-version conflict
  repo_test.go              not-found, conflict, success, concurrent single-winner
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: `Update(ctx, id, expectedVersion, mutate func(*Record)) error` using `if cur, ok := rows[id]; !ok { return ErrNotFound }` for existence and a version-match guard before applying `mutate` and bumping the version.
- Test: unknown id returns `ErrNotFound`; stale version returns `ErrVersionConflict` with the row unchanged; matching version succeeds, increments the version, applies the mutation; two updates with the same expected version yield exactly one winner.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/optimistic/cmd/demo
cd ~/go-exercises/optimistic
go mod init example.com/optimistic
```

## Exists, version-matches, apply — under one lock

`Update` is three guard clauses in sequence, and the order is the logic:

1. Existence: `if cur, ok := rows[id]; !ok { return ErrNotFound }`. The comma-ok read
   distinguishes a missing row from a zero-valued one; a caller updating an id that
   was deleted must get `ErrNotFound`, not a silent create.
2. Version match: `if cur.Version != expectedVersion { return ErrVersionConflict }`.
   The caller read version N and computed a new value from it; if the stored version
   is no longer N, someone else wrote in between, the caller's value is based on stale
   data, and applying it would lose the intervening update.
3. Apply: run `mutate` on a copy, bump `Version`, store it back.

All three happen inside one mutex. That is the whole point — if the check and the
store were in separate critical sections, two writers could both pass the version
check against version N and both write version N+1, which is exactly the lost update
optimistic locking exists to prevent. Holding the lock across check-and-apply makes
the read-modify-write atomic, so of two concurrent writers with the same expected
version, exactly one succeeds and the other sees `ErrVersionConflict`. The test
proves this with two goroutines racing the same expected version.

The context is checked at the top (`if err := ctx.Err(); err != nil { return err }`)
so a canceled request does not perform a write — the same discipline a real database
driver applies.

Create `repo.go`:

```go
package optimistic

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrNotFound        = errors.New("record not found")
	ErrVersionConflict = errors.New("version conflict")
)

// Record is a row with an optimistic-lock version.
type Record struct {
	ID      string
	Name    string
	Version int
}

// Repo is an in-memory version-locked store.
type Repo struct {
	mu   sync.Mutex
	rows map[string]Record
}

func NewRepo() *Repo { return &Repo{rows: make(map[string]Record)} }

// Insert stores a new record at version 1.
func (r *Repo) Insert(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec.Version = 1
	r.rows[rec.ID] = rec
}

// Get returns a copy of the record and whether it exists.
func (r *Repo) Get(id string) (Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.rows[id]
	return rec, ok
}

// Update applies mutate to the record at id only if its version equals
// expectedVersion, then bumps the version. It returns ErrNotFound or
// ErrVersionConflict without modifying anything on failure.
func (r *Repo) Update(ctx context.Context, id string, expectedVersion int, mutate func(*Record)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	cur, ok := r.rows[id]
	if !ok {
		return ErrNotFound
	}
	if cur.Version != expectedVersion {
		return ErrVersionConflict
	}

	mutate(&cur)
	cur.Version = expectedVersion + 1
	r.rows[id] = cur
	return nil
}
```

### The runnable demo

The demo inserts a record, updates it successfully (version 1 to 2), then attempts a
second update still claiming version 1 — a stale write that must be rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/optimistic"
)

func main() {
	repo := optimistic.NewRepo()
	repo.Insert(optimistic.Record{ID: "acct-1", Name: "alice"})

	ctx := context.Background()
	if err := repo.Update(ctx, "acct-1", 1, func(r *optimistic.Record) { r.Name = "alice-2" }); err != nil {
		fmt.Println("update 1 failed:", err)
		return
	}
	rec, _ := repo.Get("acct-1")
	fmt.Printf("after update: name=%s version=%d\n", rec.Name, rec.Version)

	// A writer that still thinks the version is 1 is stale.
	err := repo.Update(ctx, "acct-1", 1, func(r *optimistic.Record) { r.Name = "clobber" })
	if errors.Is(err, optimistic.ErrVersionConflict) {
		fmt.Println("stale write rejected:", err)
	}
	rec, _ = repo.Get("acct-1")
	fmt.Printf("unchanged: name=%s version=%d\n", rec.Name, rec.Version)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after update: name=alice-2 version=2
stale write rejected: version conflict
unchanged: name=alice-2 version=2
```

### Tests

The tests cover the three branches plus the concurrency guarantee. Not-found and
conflict assert via `errors.Is` and check that a rejected update leaves the row
untouched. The success case checks the mutation applied and the version incremented.
The concurrency test launches two updates against the same expected version and
asserts exactly one succeeds and one conflicts.

Create `repo_test.go`:

```go
package optimistic

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func seeded() *Repo {
	r := NewRepo()
	r.Insert(Record{ID: "x", Name: "orig"})
	return r
}

func TestUpdateNotFound(t *testing.T) {
	t.Parallel()
	r := seeded()
	err := r.Update(t.Context(), "missing", 1, func(*Record) {})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateVersionConflictLeavesRowUnchanged(t *testing.T) {
	t.Parallel()
	r := seeded()
	err := r.Update(t.Context(), "x", 99, func(rec *Record) { rec.Name = "clobber" })
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("err = %v, want ErrVersionConflict", err)
	}
	rec, _ := r.Get("x")
	if rec.Name != "orig" || rec.Version != 1 {
		t.Fatalf("row changed to %+v; want name=orig version=1", rec)
	}
}

func TestUpdateSuccessBumpsVersion(t *testing.T) {
	t.Parallel()
	r := seeded()
	if err := r.Update(t.Context(), "x", 1, func(rec *Record) { rec.Name = "new" }); err != nil {
		t.Fatalf("Update() = %v, want nil", err)
	}
	rec, _ := r.Get("x")
	if rec.Name != "new" || rec.Version != 2 {
		t.Fatalf("row = %+v; want name=new version=2", rec)
	}
}

func TestConcurrentUpdatesSingleWinner(t *testing.T) {
	t.Parallel()
	r := seeded()

	var wins, conflicts atomic.Int64
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.Update(t.Context(), "x", 1, func(rec *Record) { rec.Name = "w" })
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrVersionConflict):
				conflicts.Add(1)
			default:
				t.Errorf("goroutine %d: unexpected err %v", i, err)
			}
		}()
	}
	wg.Wait()

	if wins.Load() != 1 || conflicts.Load() != 1 {
		t.Fatalf("wins=%d conflicts=%d; want 1 and 1", wins.Load(), conflicts.Load())
	}
}
```

## Review

The update is correct when not-found, conflict, and success read top-to-bottom, a
rejected update mutates nothing, and check-and-apply happen inside one lock so exactly
one of two same-version writers wins. The mistakes to avoid are performing the version
check and the store in separate critical sections (both writers pass, both write —
the lost update returns), mutating the stored value in place before the check (a
failed update must leave the row untouched, so mutate a copy), and treating a missing
row as an implicit create. A real datastore does the same check with a
`WHERE version = ?` clause and a row count; the control flow is identical.

## Resources

- [Optimistic concurrency control](https://en.wikipedia.org/wiki/Optimistic_concurrency_control)
- [Go maps: the comma-ok idiom](https://go.dev/blog/maps)
- [errors.Is](https://pkg.go.dev/errors#Is)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-idempotency-guard.md](06-idempotency-guard.md) | Next: [08-token-bucket-admission.md](08-token-bucket-admission.md)
