# Exercise 30: Range Loop Shadowing Bug Breaks Deferred Handler

Every earlier exercise in this chapter has relied on one guarantee: a
deferred closure that inspects a named `err` result sees whatever the
function's return statements actually put there. That guarantee has exactly
one common way to break — `if err := something(); ...` inside a nested
scope, where `:=` declares a brand-new local variable that merely happens to
share the name `err`. This exercise reproduces the bug deliberately, side by
side with the one-character fix, so the failure mode is unmistakable instead
of theoretical.

**Nivel: Avanzado** — validacion normal (tabla de casos que documenta el bug y confirma el fix).

## What you'll build

```text
rangeerr/                   independent module: example.com/rangeerr
  go.mod
  rangeerr.go                ProcessAllBuggy (shadowed err) and ProcessAllFixed (assigned err)
  cmd/demo/
    main.go                  runnable demo: same input through both versions
  rangeerr_test.go            table proving the buggy version always reports success; fixed version doesn't
```

- Files: `rangeerr.go`, `cmd/demo/main.go`, `rangeerr_test.go`.
- Implement: `ProcessAllBuggy(items []Item) (processed int, err error)` using `if err := processItem(it); err != nil { break }` (shadowing bug), and `ProcessAllFixed` using `if err = processItem(it); err != nil { return }` (assignment, no shadow).
- Test: a table of item slices with failures at different positions, asserting the buggy version's `err` is always nil despite a failure, and the fixed version's `err` correctly reports it.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The one-character difference between a bug and a fix

```go
// BUG: := declares a new local err, shadowing the named result.
if err := processItem(it); err != nil {
    break
}
...
return // named err is still nil here — the failure was lost

// FIX: = assigns to the named result; there is no shadow.
if err = processItem(it); err != nil {
    return
}
```

In `ProcessAllBuggy`, `if err := processItem(it); err != nil` looks correct
at a glance — it reads like it's checking *the* error. But `:=` inside an
`if` statement's init clause always declares a new variable scoped to that
`if`, even when a variable of the same name already exists in an outer
scope. That inner `err` shadows the named result for the lifetime of the
`if` block, and once the block ends, the inner `err` is gone; the outer,
named `err` was never touched. The `break` exits the loop having recorded
nothing, and the naked `return` after it sends back whatever the named `err`
already was — nil, in this case, since nothing ever assigned to it. The
deferred closure that wraps errors in every other exercise in this chapter
would run here too, and it would wrap nothing, because there is nothing to
wrap.

`ProcessAllFixed` changes exactly one token: `err = processItem(it)`, plain
assignment, writes into the outer named `err` — no new variable, no shadow,
and the deferred closure now sees the real failure.

Create `rangeerr.go`:

```go
package rangeerr

import "fmt"

// Item is one unit of work; Fail simulates a processing failure.
type Item struct {
	ID   string
	Fail bool
}

func processItem(it Item) error {
	if it.Fail {
		return fmt.Errorf("item %s failed", it.ID)
	}
	return nil
}

// ProcessAllBuggy demonstrates a common shadowing trap: `if err := ...` inside
// the loop declares a NEW local variable that shadows the named result err.
// The deferred closure below only ever observes the outer, always-nil err, so
// a naked return after the loop silently discards the failure — the bug this
// whole package exists to show.
func ProcessAllBuggy(items []Item) (processed int, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("ProcessAllBuggy: %w", err)
		}
	}()

	for _, it := range items {
		if err := processItem(it); err != nil { // BUG: := shadows the named err
			break
		}
		processed++
	}
	return // named err is still nil here: the failure was lost
}

// ProcessAllFixed is the same loop with the shadowing removed: `=` assigns to
// the named result instead of declaring a new local, so the deferred closure
// sees the real failure.
func ProcessAllFixed(items []Item) (processed int, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("ProcessAllFixed: %w", err)
		}
	}()

	for _, it := range items {
		if err = processItem(it); err != nil { // FIX: = assigns to named err
			return
		}
		processed++
	}
	return
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/rangeerr"
)

func main() {
	items := []rangeerr.Item{
		{ID: "1"},
		{ID: "2", Fail: true},
		{ID: "3"},
	}

	processed, err := rangeerr.ProcessAllBuggy(items)
	fmt.Printf("buggy:  processed=%d err=%v\n", processed, err)

	processed, err = rangeerr.ProcessAllFixed(items)
	fmt.Printf("fixed:  processed=%d err=%v\n", processed, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy:  processed=1 err=<nil>
fixed:  processed=1 err=ProcessAllFixed: item 2 failed
```

