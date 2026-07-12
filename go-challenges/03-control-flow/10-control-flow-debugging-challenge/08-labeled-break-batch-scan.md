# Exercise 8: The Duplicate-Detector Whose Inner break Never Left the Batch

A nested-loop scan over batches of records looks for the first cross-batch
duplicate key. It shipped with a plain `break` that exits only the inner loop, so
the outer loop kept scanning and reported a *later* duplicate instead of the
first. You will reproduce with a test that expects a single early match, diagnose
that the break must leave both loops, and fix it with a labeled break.

## What you'll build

```text
dedup/                     module example.com/dedup
  go.mod
  dedup.go                 Batch; FirstDuplicate; internal scan returning an examined counter
  cmd/demo/
    main.go                runnable demo: scan a few batches of order IDs
  dedup_test.go            first-match table, early-termination counter, Example
```

- Files: `dedup.go`, `cmd/demo/main.go`, `dedup_test.go`.
- Implement: `FirstDuplicate([]Batch) (string, bool)` over a `map[string]struct{}` seen-set with a labeled `break Scan` that leaves both loops at the first duplicate; an internal `scan` returning how many keys were examined.
- Test: a table where the first duplicate is early and later duplicates exist (the exact key proves which one fired); an examined-count assertion proving the outer loop stopped; a no-duplicate case returning the sentinel.
- Verify: `go test -count=1 -race ./...`.

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/08-labeled-break-batch-scan/cmd/demo
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/08-labeled-break-batch-scan
```

### The artifact and the planted bug

The scanner walks batches in order, remembering every key it has seen, and stops
at the first key that repeats across the stream. The version that shipped used a
plain `break`:

```go
func FirstDuplicate(batches []Batch) (string, bool) {
	seen := make(map[string]struct{})
	var dup string
	found := false
	for _, batch := range batches {
		for _, key := range batch {
			if _, ok := seen[key]; ok {
				dup = key
				found = true
				break // BUG: leaves only the inner loop; the outer scan continues
			}
			seen[key] = struct{}{}
		}
	}
	return dup, found
}
```

A plain `break` exits only the innermost loop. After it finds the first duplicate
and breaks the inner loop, the *outer* loop advances to the next batch and keeps
scanning — and if a later key also repeats, it overwrites `dup`. So the function
returns the *last* cross-batch duplicate, not the first, and does more work than
it should. It survives review because on inputs with exactly one duplicate the
answer happens to be right.

The failing test reads:

```text
--- FAIL: TestFirstDuplicate/first_duplicate_is_early (0.00s)
    dedup_test.go:44: FirstDuplicate = "x", true; want "b", true
```

`b` is the first repeat, but the scan returned `x`, a later one — the outer loop
never stopped. The fix labels the outer loop and uses `break Scan` to leave both
loops at once.

Create `dedup.go`:

```go
package dedup

// Batch is a group of record keys delivered together.
type Batch []string

// FirstDuplicate returns the first key that repeats across the batches (in scan
// order) and true; if no key repeats it returns "" and false. It stops at the
// first duplicate.
func FirstDuplicate(batches []Batch) (string, bool) {
	key, found, _ := scan(batches)
	return key, found
}

// scan is FirstDuplicate plus the number of keys examined, so a test can prove
// the outer loop terminated early. The labeled break leaves both loops at the
// first duplicate; a plain break would leave only the inner one.
func scan(batches []Batch) (key string, found bool, examined int) {
	seen := make(map[string]struct{})
Scan:
	for _, batch := range batches {
		for _, k := range batch {
			examined++
			if _, ok := seen[k]; ok {
				key, found = k, true
				break Scan
			}
			seen[k] = struct{}{}
		}
	}
	return key, found, examined
}
```

`break Scan` transfers control to just after the labeled `for`, terminating both
loops at the first duplicate. Its counterpart, `continue Scan`, would advance the
*outer* loop from inside the inner one — useful when a batch-level condition means
"skip the rest of this batch and move to the next". The `examined` counter is not
part of the public contract; it exists so the test can assert the scan stopped
rather than ran to the end.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/dedup"
)

func main() {
	batches := []dedup.Batch{
		{"order-1", "order-2", "order-3"},
		{"order-4", "order-2"},
		{"order-5"},
	}
	if key, ok := dedup.FirstDuplicate(batches); ok {
		fmt.Printf("first duplicate key: %s\n", key)
	} else {
		fmt.Println("no duplicates")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
first duplicate key: order-2
```

