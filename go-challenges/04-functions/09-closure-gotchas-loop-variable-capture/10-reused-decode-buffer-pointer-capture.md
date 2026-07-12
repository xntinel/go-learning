# Exercise 10: Batch Import Decode Buffer: Pointer Capture Into a Pending Slice

**Nivel: Intermedio** — validacion rapida (un test corto).

A batch import job parses "id,amount" lines and appends a `*Record` per line
into a pending slice for downstream processing (dedup, bulk insert). Go 1.22
fixed the loop-variable-capture bug, but it did nothing for a variable YOU
declare above the loop and reuse by hand: if one `Record` is allocated once
and mutated every iteration, every pointer you append aliases the same
storage.

## What you'll build

```text
batchimport/                 independent module: example.com/batchimport
  go.mod                     go 1.24
  batchimport.go              Record, ParsePending, ParsePendingBuggy
  batchimport_test.go         table test: fresh alloc vs. reused-buffer capture
```

- Files: `batchimport.go`, `batchimport_test.go`.
- Implement: `ParsePending(lines) ([]*Record, error)` allocating a fresh `Record` per line; `ParsePendingBuggy` reusing one `Record` declared above the loop, to see the failure mode it produces.
- Test: one table test comparing both against the same input lines.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/10-reused-decode-buffer-pointer-capture
cd go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/10-reused-decode-buffer-pointer-capture
go mod edit -go=1.24
```

### The reused-buffer trap has nothing to do with the range variable

`ParsePendingBuggy` declares `var rec Record` ABOVE the loop and appends
`&rec` every iteration. The `line` range variable is per-iteration and fine
on a `go 1.24` module — the bug is the hand-declared `rec`, one storage
location the whole loop shares. `ParsePending` fixes it the same way you fix
a captured loop variable: give each iteration its own storage, via
`new(Record)` inside the loop instead of reusing one declared outside it.

Create `batchimport.go`:

```go
package batchimport

import (
	"fmt"
	"strconv"
	"strings"
)

// Record is one parsed import line pending downstream processing.
type Record struct {
	ID     string
	Amount float64
}

// ParsePendingBuggy parses each "id,amount" line into a single reused Record
// and appends its address to the pending slice. Every pointer in the result
// ends up aliasing the SAME storage location, so after the loop every entry
// holds only the last line's values.
func ParsePendingBuggy(lines []string) ([]*Record, error) {
	pending := make([]*Record, 0, len(lines))
	var rec Record // BUG: one shared Record reused across every iteration
	for _, line := range lines {
		parts := strings.SplitN(line, ",", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed line %q", line)
		}
		amt, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return nil, fmt.Errorf("parse amount in %q: %w", line, err)
		}
		rec.ID = parts[0]
		rec.Amount = amt
		pending = append(pending, &rec) // aliases the same storage every time
	}
	return pending, nil
}

// ParsePending parses each "id,amount" line into a freshly allocated Record
// per iteration, so each pointer in the result stays independently correct.
func ParsePending(lines []string) ([]*Record, error) {
	pending := make([]*Record, 0, len(lines))
	for _, line := range lines {
		parts := strings.SplitN(line, ",", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed line %q", line)
		}
		amt, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return nil, fmt.Errorf("parse amount in %q: %w", line, err)
		}
		rec := new(Record) // fresh allocation every iteration
		rec.ID = parts[0]
		rec.Amount = amt
		pending = append(pending, rec)
	}
	return pending, nil
}
```

### Test

One table test drives both functions against the same three lines and checks
the per-entry values each produces.

Create `batchimport_test.go`:

```go
package batchimport

import "testing"

func TestParsePending(t *testing.T) {
	lines := []string{"a,1.5", "b,2.5", "c,3.5"}

	tests := []struct {
		name  string
		parse func([]string) ([]*Record, error)
		want  []Record // want[i] is the expected value at pending[i]
	}{
		{
			name:  "fresh allocation per line keeps each record correct",
			parse: ParsePending,
			want:  []Record{{ID: "a", Amount: 1.5}, {ID: "b", Amount: 2.5}, {ID: "c", Amount: 3.5}},
		},
		{
			name:  "reused shared record collapses every entry onto the last line",
			parse: ParsePendingBuggy,
			want:  []Record{{ID: "c", Amount: 3.5}, {ID: "c", Amount: 3.5}, {ID: "c", Amount: 3.5}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pending, err := tt.parse(lines)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(pending) != len(tt.want) {
				t.Fatalf("len(pending) = %d, want %d", len(pending), len(tt.want))
			}
			for i, want := range tt.want {
				if *pending[i] != want {
					t.Fatalf("pending[%d] = %+v, want %+v", i, *pending[i], want)
				}
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`ParsePendingBuggy` proves Go 1.22's per-iteration range variable does not
protect a variable you declare and reuse by hand: `rec` lives above the loop,
so every appended `&rec` is the same address, and the table test shows all
three entries collapsing onto the last line. `ParsePending` fixes it with
`new(Record)` inside the loop body — the same "own storage per iteration"
discipline, applied to a manually declared variable instead of a range one.

## Resources

- [Effective Go: Allocation with `new`](https://go.dev/doc/effective_go#allocation_new) — what a fresh allocation per iteration actually buys you.
- [Go wiki: Loop variable capture](https://go.dev/wiki/LoopvarExperiment) — the per-iteration variable change, and what it does not cover.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-per-key-memoizer.md](09-per-key-memoizer.md) | Next: [11-defer-named-return-shadowed-err.md](11-defer-named-return-shadowed-err.md)