### Tests

Create `rangeerr_test.go`:

```go
package rangeerr

import (
	"strings"
	"testing"
)

func TestProcessAllBuggyLosesTheError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		items         []Item
		wantProcessed int
	}{
		{"failure in middle", []Item{{ID: "1"}, {ID: "2", Fail: true}, {ID: "3"}}, 1},
		{"failure first", []Item{{ID: "1", Fail: true}, {ID: "2"}}, 0},
		{"no failure", []Item{{ID: "1"}, {ID: "2"}}, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			processed, err := ProcessAllBuggy(tc.items)
			// This documents the trap: even when an item fails, the
			// shadowed err never reaches the named result, so the naked
			// return always reports success.
			if err != nil {
				t.Fatalf("ProcessAllBuggy err = %v, want nil (bug: shadowed err is always lost)", err)
			}
			if processed != tc.wantProcessed {
				t.Fatalf("processed = %d, want %d", processed, tc.wantProcessed)
			}
		})
	}
}

func TestProcessAllFixedReportsTheError(t *testing.T) {
	t.Parallel()

	items := []Item{{ID: "1"}, {ID: "2", Fail: true}, {ID: "3"}}
	processed, err := ProcessAllFixed(items)
	if err == nil {
		t.Fatal("ProcessAllFixed: want error, got nil")
	}
	if !strings.Contains(err.Error(), "item 2 failed") {
		t.Fatalf("err = %v, want it to mention item 2", err)
	}
	if processed != 1 {
		t.Fatalf("processed = %d, want 1 (stops at the failing item)", processed)
	}
}

func TestProcessAllFixedSuccessPath(t *testing.T) {
	t.Parallel()

	items := []Item{{ID: "1"}, {ID: "2"}, {ID: "3"}}
	processed, err := ProcessAllFixed(items)
	if err != nil {
		t.Fatalf("ProcessAllFixed: unexpected error: %v", err)
	}
	if processed != 3 {
		t.Fatalf("processed = %d, want 3", processed)
	}
}
```

## Review

`ProcessAllBuggy` is not a bug in `processItem`, in the loop's iteration
order, or in the deferred closure's wrapping logic — every one of those is
correct in isolation. The bug is entirely in one token: `:=` instead of `=`
inside the `if` that checks `processItem`'s result. `TestProcessAllBuggyLosesTheError`
documents the failure mode rather than hiding it: it asserts the buggy
version's `err` is nil even when an item fails, so the trap stays visible in
the test suite instead of being "fixed" by deleting the function. The
takeaway to carry into every other exercise in this chapter: whenever a
deferred closure reads a named result, every assignment to that result
inside a nested scope (an `if`, a `for`, a `switch` case) must use `=`, never
`:=` — a `go vet -shadow`-style check (not part of the default `go vet`
suite) exists precisely because this mistake compiles cleanly and produces
no warning on its own.

## Resources

- [Go Spec: Declarations and scope](https://go.dev/ref/spec#Declarations_and_scope)
- [Go Spec: Short variable declarations](https://go.dev/ref/spec#Short_variable_declarations)
- [Effective Go: Named result parameters](https://go.dev/doc/effective_go#named-results)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-request-trace-context-propagation.md](29-request-trace-context-propagation.md) | Next: [31-multi-file-handle-partial-close-unwind.md](31-multi-file-handle-partial-close-unwind.md)
