# Exercise 8: Weighted semaphore for memory-budgeted request admission

Report exports do not cost one unit each: a 50-row CSV needs a few megabytes,
a full-history export needs hundreds. Admitting requests by COUNT lets three
big exports OOM the pod while admitting by WEIGHT keeps the sum under a memory
budget. This module builds a weighted semaphore in the shape of
`golang.org/x/sync/semaphore` — and heterogeneous weights are exactly the
situation where `Signal` stops being an optimization question and becomes a
starvation bug you can write a failing test for.

## What you'll build

```text
wsem/                       independent module: example.com/wsem
  go.mod                    module path example.com/wsem
  sem.go                    type Sem: New(budget), Acquire(n), Release(n), Free; ErrOverBudget
  cmd/
    demo/
      main.go               a 384 MB export holds the budget; a 256 MB one waits its turn
  sem_test.go               small-waiter-behind-big-waiter release, over-budget reject, -race
```

- Files: `sem.go`, `cmd/demo/main.go`, `sem_test.go`.
- Implement: `New(budget)` and a blocking `Acquire(n) error` that parks until `n` permits are free, `Release(n)` that returns them and Broadcasts, `Free()` for observability; `Acquire(n > budget)` is rejected up front with a wrapped `ErrOverBudget` because it could never succeed.
- Test: with budget 10 and 9 outstanding, parked `Acquire(8)` and `Acquire(2)` behave correctly when 3 permits free — the 2 is admitted, the 8 re-parks; outstanding weight never exceeds the budget under `-race`; over-budget requests fail via `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/wsem/cmd/demo
cd ~/go-exercises/wsem
go mod init example.com/wsem
```

### Why heterogeneous weights make Signal a lost-wakeup bug

With a counting semaphore (every waiter needs 1), `Signal` per released permit
is fine: any woken waiter can proceed. Weights break that symmetry. Suppose
budget 10, 9 held, an `Acquire(8)` and an `Acquire(2)` both parked. `Release(3)`
frees 4 permits — enough for the 2, not the 8. A `Signal` implementation wakes
ONE waiter, and the runtime gives no fairness guarantee about which: if it
picks the 8, that waiter re-checks `free < 8`, re-parks, and nobody wakes the
2 — the freed capacity sits idle while an admissible request starves behind an
inadmissible one. That is a lost wakeup manufactured purely by the wakeup
policy; the state was correct the whole time. `Release` therefore Broadcasts:
every waiter re-evaluates its own `n` against the freed budget, those that fit
proceed, the rest re-park. The cost is O(waiters) spurious wakeups per
release, which is the standard price of heterogeneous predicates on one Cond.

Note what this design deliberately does NOT provide: FIFO fairness. A stream
of small acquisitions can starve a large one indefinitely, because the large
waiter only wins when the budget happens to drain low enough. The production
`x/sync/semaphore` solves this with an explicit waiter queue and by waking
waiters in order — the ordering lives in the data structure, never in
`Cond` wakeup order, exactly as the concepts file prescribes. Here the simple
policy is kept and documented, which is acceptable when large requests are
rare; the exercise's point is the admission arithmetic and the Broadcast.

Two smaller decisions worth reading closely: `Acquire(n > budget)` returns a
sentinel error immediately instead of deadlocking forever (a caller bug should
surface as an error, not a stuck goroutine), and `Release` of more than is
held panics — like `sync.WaitGroup`'s negative counter, it is unrecoverable
API misuse, not a runtime condition to handle.

Create `sem.go`:

```go
package wsem

import (
	"errors"
	"fmt"
	"sync"
)

// ErrOverBudget is returned by Acquire when the requested weight exceeds the
// semaphore's total budget and could never be satisfied.
var ErrOverBudget = errors.New("weight exceeds semaphore budget")

// Sem is a weighted semaphore: Acquire(n) admits a request of weight n only
// when the outstanding total stays within the budget.
type Sem struct {
	mu     sync.Mutex
	cond   *sync.Cond
	budget int
	used   int
}

// New returns a semaphore with the given total budget (minimum 1).
func New(budget int) *Sem {
	if budget < 1 {
		budget = 1
	}
	s := &Sem{budget: budget}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Acquire blocks until n permits are free, then takes them. A weight larger
// than the whole budget fails immediately with a wrapped ErrOverBudget; a
// non-positive weight is API misuse and panics.
func (s *Sem) Acquire(n int) error {
	if n <= 0 {
		panic("wsem: Acquire with non-positive weight")
	}
	if n > s.budget { // budget is immutable after New; safe to read unlocked
		return fmt.Errorf("acquire %d of budget %d: %w", n, s.budget, ErrOverBudget)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.budget-s.used < n {
		s.cond.Wait()
	}
	s.used += n
	return nil
}

// Release returns n permits and wakes every waiter so each re-checks its own
// weight against the freed budget. Broadcast is required: waiters are
// heterogeneous, and Signal could wake only one that still does not fit.
func (s *Sem) Release(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || n > s.used {
		panic("wsem: Release of more than is held")
	}
	s.used -= n
	s.cond.Broadcast()
}

// Free reports the currently unused budget.
func (s *Sem) Free() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.budget - s.used
}
```

### The runnable demo

A 384 MB export is admitted against a 512 MB budget; a 256 MB export must
wait (the demo proves it waits with a timeout probe, since 128 MB free < 256),
and is admitted the moment the big one releases.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/wsem"
)

