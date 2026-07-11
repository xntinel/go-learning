# Exercise 27: Connection Pool Checkout: Defer Release Inside Checkout Loop

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde).

A batch processor checks out one connection per job from a fixed-capacity
pool. The obvious `defer pool.Release(conn)` right after checkout, inside the
same loop that processes every job, schedules every release to run only
when the whole batch function returns — not when that job finishes. Once the
pool's capacity is exhausted, every remaining job fails to check out a
connection at all, even though each job only needed one for an instant.

## What you'll build

```text
connpool/                    independent module: example.com/connpool
  go.mod                     go 1.24
  connpool.go                Pool, Conn, BuggyProcessAll, ProcessAll
  cmd/
    demo/
      main.go                runnable demo: process 5 jobs against a 2-conn pool
  connpool_test.go            table test: starvation vs no starvation; edge cases
```

- Files: `connpool.go`, `cmd/demo/main.go`, `connpool_test.go`.
- Implement: `Pool.TryCheckout`/`Release`; `BuggyProcessAll` (defer release inside the batch loop) and `ProcessAll` (checkout+release scoped to a per-job helper).
- Test: a table of pool capacities and job counts asserting `ProcessAll` never starves while `BuggyProcessAll` fails every job past capacity; cover an empty job list and a zero-capacity pool.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/connpool/cmd/demo
cd ~/go-exercises/connpool
go mod init example.com/connpool
go mod edit -go=1.24
```

### Why deferring the release inside the batch loop starves later jobs

`defer` schedules a call to run when the *enclosing function* returns, not
when the loop iteration ends. `BuggyProcessAll` checks out a connection for
job 1 and defers its release — that release does not run until
`BuggyProcessAll` itself returns, long after job 1 is done with it. Job 2
checks out its own connection while job 1's is still held, and so does job
3, and so on: with a pool of capacity 2, jobs 1 and 2 succeed and every job
from job 3 onward finds the pool exhausted and fails outright. The fix is
not more capacity — it is scoping each job's checkout-then-release into a
helper function whose own `return` triggers that job's `defer` immediately,
so the connection is back in the pool before the next job even asks for one.

Create `connpool.go`:

```go
package connpool

import (
	"errors"
	"fmt"
	"sync"
)

// Conn is a checked-out handle from a Pool.
type Conn struct {
	ID int
}

// Pool is a fixed-capacity connection pool.
type Pool struct {
	mu   sync.Mutex
	free int
	next int
}

// New returns a Pool with the given capacity, all connections free.
func New(capacity int) *Pool {
	return &Pool{free: capacity}
}

// TryCheckout returns a Conn and true if one is available, or (nil, false)
// if the pool is currently exhausted. It never blocks.
func (p *Pool) TryCheckout() (*Conn, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.free <= 0 {
		return nil, false
	}
	p.free--
	p.next++
	return &Conn{ID: p.next}, true
}

// Release returns a Conn to the pool.
func (p *Pool) Release(c *Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.free++
}

// BuggyProcessAll checks out one connection per job and defers its release,
// but the defer is inside the SAME loop that runs every job, so `defer`
// schedules the Release to run only when BuggyProcessAll itself returns --
// not when that job finishes. Every connection checked out for job 1 is
// still held while job 2, job 3, ... check out theirs, so once the pool's
// capacity is exhausted every remaining job fails with "pool exhausted",
// even though each job only needed its connection for an instant.
func BuggyProcessAll(p *Pool, jobs []string) (acquired []string, err error) {
	var errs []error
	for _, job := range jobs {
		conn, ok := p.TryCheckout()
		if !ok {
			errs = append(errs, fmt.Errorf("job %s: pool exhausted", job))
			continue
		}
		defer p.Release(conn) // BUG: queued until BuggyProcessAll returns, not per job
		acquired = append(acquired, job)
	}
	return acquired, errors.Join(errs...)
}

