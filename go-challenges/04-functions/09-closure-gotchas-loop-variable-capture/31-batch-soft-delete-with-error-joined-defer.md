# Exercise 31: Batch Soft Delete: Aggregated Error Accumulation with Defer in Loop

**Nivel: Intermedio** — validacion rapida (un test corto).

A batch soft-delete job is meant to give up after a handful of failures, so
one bad batch cannot hammer a failing downstream forever. The check happens
at the top of the loop: `if len(errs) >= maxErrors { break }`. The trap is
wrapping each failure's append to `errs` in `defer` — which means NONE of
those appends run until the function itself returns, so the guard can never
see an earlier iteration's failure, and — worse — the function's own
`return errors.Join(errs...)` computes its result before any deferred append
has even run, silently returning `nil` for a batch that failed repeatedly.

## What you'll build

```text
softdelete/                  independent module: example.com/softdelete
  go.mod                     go 1.24
  softdelete.go               DeleteFunc, BuggyDeleteBatch, DeleteBatch
  cmd/
    demo/
      main.go                runnable demo: 5 ids, 3 failing, print attempted + error
  softdelete_test.go           early stop works vs never stops; all-succeed and zero-max edge cases
```

- Files: `softdelete.go`, `cmd/demo/main.go`, `softdelete_test.go`.
- Implement: `BuggyDeleteBatch` deferring each failure's append to `errs`; `DeleteBatch` appending immediately so the `len(errs)` guard actually works.
- Test: assert `DeleteBatch` stops after `maxErrors` failures and returns a non-nil joined error; assert `BuggyDeleteBatch` attempts every id regardless of failures AND returns a `nil` error even though every failure happened.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why deferring the append breaks both the guard and the return value

`defer` schedules a call to run when the *enclosing function* returns.
`BuggyDeleteBatch` defers each failure's `errs = append(errs, e)` inside the
loop, so at the top of every iteration `len(errs)` still reads its value from
BEFORE any deferred append has run — which is always `0`, for the entire
batch. The early-stop guard can therefore never fire, and every single id
gets attempted no matter how many earlier ones already failed. It gets
worse: `return attempted, errors.Join(errs...)` evaluates `errors.Join(errs...)`
as part of computing the return value, and that evaluation happens BEFORE
the deferred appends run — deferred calls run strictly after the return
expression has already been evaluated. So the returned error is `nil`, even
though every failure genuinely happened; the deferred appends mutate `errs`
only after that `nil` has already been locked in.

`DeleteBatch` fixes both problems the same way: append immediately, with no
`defer` at all, so `len(errs)` is accurate at the top of the next iteration
and `errors.Join(errs...)` sees every failure that happened before the
`return` runs.

Create `softdelete.go`:

```go
package softdelete

import "errors"

// DeleteFunc soft-deletes one item, returning an error on failure.
type DeleteFunc func(id string) error

// BuggyDeleteBatch is meant to give up after maxErrors failures, so one bad
// batch cannot hammer a failing downstream forever. It checks len(errs)
// against maxErrors at the top of the loop -- but every failure's append to
// errs is wrapped in `defer`, so NONE of those appends actually run until
// BuggyDeleteBatch itself returns. That breaks two things at once: the
// len(errs) check inside the loop can never see an earlier iteration's
// failure, so the early-stop guard never fires and every single id is
// attempted; and `return attempted, errors.Join(errs...)` evaluates
// errors.Join over whatever errs holds AT THAT INSTANT -- still empty,
// because the deferred appends have not run yet -- so the returned error is
// nil even though every failure did happen. The deferred appends only
// mutate errs AFTER that return value has already been computed, which is
// too late to matter.
func BuggyDeleteBatch(ids []string, del DeleteFunc, maxErrors int) (attempted []string, err error) {
	var errs []error
	for _, id := range ids {
		if len(errs) >= maxErrors {
			break // BUG: errs is always empty here; this can never trigger
		}
		attempted = append(attempted, id)
		e := del(id)
		defer func() {
			if e != nil {
				errs = append(errs, e)
			}
		}()
	}
	return attempted, errors.Join(errs...)
}

// DeleteBatch gives up after maxErrors failures, appending each failure to
// errs immediately instead of deferring it, so the len(errs) guard at the
// top of the loop actually sees every earlier iteration's failures.
func DeleteBatch(ids []string, del DeleteFunc, maxErrors int) (attempted []string, err error) {
	var errs []error
	for _, id := range ids {
		if len(errs) >= maxErrors {
			break
		}
		attempted = append(attempted, id)
		if e := del(id); e != nil {
			errs = append(errs, e)
		}
	}
	return attempted, errors.Join(errs...)
}
```

### The runnable demo

The demo deletes five ids where the first three fail, with `maxErrors=2`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/softdelete"
)

