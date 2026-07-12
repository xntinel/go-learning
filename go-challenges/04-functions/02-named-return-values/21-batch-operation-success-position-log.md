# Exercise 21: Track Successful Batch Item Positions

A batch job that keeps going past individual item failures needs to report,
at the end, exactly which positions succeeded — not just a count, and not
just an error. This exercise builds a batch processor that accumulates
success indices into a named slice result and logs the final tally through a
single deferred closure, so the logging cannot be scattered across (or
forgotten at) each of the batch's several possible exit shapes.

**Nivel: Intermedio** — validacion rapida (una tabla corta de tres casos).

## What you'll build

```text
batchlog/                    independent module: example.com/batchlog
  go.mod
  batchlog.go                 ProcessBatch (named successIdx, deferred tally log)
  cmd/demo/
    main.go                   runnable demo: a batch with two failures interleaved
  batchlog_test.go             all succeed, one failure, all fail — logged message asserted
```

- Files: `batchlog.go`, `cmd/demo/main.go`, `batchlog_test.go`.
- Implement: `ProcessBatch(items []string, process func(string) error, logf func(msg string)) (successIdx []int, err error)` that keeps processing past failures and logs a final tally via `logf`.
- Test: all items succeed, one item fails in the middle, and every item fails — asserting both `successIdx` and the exact logged message each time.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/21-batch-operation-success-position-log/cmd/demo
cd go-solutions/04-functions/02-named-return-values/21-batch-operation-success-position-log
go mod edit -go=1.24
```

### One log line, fed by the named result it is reporting on

```go
defer func() {
    logf(fmt.Sprintf("batch complete: %d/%d succeeded, positions=%v", len(successIdx), len(items), successIdx))
}()

var errs []error
for i, item := range items {
    if perr := process(item); perr != nil {
        errs = append(errs, fmt.Errorf("item %d: %w", i, perr))
        continue
    }
    successIdx = append(successIdx, i)
}
err = errors.Join(errs...)
return
```

Because `successIdx` is a named result, the deferred closure can read its
final value directly — whatever the loop built up — without `ProcessBatch`
needing a separate local variable threaded through to a logging call at the
bottom of the function. The log line always reflects reality: it runs after
the loop, so it sees every index the loop appended, on every possible
outcome (full success, partial failure, or every item failing).

Create `batchlog.go`:

```go
package batchlog

import (
	"errors"
	"fmt"
)

// ProcessBatch runs process over each item, continuing past individual
// failures so one bad item does not stop the rest of the batch. successIdx
// is a named result that accumulates the index of every item that
// succeeded, in order.
//
// The named result is what lets a single deferred closure log the final
// tally: the defer runs after the loop has finished populating successIdx,
// reads it (and len(items)) directly from the named result, and reports it
// through logf on every exit — success, partial failure, or a fully failed
// batch — without the caller having to remember to log it themselves at
// each return point.
func ProcessBatch(items []string, process func(string) error, logf func(msg string)) (successIdx []int, err error) {
	defer func() {
		logf(fmt.Sprintf("batch complete: %d/%d succeeded, positions=%v", len(successIdx), len(items), successIdx))
	}()

	var errs []error
	for i, item := range items {
		if perr := process(item); perr != nil {
			errs = append(errs, fmt.Errorf("item %d: %w", i, perr))
			continue
		}
		successIdx = append(successIdx, i)
	}
	err = errors.Join(errs...)
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

	"example.com/batchlog"
)

func main() {
	items := []string{"ok1", "bad", "ok2", "bad", "ok3"}
	process := func(s string) error {
		if s == "bad" {
			return errors.New("rejected")
		}
		return nil
	}

	var logged string
	successIdx, err := batchlog.ProcessBatch(items, process, func(msg string) {
		logged = msg
	})

	fmt.Printf("successIdx=%v err=%v\n", successIdx, err)
	fmt.Println("logged:", logged)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
successIdx=[0 2 4] err=item 1: rejected
item 3: rejected
logged: batch complete: 3/5 succeeded, positions=[0 2 4]
```

### Tests

Create `batchlog_test.go`:

```go
package batchlog

import (
	"errors"
	"reflect"
	"testing"
)

func TestProcessBatch(t *testing.T) {
	t.Parallel()

	fails := func(bad string) func(string) error {
		return func(s string) error {
			if s == bad {
				return errors.New("rejected")
			}
			return nil
		}
	}

	tests := []struct {
		name         string
		items        []string
		bad          string
		wantSuccess  []int
		wantErr      bool
		wantLogMatch string
	}{
		{
			name:         "all succeed",
			items:        []string{"a", "b", "c"},
			bad:          "",
			wantSuccess:  []int{0, 1, 2},
			wantErr:      false,
			wantLogMatch: "batch complete: 3/3 succeeded, positions=[0 1 2]",
		},
		{
			name:         "one failure in the middle",
			items:        []string{"a", "bad", "c"},
			bad:          "bad",
			wantSuccess:  []int{0, 2},
			wantErr:      true,
			wantLogMatch: "batch complete: 2/3 succeeded, positions=[0 2]",
		},
		{
			name:         "all fail",
			items:        []string{"bad", "bad"},
			bad:          "bad",
			wantSuccess:  nil,
			wantErr:      true,
			wantLogMatch: "batch complete: 0/2 succeeded, positions=[]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logged string
			successIdx, err := ProcessBatch(tt.items, fails(tt.bad), func(msg string) {
				logged = msg
			})

			if !reflect.DeepEqual(successIdx, tt.wantSuccess) {
				t.Fatalf("successIdx = %v, want %v", successIdx, tt.wantSuccess)
			}
			if tt.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if logged != tt.wantLogMatch {
				t.Fatalf("logged = %q, want %q", logged, tt.wantLogMatch)
			}
		})
	}
}
```

## Review

`ProcessBatch` is correct when it never stops early on a single item's
failure, `successIdx` holds exactly the indices that succeeded in order, and
the logged tally always matches both `successIdx` and the total item count.
The named result `successIdx` is what makes the deferred log line possible in
the first place — a defer can only read a result it has a name for. The
mistake to avoid is logging inside the loop at each failure instead of once
at the end: that would spam a log line per failed item instead of the single
summary a batch job's operator actually wants to see.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join)
- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-operation-deadline-hard-cancel.md](20-operation-deadline-hard-cancel.md) | Next: [22-http-response-trailer-header-injection.md](22-http-response-trailer-header-injection.md)