### Tests

`TestFirstDuplicate` is the table: the key case has an early duplicate (`b`) and a
*later* one (`x`), so asserting the exact returned key distinguishes the correct
first-match from the buggy last-match. It also covers a same-batch duplicate, a
no-duplicate input returning the sentinel, and an empty input. `TestStopsEarly`
calls the internal `scan` and asserts the examined count, proving the outer loop
terminated at the first duplicate rather than running to the end.

Create `dedup_test.go`:

```go
package dedup

import (
	"fmt"
	"testing"
)

func TestFirstDuplicate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		batches []Batch
		wantKey string
		wantOK  bool
	}{
		{
			name: "first duplicate is early",
			batches: []Batch{
				{"a", "b", "c"},
				{"x", "b", "y"}, // b repeats first
				{"z", "x"},      // x repeats later; a last-match bug returns this
			},
			wantKey: "b",
			wantOK:  true,
		},
		{
			name:    "duplicate within one batch",
			batches: []Batch{{"a", "a"}},
			wantKey: "a",
			wantOK:  true,
		},
		{
			name:    "no duplicates",
			batches: []Batch{{"a", "b"}, {"c", "d"}},
			wantKey: "",
			wantOK:  false,
		},
		{
			name:    "empty",
			batches: nil,
			wantKey: "",
			wantOK:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			key, ok := FirstDuplicate(tc.batches)
			if key != tc.wantKey || ok != tc.wantOK {
				t.Fatalf("FirstDuplicate = %q, %v; want %q, %v", key, ok, tc.wantKey, tc.wantOK)
			}
		})
	}
}

func TestStopsEarly(t *testing.T) {
	t.Parallel()

	batches := []Batch{{"a", "b", "c"}, {"x", "b", "y"}, {"z", "x"}}
	key, found, examined := scan(batches)
	if key != "b" || !found {
		t.Fatalf("scan key = %q, found = %v; want b, true", key, found)
	}
	// a, b, c, x, b -> the fifth key is the first duplicate; a labeled break
	// stops here. A plain break would examine all eight keys.
	if examined != 5 {
		t.Fatalf("examined = %d, want 5 (outer loop must stop at the first duplicate)", examined)
	}
}

func ExampleFirstDuplicate() {
	key, ok := FirstDuplicate([]Batch{{"user1", "user2"}, {"user3", "user1"}})
	fmt.Println(key, ok)
	// Output: user1 true
}
```

## Review

The scanner is correct when it returns the *first* cross-batch duplicate and stops
there. Two checks pin that: asserting the exact key (an early `b` versus a later
`x`) proves it did not keep scanning to a later match, and asserting the examined
count proves the outer loop actually terminated rather than merely producing the
right answer by luck. A plain `break` inside nested loops leaves only the inner
loop; when the decision to stop is made inside but must end the whole scan, label
the outer loop and `break Outer`. Reach for `continue Outer` when the inner loop
should abandon the current outer iteration and advance to the next.

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — `break` with a label.
- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — `continue` with a label.
- [Effective Go: Labels](https://go.dev/doc/effective_go#control-structures) — labeled break/continue for nested loops.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-recover-middleware-swallows-panic.md](07-recover-middleware-swallows-panic.md) | Next: [09-type-switch-event-dispatch-default.md](09-type-switch-event-dispatch-default.md)
