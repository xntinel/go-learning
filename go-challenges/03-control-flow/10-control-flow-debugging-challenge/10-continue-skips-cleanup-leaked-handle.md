# Exercise 10: The Row Importer That Leaked a Handle on Every Invalid Row

**Nivel: Intermedio** — validacion rapida (un test corto).

A batch importer opens one staging handle per row, validates the row, and
commits it. It shipped with a `continue` placed above the cleanup call, so an
invalid row skipped straight to the next iteration without releasing its
handle. Over a small batch nothing looks wrong; over a real feed the process
runs out of staging handles. You will reproduce it with a mixed batch,
diagnose the `continue`, and fix the ordering.

## What you'll build

```text
rowimport/                module example.com/rowimport
  go.mod
  rowimport.go            Store (open-handle counter), Row, ImportBatch
  rowimport_test.go        committed rows + open-handle-count assertion
```

- Files: `rowimport.go`, `rowimport_test.go`.
- Implement: `ImportBatch(*Store, []Row) (committed []string, err error)` that stages, validates, and commits each row, closing the staging handle on *every* exit from the loop body.
- Test: run a batch with one invalid row and assert both the committed IDs and that `Store.Open()` is back to `0` afterward.
- Verify: `go test -count=1 ./...`.

### The artifact and the planted bug

```go
func ImportBatch(s *Store, rows []Row) (committed []string, err error) {
	for _, r := range rows {
		s.OpenStaging()
		if r.Value < 0 {
			continue // BUG: skips CloseStaging, leaking the handle
		}
		committed = append(committed, r.ID)
		s.CloseStaging()
	}
	return committed, nil
}
```

The happy path opens, commits, and closes — that passes review at a glance.
But the row is staged *before* it is validated, and the `continue` for a
rejected row jumps to the next iteration without ever reaching
`CloseStaging`. Each invalid row leaks one handle permanently; a steady
trickle of bad rows exhausts the pool exactly like an unclosed file
descriptor.

The failing row reads:

```text
--- FAIL: TestImportBatchClosesHandleOnInvalidRow
    rowimport_test.go:20: open handles = 1, want 0
```

The fix releases the handle on *both* exits from the loop body — the reject
path and the commit path — before moving on:

```go
func ImportBatch(s *Store, rows []Row) (committed []string, err error) {
	for _, r := range rows {
		s.OpenStaging()
		if r.Value < 0 {
			s.CloseStaging()
			continue
		}
		committed = append(committed, r.ID)
		s.CloseStaging()
	}
	return committed, nil
}
```

Create `rowimport.go`:

```go
package rowimport

// Store tracks how many staging handles are currently open. In production
// this stands in for a file descriptor, a DB cursor, or a pooled connection.
type Store struct {
	open int
}

func (s *Store) OpenStaging()  { s.open++ }
func (s *Store) CloseStaging() { s.open-- }
func (s *Store) Open() int     { return s.open }

// Row is one record from the import feed.
type Row struct {
	ID    string
	Value int
}

// ImportBatch stages each row, validates it, and commits the valid ones,
// releasing the staging handle on every exit from the loop body.
func ImportBatch(s *Store, rows []Row) (committed []string, err error) {
	for _, r := range rows {
		s.OpenStaging()
		if r.Value < 0 {
			s.CloseStaging()
			continue
		}
		committed = append(committed, r.ID)
		s.CloseStaging()
	}
	return committed, nil
}
```

### Tests

Create `rowimport_test.go`:

```go
package rowimport

import "testing"

func TestImportBatchClosesHandleOnInvalidRow(t *testing.T) {
	s := &Store{}
	rows := []Row{
		{ID: "r1", Value: 10},
		{ID: "r2", Value: -1},
		{ID: "r3", Value: 5},
	}
	committed, err := ImportBatch(s, rows)
	if err != nil {
		t.Fatalf("ImportBatch error = %v, want nil", err)
	}
	if len(committed) != 2 || committed[0] != "r1" || committed[1] != "r3" {
		t.Fatalf("committed = %v, want [r1 r3]", committed)
	}
	if got := s.Open(); got != 0 {
		t.Fatalf("open handles = %d, want 0", got)
	}
}
```

Run: `go test -count=1 ./...`.

## Review

A `continue` that fires before a cleanup call is the same class of bug as a
missing `defer`, just without the keyword to search for. The invariant is
"every open has a matching close on every exit path" — reject and commit are
both exits from the loop body, and both must release the handle. The test
proves it by counting open handles after a batch that mixes valid and
invalid rows, not by only checking that the valid rows made it through.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — `continue` transfers control to the post statement, skipping everything below it in the body.
- [Effective Go: Control structures](https://go.dev/doc/effective_go#control-structures) — idiomatic early-exit patterns and why cleanup must precede them.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-type-switch-event-dispatch-default.md](09-type-switch-event-dispatch-default.md) | Next: [11-batch-chunking-off-by-one-drops-tail.md](11-batch-chunking-off-by-one-drops-tail.md)
