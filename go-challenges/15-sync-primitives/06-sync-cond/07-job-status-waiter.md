# Exercise 7: Long-poll job-status waiter with context cancellation

A client kicks off an async import and long-polls `GET /jobs/{id}?wait=done`:
the handler must block until the job reaches the requested status or the
request deadline fires. This module builds the store behind that endpoint and
works the hardest pattern in the chapter end to end: a cancellable `Cond` wait,
with a watcher goroutine that provably never leaks.

## What you'll build

```text
jobwait/                    independent module: example.com/jobwait
  go.mod                    module path example.com/jobwait
  store.go                  type Store: SetStatus, Status, WaitFor(ctx, id, status)
  cmd/
    demo/
      main.go               a worker advances a job; two long-polls succeed, one times out
  store_test.go             park/release, fake-clock deadline, cross-job honesty, -race
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a `Store` where `SetStatus(id, status)` records a transition and Broadcasts, and `WaitFor(ctx, id, status) error` blocks until job `id` reaches `status` or `ctx` is done (returning `ctx.Err()`).
- Test: a waiter parks until the matching `SetStatus` and ignores non-matching ones; a `ctx` timeout returns `context.DeadlineExceeded` on `synctest`'s fake clock with zero real sleeping; waiters on different jobs stay honest across cross-job Broadcasts; the watcher goroutine never leaks on the happy path.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/jobwait/cmd/demo
cd ~/go-exercises/jobwait
go mod init example.com/jobwait
```

### Heterogeneous predicates force Broadcast

Every waiter here waits on its OWN predicate — `jobs["import-7"] == "done"`,
`jobs["export-3"] == "running"` — over one shared map guarded by one mutex.
This is the textbook case where `Signal` is a correctness bug, not an
optimization choice: a `SetStatus("import-7", "done")` that signalled could
wake the `export-3` waiter, whose predicate is still false, and it would
re-park while the `import-7` waiter — the one the update was for — sleeps
forever. `Broadcast` wakes everyone; each waiter re-checks its own predicate
in its `for` loop and only proceeds if its job reached its status. The
cross-job wakeups are wasted CPU proportional to the number of concurrent
long-polls, which is fine at handler scale; if you had tens of thousands of
waiters you would shard to per-job Conds — same pattern, more bookkeeping.

### The cancellable wait, dissected

`Cond.Wait` cannot observe `ctx.Done()`, so `WaitFor` builds cancellation from
three cooperating pieces, each load-bearing:

1. The watcher goroutine `select`s on `ctx.Done()` and, when it fires, takes
   the mutex and Broadcasts. Taking the mutex first is what makes this
   race-free: the waiter holds the lock from its predicate check until `Wait`
   atomically releases it, so there is no instant where the Broadcast can land
   between "decided to sleep" and "asleep".
2. The predicate loop re-checks `ctx.Err()` BEFORE each `Wait`. The Broadcast
   only wakes the waiter; it is the `ctx.Err()` check that turns the wakeup
   into a `context.DeadlineExceeded` return. Forget it and the cancelled
   waiter re-parks forever.
3. The `done` channel, closed by `defer` when `WaitFor` returns, gives the
   watcher its happy-path exit. Without it, every successful long-poll leaks a
   goroutine that survives until the HTTP request's context is cancelled —
   under load, that is thousands of parked goroutines held by nothing but a
   forgotten `select`. `synctest.Test` fails a bubble that ends with a live
   goroutine, so the happy-path test doubles as a leak detector.

One fast path matters for HTTP semantics: if the job is ALREADY at the target
status, `WaitFor` returns nil without spawning the watcher at all — a
long-poll for a finished job is a plain read.

Create `store.go`:

```go
package jobwait

import (
	"context"
	"sync"
)

// Store tracks job statuses and lets callers block until a job reaches a
// requested status.
type Store struct {
	mu   sync.Mutex
	cond *sync.Cond
	jobs map[string]string
}

// New returns an empty Store.
func New() *Store {
	s := &Store{jobs: make(map[string]string)}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// SetStatus records the current status of job id and wakes every waiter.
// Broadcast is required: waiters hold heterogeneous per-job predicates.
func (s *Store) SetStatus(id, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[id] = status
	s.cond.Broadcast()
}

// Status reports the last recorded status of job id.
func (s *Store) Status(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.jobs[id]
	return st, ok
}

// WaitFor blocks until job id reaches status or ctx is done, in which case it
// returns ctx.Err(). Unknown jobs simply wait: the job may be registered
// after the long-poll arrives.
func (s *Store) WaitFor(ctx context.Context, id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs[id] == status {
		return nil // fast path: no watcher needed
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cond.Broadcast() // kick the waiter so it re-checks ctx.Err
			s.mu.Unlock()
		case <-done: // WaitFor returned; exit without leaking
		}
	}()

	for s.jobs[id] != status {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.cond.Wait()
	}
	return nil
}
```

### The runnable demo

A worker goroutine advances one job through `running` to `done` on timers; the
main goroutine long-polls each transition, then long-polls a status the job
never reaches with a short deadline.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/jobwait"
)

