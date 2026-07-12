# Exercise 17: Async Job Queue Backpressure Gate with Captured Counter

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A batch job processor accepts work from many submitters at once but must cap
how many jobs are in flight, or an unbounded burst exhausts downstream
workers. `NewSubmitter` closes over one in-flight counter shared by two
returned functions — `submit` and `complete` — the same paired-closure shape
as the admission gate earlier in this lesson, but here concurrent submitters
are the whole point, so the shared counter is guarded by a mutex and proven
correct under `-race`.

## What you'll build

```text
jobqueue/                  independent module: example.com/job-queue-backpressure
  go.mod                   go 1.24
  jobqueue.go              NewSubmitter returns (submit func() bool, complete func())
  cmd/
    demo/
      main.go               submits past the limit, frees a slot, submits again
  jobqueue_test.go          table test: limit enforced, clamp, isolation, concurrency
```

- Files: `jobqueue.go`, `cmd/demo/main.go`, `jobqueue_test.go`.
- Implement: `NewSubmitter(limit int) (submit func() bool, complete func())`, both closing over one mutex-guarded `inFlight int`.
- Test: a table submits up to the limit and confirms the next call is refused, then checks `complete` frees exactly one slot and cannot drive the counter negative; a second test proves two submitters never share state; a concurrency test fires 500 goroutines at a limit of 20 and asserts exactly 20 are admitted under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Backpressure is a check-then-act under one lock

`submit` and `complete` share exactly one captured `inFlight`, allocated once
per `NewSubmitter` call, the same structural pattern as the acquire/release
gate seen earlier in this lesson. What's different here is the workload: a
job queue accepts submissions from many goroutines concurrently — worker
pools, HTTP handlers, cron triggers — all racing to submit against the same
limit. If the check ("is there room?") and the act ("increment") were two
separate locked operations, two goroutines could both pass the check before
either increments, and both would be admitted past the limit — a classic
check-then-act race. `submit` avoids it by doing the comparison and the
increment inside one `mu.Lock()`/`mu.Unlock()` pair, so only one goroutine at
a time can observe and act on a given counter value.

`complete` clamps at zero for the same reason the admission gate does: a
double-complete (a job's completion handler firing twice after a retry) must
not push the counter negative and manufacture phantom capacity.

Create `jobqueue.go`:

```go
package jobqueue

import "sync"

// NewSubmitter returns a paired submit/complete closure sharing one captured
// in-flight job count, guarded by a mutex. submit reports true and admits a
// job if fewer than limit jobs are currently in flight; once the limit is
// reached it refuses (backpressure) and leaves the counter untouched.
// complete decrements the counter, clamped at zero so a stray extra complete
// cannot manufacture capacity that was never there.
//
// Many goroutines submit jobs concurrently against one shared limit, so the
// check-then-act (read inFlight, compare to limit, increment) must happen
// inside a single critical section; splitting the read from the increment
// would let two submitters both observe capacity and both admit, blowing
// past limit.
func NewSubmitter(limit int) (submit func() bool, complete func()) {
	var mu sync.Mutex
	inFlight := 0

	submit = func() bool {
		mu.Lock()
		defer mu.Unlock()
		if inFlight >= limit {
			return false
		}
		inFlight++
		return true
	}

	complete = func() {
		mu.Lock()
		defer mu.Unlock()
		if inFlight > 0 {
			inFlight--
		}
	}

	return submit, complete
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/job-queue-backpressure"
)

func main() {
	submit, complete := jobqueue.NewSubmitter(2)

	for i := 1; i <= 3; i++ {
		fmt.Printf("submit job %d: %v\n", i, submit())
	}

	complete()
	fmt.Printf("submit job 4 after complete: %v\n", submit())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
submit job 1: true
submit job 2: true
submit job 3: false
submit job 4 after complete: true
```

### Tests

Create `jobqueue_test.go`:

```go
package jobqueue

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestSubmitEnforcesLimitAndCompleteFreesASlot(t *testing.T) {
	submit, complete := NewSubmitter(2)

	tests := []struct {
		name string
		want bool
	}{
		{"submit 1 of 2", true},
		{"submit 2 of 2", true},
		{"submit 3rd refused (backpressure)", false},
	}

	for _, tc := range tests {
		if got := submit(); got != tc.want {
			t.Fatalf("%s: submit() = %v, want %v", tc.name, got, tc.want)
		}
	}

	complete()
	if !submit() {
		t.Fatal("submit after complete: got false, want true (complete must free a slot)")
	}
	if submit() {
		t.Fatal("submit beyond refilled limit: got true, want false")
	}
}

func TestCompleteClampsAtZero(t *testing.T) {
	submit, complete := NewSubmitter(1)

	complete() // complete with nothing in flight must not go negative
	if !submit() {
		t.Fatal("submit after no-op complete: got false, want true")
	}
	if submit() {
		t.Fatal("second submit: got true, want false (limit is 1)")
	}
}

func TestTwoSubmittersDoNotShareInFlightCount(t *testing.T) {
	submitA, _ := NewSubmitter(1)
	submitB, _ := NewSubmitter(1)

	if !submitA() {
		t.Fatal("submitter A first submit: got false, want true")
	}
	if !submitB() {
		t.Fatal("submitter B first submit: got false, want true — submitters must not share captured state")
	}
}

func TestSubmitConcurrentAdmitsExactlyLimit(t *testing.T) {
	const limit = 20
	const attempts = 500

	submit, _ := NewSubmitter(limit)

	var wg sync.WaitGroup
	var admitted atomic.Int64
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if submit() {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := admitted.Load(); got != limit {
		t.Fatalf("admitted = %d, want %d", got, limit)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

The first table walks the gate to its limit, confirms a refusal, then
confirms `complete` frees exactly one slot rather than resetting the counter
entirely. The clamp test is the defensive case an extra `complete` must not
manufacture capacity that was never there. The isolation test is the same
structural guarantee every paired-closure factory in this lesson relies on.
The concurrency test is the one that actually exercises the reason this
module exists: 500 goroutines hammer `submit` at once, and exactly 20 — never
19, never 21 — must be admitted, which only holds if the check and the
increment happen inside the same critical section.

## Resources

- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the shared in-flight counter across submitters.
- [pkg.go.dev: sync/atomic](https://pkg.go.dev/sync/atomic) — the counter the concurrency test uses to aggregate admissions.
- [Go spec: Function literals](https://go.dev/ref/spec#Function_literals) — how `submit` and `complete` share one captured `inFlight`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-per-tenant-request-sampler-closure.md](16-per-tenant-request-sampler-closure.md) | Next: [18-cache-invalidation-subscriber-broadcast.md](18-cache-invalidation-subscriber-broadcast.md)
