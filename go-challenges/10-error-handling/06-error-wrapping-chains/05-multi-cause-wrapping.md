# Exercise 5: Multiple %w: Wrap Both the Operation Error and the Rollback Error

When a transaction's work fails, you roll back. But what if the rollback *also*
fails? Naive cleanup code returns one error and silently drops the other, leaving an
operator blind to half of what went wrong — often the half that means data is now
inconsistent. Go 1.20's multiple-`%w` lets a single `fmt.Errorf` wrap *both* causes
so neither is lost and each is independently detectable with `errors.Is`. This
module builds that pattern.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
txn/                        independent module: example.com/txn
  go.mod                    go 1.24
  txn.go                    ErrTxFailed, ErrRollbackFailed; Execute with multiple %w
  cmd/
    demo/
      main.go               runnable demo: rollback-ok vs rollback-also-failed
  txn_test.go               both sentinels found on double failure; only one otherwise
```

Files: `txn.go`, `cmd/demo/main.go`, `txn_test.go`.
Implement: `Execute(work, rollback func() error)` that, on work failure, rolls back and — if rollback also fails — returns one error wrapping both causes with two `%w` verbs.
Test: force work failure with a failing rollback and assert both sentinels are found via `errors.Is` on the single returned error; contrast with a succeeding rollback (only the primary sentinel); assert the message contains both cause strings.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/txn/cmd/demo
cd ~/go-exercises/txn
go mod init example.com/txn
go mod edit -go=1.24
```

### Two causes, one error, neither lost

The cleanup path has two things that can fail independently: the original work and
the rollback. They are *peers* — the rollback failure did not cause the work
failure — so this is exactly the case for wrapping multiple causes. Before Go 1.20
you had to choose one to return (usually the work error) and mention the other only
in the message text, where `errors.Is` could not reach it. Since 1.20 a single
`fmt.Errorf` may contain several `%w` verbs; the result implements `Unwrap()
[]error` and `errors.Is` finds *any* wrapped cause.

`Execute` runs `work`. On success it returns `nil`. On failure it wraps the work
error with the `ErrTxFailed` sentinel, then rolls back. If the rollback succeeds,
only the work error is returned. If the rollback *also* fails, it wraps that with
`ErrRollbackFailed` and returns a single error carrying both:

```
fmt.Errorf("%w; additionally %w", opErr, rbErr)
```

Now `errors.Is(err, ErrTxFailed)` and `errors.Is(err, ErrRollbackFailed)` are
*both* true on the one returned value, and the message contains both underlying
reasons so an operator reading a log line sees the whole picture. (`errors.Join(opErr,
rbErr)` would give the same `errors.Is` reachability; multiple-`%w` is preferred
when you also want a single custom sentence joining them, as here.)

A detail worth noting: each cause is itself a small chain. `opErr` is
`fmt.Errorf("%w: %v", ErrTxFailed, workErr)` — sentinel wrapped with `%w` so it is
findable, the raw work error appended with `%v` for the message. Same for the
rollback side. The outer multiple-`%w` then joins those two chains into a tree.

Create `txn.go`:

```go
package txn

import (
	"errors"
	"fmt"
)

// ErrTxFailed marks the primary operation failure; ErrRollbackFailed marks a
// failed cleanup. Both can be present at once on a single returned error.
var (
	ErrTxFailed       = errors.New("transaction failed")
	ErrRollbackFailed = errors.New("rollback failed")
)

// Execute runs work inside a transaction. If work fails it rolls back; if the
// rollback ALSO fails, both causes are wrapped in one error so neither is lost.
func Execute(work func() error, rollback func() error) error {
	workErr := work()
	if workErr == nil {
		return nil
	}
	opErr := fmt.Errorf("%w: %v", ErrTxFailed, workErr)

	if rbErr := rollback(); rbErr != nil {
		rollbackErr := fmt.Errorf("%w: %v", ErrRollbackFailed, rbErr)
		return fmt.Errorf("%w; additionally %w", opErr, rollbackErr)
	}
	return opErr
}
```

### The runnable demo

The demo runs two scenarios: work fails but rollback recovers (one cause), and work
fails and rollback fails too (both causes). It prints which sentinels each returned
error matches.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/txn"
)