func main() {
	ids := []string{"a", "b", "c", "d", "e"}
	failing := map[string]bool{"a": true, "b": true, "c": true}

	del := func(id string) error {
		if failing[id] {
			return fmt.Errorf("delete %s: downstream unavailable", id)
		}
		return nil
	}

	attempted, err := softdelete.BuggyDeleteBatch(ids, del, 2)
	fmt.Println("buggy  attempted:", attempted)
	fmt.Println("buggy  error:", err)

	attempted, err = softdelete.DeleteBatch(ids, del, 2)
	fmt.Println("fixed  attempted:", attempted)
	fmt.Println("fixed  error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy  attempted: [a b c d e]
buggy  error: <nil>
fixed  attempted: [a b]
fixed  error: delete a: downstream unavailable
delete b: downstream unavailable
```

### Tests

`TestDeleteBatchStopsEarlyOnceMaxErrorsReached` asserts `DeleteBatch` stops
after 2 failures and returns a non-nil joined error. `TestBuggyDeleteBatch
NeverStopsEarly` asserts `BuggyDeleteBatch` attempts every id AND returns a
`nil` error despite three real failures. `TestDeleteBatchAllSucceedEdgeCase`
and `TestDeleteBatchMaxErrorsZeroEdgeCase` cover the boundaries of no
failures at all and a guard so strict it stops before the first attempt.

Create `softdelete_test.go`:

```go
package softdelete

import (
	"fmt"
	"testing"
)

func failingDel(failing map[string]bool) DeleteFunc {
	return func(id string) error {
		if failing[id] {
			return fmt.Errorf("delete %s: downstream unavailable", id)
		}
		return nil
	}
}

func TestDeleteBatchStopsEarlyOnceMaxErrorsReached(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e"}
	del := failingDel(map[string]bool{"a": true, "b": true, "c": true})

	attempted, err := DeleteBatch(ids, del, 2)

	want := []string{"a", "b"}
	if len(attempted) != len(want) {
		t.Fatalf("attempted = %v, want %v (should stop after 2 failures)", attempted, want)
	}
	for i := range want {
		if attempted[i] != want[i] {
			t.Fatalf("attempted = %v, want %v", attempted, want)
		}
	}
	if err == nil {
		t.Fatal("err = nil, want joined errors")
	}
}

func TestBuggyDeleteBatchNeverStopsEarly(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e"}
	del := failingDel(map[string]bool{"a": true, "b": true, "c": true})

	attempted, err := BuggyDeleteBatch(ids, del, 2)

	if len(attempted) != len(ids) {
		t.Fatalf("attempted = %v (len %d), want all %d ids (deferred appends hide failures from the len(errs) guard)", attempted, len(attempted), len(ids))
	}
	if err != nil {
		t.Fatalf("err = %v, want nil (errors.Join ran before the deferred appends populated errs)", err)
	}
}

func TestDeleteBatchAllSucceedEdgeCase(t *testing.T) {
	ids := []string{"a", "b", "c"}
	del := failingDel(nil)

	attempted, err := DeleteBatch(ids, del, 1)
	if len(attempted) != len(ids) {
		t.Fatalf("attempted = %v, want all %d ids", attempted, len(ids))
	}
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestDeleteBatchMaxErrorsZeroEdgeCase(t *testing.T) {
	ids := []string{"a", "b"}
	del := failingDel(map[string]bool{"a": true})

	attempted, err := DeleteBatch(ids, del, 0)
	if len(attempted) != 0 {
		t.Fatalf("attempted = %v, want empty (maxErrors=0 stops before the first id)", attempted)
	}
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}
```

## Review

A batch delete is correct when it stops attempting further items once too
many have failed, and honestly reports every failure that did happen. The
mechanism to keep straight is that `defer` runs strictly after the return
expression has already been evaluated — deferring the append to an error
slice does not just delay bookkeeping, it means any check or return value
computed from that slice earlier in the same function sees it as
permanently empty. `TestBuggyDeleteBatchNeverStopsEarly`'s `err != nil`
check is the sharper guard here: the early-stop failure is visible (every id
gets attempted), but the silently swallowed error is the more dangerous bug,
because nothing about the buggy function's return value hints that anything
went wrong at all.

## Resources

- [Go blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — when deferred calls actually run relative to a function's return.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — aggregating multiple errors into one that `errors.Is` can match.
- [Effective Go: defer](https://go.dev/doc/effective_go#defer) — defer timing and LIFO ordering.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-sentinel-bootstrap-value-goroutine-fan-capture.md](30-sentinel-bootstrap-value-goroutine-fan-capture.md) | Next: [32-dns-cache-entry-pointer-capture-mutation-race.md](32-dns-cache-entry-pointer-capture-mutation-race.md)
