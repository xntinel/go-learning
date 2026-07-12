# Exercise 4: Hard SLA on a downstream dependency call

"Call service B, but never block my handler more than 200ms" is one of the most
common shapes in backend code. This exercise builds `CallWithBudget`: run a slow
operation in a goroutine, hand its result back on a buffered channel, and select
between the result and a timer. It returns `ErrTimeout` on breach without leaking
the in-flight goroutine, and propagates the operation's own error untouched.

## What you'll build

```text
slacall/                      module example.com/sla
  go.mod
  sla.go                      ErrTimeout; CallWithBudget[T]
  cmd/demo/main.go            fast dependency, slow dependency
  sla_test.go                 table over (latency, budget, opErr); no-leak proof; Example
```

Files: `sla.go`, `cmd/demo/main.go`, `sla_test.go`.
Implement: `CallWithBudget[T any](budget time.Duration, op func() (T, error)) (T, error)`.
Test: fast returns value; slow returns `ErrTimeout`; op error propagates (not `ErrTimeout`); goroutine count returns to baseline after timeouts.
Verify: `go test -count=1 -race ./...`

### Result and error on one buffered channel

The operation returns `(T, error)`, so both must travel back from the goroutine.
Bundle them in a small `result[T]` struct and send that struct on a buffered
channel of capacity one. The buffer is the exit door from Exercise 2: when the
budget wins the race and the caller returns `ErrTimeout`, the in-flight goroutine
still finishes `op()`, sends its `result` into the buffer slot, and exits. Without
the buffer it would block on the send forever.

The error contract has two distinct paths that callers must be able to tell apart.
If `op` finishes within budget, `CallWithBudget` returns *exactly* what `op`
returned — value and error — so a downstream 404 or a validation failure
propagates unchanged and is *not* masquerading as a timeout. Only when the timer
wins does it return the package's `ErrTimeout`. A caller therefore writes
`errors.Is(err, ErrTimeout)` to branch on "the dependency was too slow" versus
"the dependency answered, with this error." Conflating the two — for instance
wrapping every failure as a timeout — would make a fast, deterministic downstream
error look like a flaky latency problem and send an on-call engineer chasing the
wrong thing.

Create `sla.go`:

```go
package sla

import (
	"errors"
	"time"
)

// ErrTimeout is returned when a downstream call exceeds its budget.
var ErrTimeout = errors.New("sla: downstream call exceeded budget")

type result[T any] struct {
	val T
	err error
}

// CallWithBudget runs op and returns its result if it finishes within budget.
// On breach it returns the zero value and ErrTimeout; the op goroutine still
// completes into the buffered channel and exits, so it does not leak. An error
// returned by op within budget is propagated unchanged, never as ErrTimeout.
func CallWithBudget[T any](budget time.Duration, op func() (T, error)) (T, error) {
	var zero T
	done := make(chan result[T], 1) // buffer of 1: the loser of the race can still send
	go func() {
		v, err := op()
		done <- result[T]{val: v, err: err}
	}()
	select {
	case r := <-done:
		return r.val, r.err
	case <-time.After(budget):
		return zero, ErrTimeout
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/sla"
)

func main() {
	v, err := sla.CallWithBudget(200*time.Millisecond, func() (string, error) {
		time.Sleep(20 * time.Millisecond)
		return "pong", nil
	})
	fmt.Printf("fast: v=%q err=%v\n", v, err)

	_, err = sla.CallWithBudget(50*time.Millisecond, func() (string, error) {
		time.Sleep(300 * time.Millisecond)
		return "pong", nil
	})
	fmt.Printf("slow: timedOut=%v\n", errors.Is(err, sla.ErrTimeout))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast: v="pong" err=<nil>
slow: timedOut=true
```

### Tests

`TestCallWithBudget` is table-driven over `(latency, budget, opErr)`: a fast op
returns its value; a slow op returns `ErrTimeout`; an op that errors quickly has
its error propagated and is asserted *not* to be `ErrTimeout`. `TestNoLeak`
records the baseline goroutine count, times out twenty slow calls, and polls until
the count returns to baseline — proving the buffered channel let every abandoned
worker finish and exit. It is not `t.Parallel()` because it reads a global counter.

Create `sla_test.go`:

```go
package sla

import (
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

func TestCallWithBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		latency time.Duration
		budget  time.Duration
		opErr   error
		wantVal int
		wantErr error
	}{
		{"fast success", 5 * time.Millisecond, 200 * time.Millisecond, nil, 42, nil},
		{"slow times out", 200 * time.Millisecond, 20 * time.Millisecond, nil, 0, ErrTimeout},
		{"op error propagates", 5 * time.Millisecond, 200 * time.Millisecond, errBoom, 0, errBoom},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			val, err := CallWithBudget(tc.budget, func() (int, error) {
				time.Sleep(tc.latency)
				if tc.opErr != nil {
					return 0, tc.opErr
				}
				return 42, nil
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if val != tc.wantVal {
				t.Fatalf("val = %d, want %d", val, tc.wantVal)
			}
		})
	}
}

func TestOpErrorIsNotTimeout(t *testing.T) {
	t.Parallel()
	_, err := CallWithBudget(time.Second, func() (int, error) {
		return 0, errBoom
	})
	if errors.Is(err, ErrTimeout) {
		t.Fatalf("op error was reported as ErrTimeout: %v", err)
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestNoLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	for range 20 {
		_, err := CallWithBudget(10*time.Millisecond, func() (int, error) {
			time.Sleep(50 * time.Millisecond)
			return 1, nil
		})
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("err = %v, want ErrTimeout", err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if extra := runtime.NumGoroutine() - base; extra > 2 {
		t.Fatalf("workers leaked: %d extra goroutines", extra)
	}
}

func ExampleCallWithBudget() {
	v, err := CallWithBudget(time.Second, func() (int, error) {
		return 7, nil
	})
	fmt.Println(v, err)
	// Output: 7 <nil>
}
```

## Review

The guard is correct when its error precisely distinguishes the two failure modes:
the timer winning yields `ErrTimeout`, while `op` answering yields exactly `op`'s
own result and error. The most damaging mistake here is collapsing those into one
error type, which turns a deterministic downstream failure into a phantom latency
problem. The second is the unbuffered-channel leak from Exercise 2 — under an SLA
guard that fires often, an unbuffered `done` channel leaks one goroutine per
breach; `TestNoLeak` is the guard against regressing that. Run `go test -race` to
confirm the abandoned workers finish without racing on the discarded result.

## Resources

- [`time.After`](https://pkg.go.dev/time#After) — the budget timer in the select.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — distinguishing `ErrTimeout` from a propagated op error.
- [Go blog: Go Concurrency Patterns — Timeout](https://go.dev/blog/concurrency-timeouts) — the canonical goroutine-plus-timer shape.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-reset-timer-idle-consumer.md](03-reset-timer-idle-consumer.md) | Next: [05-retry-backoff-total-budget.md](05-retry-backoff-total-budget.md)