func main() {
	s := wsem.New(512) // 512 MB memory budget for report exports

	if err := s.Acquire(384); err != nil {
		panic(err)
	}
	fmt.Println("384 MB export admitted; free:", s.Free(), "MB")

	admitted := make(chan struct{})
	go func() {
		if err := s.Acquire(256); err != nil {
			panic(err)
		}
		close(admitted)
	}()

	select {
	case <-admitted:
		fmt.Println("impossible: 256 > 128 free")
	case <-time.After(50 * time.Millisecond):
		fmt.Println("256 MB export waiting; free:", s.Free(), "MB")
	}

	s.Release(384)
	<-admitted
	fmt.Println("256 MB export admitted; free:", s.Free(), "MB")
	s.Release(256)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
384 MB export admitted; free: 128 MB
256 MB export waiting; free: 128 MB
256 MB export admitted; free: 256 MB
```

### Tests

`TestSmallWaiterNotStarvedByBigOne` is the scenario from the prose, pinned
deterministically: after `Release(3)`, `synctest.Wait` settles the bubble and
the test asserts the 2 got in while the 8 is still parked — a `Signal`
implementation fails this whenever the runtime picks the 8.
`TestOutstandingNeverExceedsBudget` is the `-race` invariant stress.

Create `sem_test.go`:

```go
package wsem

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestSmallWaiterNotStarvedByBigOne(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := New(10)
		if err := s.Acquire(9); err != nil {
			t.Fatalf("Acquire(9): %v", err)
		}

		got8 := make(chan struct{})
		got2 := make(chan struct{})
		go func() {
			if err := s.Acquire(8); err != nil {
				t.Errorf("Acquire(8): %v", err)
			}
			close(got8)
		}()
		go func() {
			if err := s.Acquire(2); err != nil {
				t.Errorf("Acquire(2): %v", err)
			}
			close(got2)
		}()

		synctest.Wait() // both parked: only 1 permit free
		select {
		case <-got8:
			t.Fatal("Acquire(8) admitted with 1 free")
		case <-got2:
			t.Fatal("Acquire(2) admitted with 1 free")
		default:
		}

		s.Release(3) // 4 free: enough for the 2, not the 8
		synctest.Wait()
		select {
		case <-got2:
		default:
			t.Fatal("2-weight waiter not admitted after Release(3); Signal instead of Broadcast?")
		}
		select {
		case <-got8:
			t.Fatal("8-weight waiter admitted with only 2 free")
		default:
		}

		s.Release(6) // 8 free: now the big one fits
		synctest.Wait()
		select {
		case <-got8:
		default:
			t.Fatal("8-weight waiter not admitted after enough budget freed")
		}

		s.Release(8)
		s.Release(2)
		if got := s.Free(); got != 10 {
			t.Fatalf("Free = %d after all releases, want 10", got)
		}
	})
}

func TestOverBudgetRejectedUpFront(t *testing.T) {
	t.Parallel()

	s := New(10)
	err := s.Acquire(11)
	if !errors.Is(err, ErrOverBudget) {
		t.Fatalf("Acquire(11) = %v, want ErrOverBudget", err)
	}
	if got := s.Free(); got != 10 {
		t.Fatalf("Free = %d after rejected Acquire, want 10", got)
	}
}

func TestOutstandingNeverExceedsBudget(t *testing.T) {
	t.Parallel()

	const budget = 10
	s := New(budget)
	var outstanding atomic.Int64
	var wg sync.WaitGroup
	for i := range 200 {
		weight := i%3 + 1
		wg.Go(func() {
			if err := s.Acquire(weight); err != nil {
				t.Errorf("Acquire(%d): %v", weight, err)
				return
			}
			if o := outstanding.Add(int64(weight)); o > budget {
				t.Errorf("outstanding weight %d exceeds budget %d", o, budget)
			}
			runtime.Gosched()
			outstanding.Add(-int64(weight))
			s.Release(weight)
		})
	}
	wg.Wait()
	if got := s.Free(); got != budget {
		t.Fatalf("Free = %d after all releases, want %d", got, budget)
	}
}

func Example() {
	s := New(10)
	_ = s.Acquire(7)
	fmt.Println("free after admit:", s.Free())
	s.Release(7)
	fmt.Println(errors.Is(s.Acquire(11), ErrOverBudget))
	// Output:
	// free after admit: 3
	// true
}
```

## Review

Correctness here is one inequality — the sum of admitted weights never exceeds
the budget — plus one liveness property: any waiter whose weight currently
fits eventually gets in. The inequality is enforced by the predicate under the
lock and pinned by `TestOutstandingNeverExceedsBudget`, whose `outstanding`
counter is adjusted strictly inside the Acquire/Release window so any
violation it reports is real. The liveness property is entirely a function of
`Broadcast` in `Release`; re-read the failing sequence in the prose until you
can reproduce it from memory, because it is the single most common way
hand-rolled semaphores deadlock in production.

Know when to graduate from this module's design: if you need context-aware
acquisition, FIFO fairness, or `TryAcquire`, use `golang.org/x/sync/semaphore`
instead of growing this one — it implements the waiter queue and cancellation
correctly, and its docs define the same block-until-free semantics you built
here. The reason to have built it by hand is that you now know exactly why its
`Release` wakes waiters the way it does.

## Resources

- [`golang.org/x/sync/semaphore`](https://pkg.go.dev/golang.org/x/sync/semaphore) — the production weighted semaphore this module mirrors.
- [`sync.Cond`](https://pkg.go.dev/sync#Cond) — Broadcast semantics and the no-fairness caveat.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — settling the bubble to assert who was admitted.

---

Back to [07-job-status-waiter.md](07-job-status-waiter.md) | Next: [09-config-version-fanout.md](09-config-version-fanout.md)