func main() {
	s := jobwait.New()

	go func() { // the async worker
		time.Sleep(20 * time.Millisecond)
		s.SetStatus("import-7", "running")
		time.Sleep(20 * time.Millisecond)
		s.SetStatus("import-7", "done")
	}()

	for _, want := range []string{"running", "done"} {
		if err := s.WaitFor(context.Background(), "import-7", want); err == nil {
			fmt.Printf("job import-7 reached %q\n", want)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := s.WaitFor(ctx, "import-7", "archived")
	fmt.Println("long-poll for \"archived\" gave up:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
job import-7 reached "running"
job import-7 reached "done"
long-poll for "archived" gave up: context deadline exceeded
```

### Tests

`TestWaiterParksUntilMatch` proves the waiter survives a NON-matching update
(wakes, re-checks, re-parks) and returns only on the matching one.
`TestDeadline` runs a 5-second timeout instantly on the fake clock.
`TestCrossJobHonesty` pins the Broadcast contract with two waiters on
different jobs. `TestStress` races setters against waiters under `-race`.

Create `store_test.go`:

```go
package jobwait

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestWaiterParksUntilMatch(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := New()
		got := make(chan error, 1)
		go func() { got <- s.WaitFor(context.Background(), "job-1", "done") }()

		synctest.Wait() // waiter durably parked

		// A non-matching update wakes the waiter; it must re-park.
		s.SetStatus("job-1", "running")
		synctest.Wait()
		select {
		case <-got:
			t.Fatal("WaitFor returned on a non-matching status")
		default:
		}

		s.SetStatus("job-1", "done")
		synctest.Wait()
		select {
		case err := <-got:
			if err != nil {
				t.Fatalf("WaitFor = %v, want nil", err)
			}
		default:
			t.Fatal("WaitFor did not return after the matching SetStatus")
		}
	})
}

func TestAlreadyAtStatus(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := New()
		s.SetStatus("job-1", "done")
		if err := s.WaitFor(t.Context(), "job-1", "done"); err != nil {
			t.Fatalf("WaitFor on satisfied predicate = %v, want nil", err)
		}
	})
}

func TestDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := New()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		start := time.Now()
		err := s.WaitFor(ctx, "job-1", "done")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitFor = %v, want context.DeadlineExceeded", err)
		}
		if d := time.Since(start); d != 5*time.Second {
			t.Fatalf("gave up after %v of fake time, want exactly 5s", d)
		}
	})
}

func TestCrossJobHonesty(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := New()
		gotA := make(chan error, 1)
		gotB := make(chan error, 1)
		go func() { gotA <- s.WaitFor(context.Background(), "job-a", "done") }()
		go func() { gotB <- s.WaitFor(context.Background(), "job-b", "done") }()
		synctest.Wait()

		s.SetStatus("job-a", "done") // Broadcast wakes BOTH; only A may return
		synctest.Wait()
		select {
		case err := <-gotA:
			if err != nil {
				t.Fatalf("waiter A = %v, want nil", err)
			}
		default:
			t.Fatal("waiter A not released by its own status")
		}
		select {
		case <-gotB:
			t.Fatal("waiter B released by job-a's update")
		default:
		}

		s.SetStatus("job-b", "done")
		synctest.Wait()
		if err := <-gotB; err != nil {
			t.Fatalf("waiter B = %v, want nil", err)
		}
	})
}

func TestStress(t *testing.T) {
	t.Parallel()

	s := New()
	const jobs = 50
	var wg sync.WaitGroup
	for i := range jobs {
		id := fmt.Sprintf("job-%d", i)
		wg.Go(func() {
			if err := s.WaitFor(context.Background(), id, "done"); err != nil {
				t.Errorf("WaitFor(%s) = %v", id, err)
			}
		})
		wg.Go(func() {
			s.SetStatus(id, "running")
			s.SetStatus(id, "done")
		})
	}
	wg.Wait()
}

func Example() {
	s := New()
	s.SetStatus("job-1", "done")
	if err := s.WaitFor(context.Background(), "job-1", "done"); err == nil {
		fmt.Println("job-1 is done")
	}
	// Output: job-1 is done
}
```

## Review

The contract to verify is three-sided: a waiter returns exactly when ITS job
reaches ITS status, a deadline converts to `ctx.Err()` and nothing else, and no
goroutine outlives the call that spawned it. The tests map one-to-one:
`TestCrossJobHonesty` would catch a broken predicate (a waiter trusting the
wakeup instead of re-checking its own job), `TestDeadline` would catch a
missing `ctx.Err()` re-check as a bubble deadlock, and every `synctest.Test`
block implicitly asserts the watcher exited — delete the `done` channel and
the happy-path tests fail with a leaked-goroutine report, which is a far
better signal than a production heap profile six weeks later.

When adapting this to a real handler, resist two temptations. Do not cache
`s.jobs[id]` outside the lock to "save" re-reads — the whole design depends on
the predicate being read under `cond.L`. And do not replace the watcher with a
`time.AfterFunc` that Broadcasts: it works for deadlines but silently ignores
explicit cancellation (client disconnect), which is the more common event for
long-polls.

## Resources

- [`sync.Cond`](https://pkg.go.dev/sync#Cond) — the waiting contract this module extends with cancellation.
- [`context`](https://pkg.go.dev/context) — Done, Err, and deadline semantics.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — fake-clock timeouts and the goroutine-leak check.
- [The Go Memory Model](https://go.dev/ref/mem) — why the watcher must Broadcast under the lock.

---

Back to [06-pausable-worker-pool.md](06-pausable-worker-pool.md) | Next: [08-weighted-semaphore.md](08-weighted-semaphore.md)