// ProcessAll checks out and releases each job's connection inside a
// per-job helper, so the release happens as soon as that job finishes and
// before the next job even tries to check one out. No job starves another.
func ProcessAll(p *Pool, jobs []string) (acquired []string, err error) {
	var errs []error
	for _, job := range jobs {
		ok, jobErr := func() (bool, error) {
			conn, ok := p.TryCheckout()
			if !ok {
				return false, fmt.Errorf("job %s: pool exhausted", job)
			}
			defer p.Release(conn)
			return true, nil
		}()
		if jobErr != nil {
			errs = append(errs, jobErr)
			continue
		}
		if ok {
			acquired = append(acquired, job)
		}
	}
	return acquired, errors.Join(errs...)
}
```

### The runnable demo

The demo processes five jobs against a 2-connection pool with both variants.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/connpool"
)

func main() {
	jobs := []string{"job-1", "job-2", "job-3", "job-4", "job-5"}

	buggy := connpool.New(2)
	acquired, err := connpool.BuggyProcessAll(buggy, jobs)
	fmt.Println("buggy  acquired:", acquired)
	fmt.Println("buggy  error:", err)

	fixed := connpool.New(2)
	acquired, err = connpool.ProcessAll(fixed, jobs)
	fmt.Println("fixed  acquired:", acquired)
	fmt.Println("fixed  error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy  acquired: [job-1 job-2]
buggy  error: job job-3: pool exhausted
job job-4: pool exhausted
job job-5: pool exhausted
fixed  acquired: [job-1 job-2 job-3 job-4 job-5]
fixed  error: <nil>
```

### Tests

`TestProcessAll` is a table test covering a pool smaller than the job count
(where `ProcessAll` succeeds on every job but `BuggyProcessAll` starves
everything past capacity) and a pool larger than the job count (where
neither variant starves, since capacity was never the binding constraint).
`TestProcessAllEmptyJobsEdgeCase` and `TestProcessAllZeroCapacityEdgeCase`
cover the boundaries of no work and no capacity at all.

Create `connpool_test.go`:

```go
package connpool

import "testing"

func TestProcessAll(t *testing.T) {
	jobs := []string{"job-1", "job-2", "job-3", "job-4", "job-5"}

	tests := []struct {
		name         string
		capacity     int
		process      func(*Pool, []string) ([]string, error)
		wantAcquired int
		wantErr      bool
	}{
		{
			name:         "fixed: scoped checkout releases before the next job",
			capacity:     2,
			process:      ProcessAll,
			wantAcquired: 5,
			wantErr:      false,
		},
		{
			name:         "buggy: deferred release starves jobs after capacity",
			capacity:     2,
			process:      BuggyProcessAll,
			wantAcquired: 2,
			wantErr:      true,
		},
		{
			name:         "fixed: capacity larger than job count never starves",
			capacity:     10,
			process:      ProcessAll,
			wantAcquired: 5,
			wantErr:      false,
		},
		{
			name:         "buggy: capacity larger than job count never starves either",
			capacity:     10,
			process:      BuggyProcessAll,
			wantAcquired: 5,
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(tt.capacity)
			acquired, err := tt.process(p, jobs)
			if len(acquired) != tt.wantAcquired {
				t.Fatalf("acquired = %v (len %d), want len %d", acquired, len(acquired), tt.wantAcquired)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestProcessAllEmptyJobsEdgeCase(t *testing.T) {
	p := New(2)
	acquired, err := ProcessAll(p, nil)
	if len(acquired) != 0 {
		t.Fatalf("acquired = %v, want empty", acquired)
	}
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestProcessAllZeroCapacityEdgeCase(t *testing.T) {
	p := New(0)
	acquired, err := ProcessAll(p, []string{"job-1"})
	if len(acquired) != 0 {
		t.Fatalf("acquired = %v, want empty", acquired)
	}
	if err == nil {
		t.Fatal("err = nil, want pool-exhausted error")
	}
}
```

## Review

Batch processing is correct when it never starves a job just because an
earlier job's connection has not been released yet. The mechanism to keep
straight is that `defer` fires at function return, so deferring a release
inside a loop that runs many jobs holds every job's resource open for the
ENTIRE batch instead of just that job's own turn. `TestProcessAll`'s
capacity-2 case is the starvation guard: it fails the moment `BuggyProcessAll`
starts rejecting jobs it should have been able to serve. The fix generalizes
beyond connection pools to any acquire/release pattern inside a loop — file
handles, locks, semaphores — whenever the resource is meant to be held only
for the duration of one iteration's work.

## Resources

- [Effective Go: defer](https://go.dev/doc/effective_go#defer) — defer timing and why it is scoped to the enclosing function.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — aggregating multiple job failures into one error.
- [Go blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — when deferred calls actually run.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-observability-span-context-value-capture-propagation.md](26-observability-span-context-value-capture-propagation.md) | Next: [28-circuit-breaker-per-service-unguarded-closure-state.md](28-circuit-breaker-per-service-unguarded-closure-state.md)