func main() {
	failWork := func() error { return errors.New("deadlock detected") }
	failRollback := func() error { return errors.New("connection lost") }
	okRollback := func() error { return nil }

	err1 := txn.Execute(failWork, okRollback)
	fmt.Println("scenario 1 (rollback ok):")
	fmt.Printf("  tx failed:       %v\n", errors.Is(err1, txn.ErrTxFailed))
	fmt.Printf("  rollback failed: %v\n", errors.Is(err1, txn.ErrRollbackFailed))

	err2 := txn.Execute(failWork, failRollback)
	fmt.Println("scenario 2 (rollback also failed):")
	fmt.Printf("  tx failed:       %v\n", errors.Is(err2, txn.ErrTxFailed))
	fmt.Printf("  rollback failed: %v\n", errors.Is(err2, txn.ErrRollbackFailed))
	fmt.Printf("  message: %v\n", err2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
scenario 1 (rollback ok):
  tx failed:       true
  rollback failed: false
scenario 2 (rollback also failed):
  tx failed:       true
  rollback failed: true
  message: transaction failed: deadlock detected; additionally rollback failed: connection lost
```

### Tests

The test drives the two branches and the success path. The double-failure case is
the point: it asserts *both* sentinels are found via `errors.Is` on the one returned
error, and that the message contains both cause strings so nothing is dropped. The
single-failure case asserts only `ErrTxFailed` is present, proving the rollback
sentinel does not appear when the rollback succeeded.

Create `txn_test.go`:

```go
package txn

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestExecute(t *testing.T) {
	t.Parallel()

	deadlock := func() error { return errors.New("deadlock detected") }
	connLost := func() error { return errors.New("connection lost") }
	ok := func() error { return nil }

	tests := []struct {
		name             string
		work             func() error
		rollback         func() error
		wantErr          bool
		wantTxFailed     bool
		wantRollbackFail bool
		wantMsgHas       []string
	}{
		{
			name:     "work succeeds",
			work:     ok,
			rollback: ok,
			wantErr:  false,
		},
		{
			name:         "work fails, rollback recovers",
			work:         deadlock,
			rollback:     ok,
			wantErr:      true,
			wantTxFailed: true,
			wantMsgHas:   []string{"deadlock detected"},
		},
		{
			name:             "work fails, rollback also fails",
			work:             deadlock,
			rollback:         connLost,
			wantErr:          true,
			wantTxFailed:     true,
			wantRollbackFail: true,
			wantMsgHas:       []string{"deadlock detected", "connection lost"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Execute(tt.work, tt.rollback)

			if tt.wantErr == (err == nil) {
				t.Fatalf("Execute error = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			if got := errors.Is(err, ErrTxFailed); got != tt.wantTxFailed {
				t.Errorf("errors.Is(err, ErrTxFailed) = %v, want %v", got, tt.wantTxFailed)
			}
			if got := errors.Is(err, ErrRollbackFailed); got != tt.wantRollbackFail {
				t.Errorf("errors.Is(err, ErrRollbackFailed) = %v, want %v", got, tt.wantRollbackFail)
			}
			for _, sub := range tt.wantMsgHas {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("message %q missing %q", err.Error(), sub)
				}
			}
		})
	}
}

func ExampleExecute() {
	err := Execute(
		func() error { return errors.New("boom") },
		func() error { return errors.New("cleanup boom") },
	)
	fmt.Println(errors.Is(err, ErrTxFailed), errors.Is(err, ErrRollbackFailed))
	// Output: true true
}
```

## Review

`Execute` is correct when the double-failure path loses nothing: both `ErrTxFailed`
and `ErrRollbackFailed` are `errors.Is`-reachable on the single returned error, and
both underlying reasons appear in the message. The contrast case pins the other
direction — a successful rollback must not make `ErrRollbackFailed` appear, or the
classifier upstream would think cleanup failed when it did not. The mistake this
guards against is the pre-1.20 habit of returning only the work error and stuffing
the rollback reason into a `%v`, where `errors.Is` can never find it; multiple-`%w`
keeps both causes first-class. `errors.Join` would serve equally for reachability if
you do not need the custom joining sentence.

## Resources

- [fmt.Errorf multiple %w](https://pkg.go.dev/fmt#Errorf) — wrapping several errors in one call.
- [Go 1.20 release notes: multiple %w](https://go.dev/doc/go1.20#errors) — the feature and `Unwrap() []error`.
- [errors.Is](https://pkg.go.dev/errors#Is) — traversal over a multi-cause tree.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-retry-classify-retryable.md](06-retry-classify-retryable.md)
