# Exercise 14: Staged Batch Write — Defer Discard Unless Committed

**Nivel: Intermedio** — validacion rapida (un test corto).

The atomic-write idiom — write to a temp file, then `defer os.Remove(tmp)`
unless you renamed it — generalizes to any staged batch: build the new state
somewhere private, and let a deferred "discard unless committed" guard clean
it up on any exit that is not the explicit commit. This module builds that
guard over an in-memory staging area, no filesystem required.

## What you'll build

```text
staged/                     independent module: example.com/staged
  go.mod
  staged/staged.go           Area (Visible/pending/committed); WriteBatch (commit-flag defer)
  staged/staged_test.go      table test: all valid rows vs. an invalid row midway; plus a panic case
```

- Files: `staged/staged.go`, `staged/staged_test.go`.
- Implement: `WriteBatch(a *Area, rows []string) (err error)` that stages every row into a private `pending` slice, validates as it goes, and calls `commit` (append `pending` onto `Visible` in one step) only once every row is valid; the deferred discard runs unconditionally but is a no-op once `commit` has flipped a `committed` flag.
- Test: a table test with all valid rows (batch becomes visible) and an empty row midway (whatever was staged is discarded, `Visible` untouched); a separate test asserting a panic mid-batch also discards `pending` and leaves `Visible` untouched before the panic propagates.

Set up the module:

```bash
mkdir -p go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/14-staged-write-discard-unless-committed/staged
cd go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/14-staged-write-discard-unless-committed
go mod edit -go=1.24
```

Create `staged/staged.go`:

```go
package staged

import "fmt"

// Area holds rows staged for a batch write. Visible is what readers see;
// pending is the uncommitted batch being built up by WriteBatch.
type Area struct {
	Visible   []string
	pending   []string
	committed bool
}

func (a *Area) stage(row string) { a.pending = append(a.pending, row) }

func (a *Area) commit() {
	a.Visible = append(a.Visible, a.pending...)
	a.pending = nil
	a.committed = true
}

func (a *Area) discardPending() { a.pending = nil }

// WriteBatch stages every row into a private pending slice, validates as it
// goes, and only calls commit -- which appends pending onto Visible in one
// step -- once every row in the batch is valid. The deferred discard is a
// commit-flag guard: it runs unconditionally on every return path, including
// a panic mid-batch, but is a no-op once commit has flipped a.committed to
// true. Unlike a snapshot/restore of already-live state, there is nothing to
// put back here -- Visible is never touched until the very end, so an
// incomplete batch simply never reaches it, and no recover is needed because
// the discard does not need to inspect or alter the panic value.
func WriteBatch(a *Area, rows []string) (err error) {
	a.committed = false
	defer func() {
		if !a.committed {
			a.discardPending()
		}
	}()

	for _, r := range rows {
		if r == "" {
			return fmt.Errorf("staged write: empty row")
		}
		if r == "__panic__" {
			panic("corrupt row")
		}
		a.stage(r)
	}
	a.commit()
	return nil
}
```

Create `staged/staged_test.go`:

```go
package staged

import (
	"slices"
	"testing"
)

func TestWriteBatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		preexisting []string
		rows        []string
		wantErr     bool
		wantVisible []string
	}{
		{
			name:        "all valid rows become visible in one step",
			preexisting: nil,
			rows:        []string{"row-1", "row-2"},
			wantErr:     false,
			wantVisible: []string{"row-1", "row-2"},
		},
		{
			name:        "empty row midway discards the whole batch",
			preexisting: []string{"old-row"},
			rows:        []string{"row-1", "", "row-3"},
			wantErr:     true,
			wantVisible: []string{"old-row"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Area{Visible: append([]string(nil), tt.preexisting...)}
			err := WriteBatch(a, tt.rows)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(a.Visible, tt.wantVisible) {
				t.Fatalf("Visible = %v, want %v", a.Visible, tt.wantVisible)
			}
			if a.pending != nil {
				t.Fatalf("pending = %v, want nil after WriteBatch returns", a.pending)
			}
		})
	}
}

func TestWriteBatchPanicDiscardsPending(t *testing.T) {
	t.Parallel()

	a := &Area{Visible: []string{"old-row"}}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = WriteBatch(a, []string{"row-1", "__panic__"})
	}()

	want := []string{"old-row"}
	if !slices.Equal(a.Visible, want) {
		t.Fatalf("Visible = %v, want %v (untouched)", a.Visible, want)
	}
	if a.pending != nil {
		t.Fatalf("pending = %v, want nil (discarded)", a.pending)
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The guard is a single boolean, checked once, in one deferred closure — simpler
than a snapshot/restore because `Visible` is never mutated until `commit`
actually runs, so there is nothing to put back on failure, only pending work to
throw away. That is also why no `recover()` is needed: the discard does not
need to see or alter the panic value, so `defer` alone is sufficient, and the
panic keeps propagating untouched after the deferred closure runs. This is the
same shape as `defer os.Remove(tmpFile)` unless you renamed it into place —
commit is a rename (or, here, one append), and everything before it is
disposable.

## Resources

- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [os.Rename](https://pkg.go.dev/os#Rename)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [13-multi-sink-close-errors-join.md](13-multi-sink-close-errors-join.md) | Next: [15-request-scoped-accumulator-defer-flush.md](15-request-scoped-accumulator-defer-flush.md)
