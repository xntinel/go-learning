# Exercise 26: Semaphore — Deferred Release Enforces Resource Bounds Under Panic

**Nivel: Intermedio** — validacion rapida (un test corto).

A counting semaphore is how a service admits at most N concurrent uses of
something expensive and finite: N outbound connections to a rate-limited
partner API, N goroutines decoding video simultaneously, N in-flight
calls into a downstream that falls over past a known concurrency. The
bound is only real if every `Acquire` is matched by exactly one
`Release`, on every code path — including the one where the work inside
the bound panics. A semaphore whose `Release` is a plain statement at the
bottom of the function is one bad input away from a permanently leaked
token, and leaked tokens accumulate silently until the semaphore is
full forever and every future `Acquire` blocks. This module builds that
guarantee with `defer` and proves it under an actual panic. The module is
fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
semaphore/                  independent module: example.com/semaphore-bounded-resource-acquire
  go.mod                     go 1.24
  semaphore.go                Semaphore (Acquire, Release, MaxInFlight, HeldTokens), Run(sem, fn) error
  cmd/
    demo/
      main.go                runnable demo: 5 tasks share a bound of 2, one task panics
  semaphore_test.go           bound-never-exceeded case under -race; panic-releases-token case
```

- Files: `semaphore.go`, `cmd/demo/main.go`, `semaphore_test.go`.
- Implement: `Semaphore` (`Acquire`, `Release`, `MaxInFlight`, `HeldTokens`) and `Run(sem *Semaphore, fn func()) (err error)`.
- Test: a barrier-forced concurrency case proving the bound is reached but never exceeded, plus a panic case proving `Release` still runs.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the release order inside Run matters

`Run` registers two defers, in this order: `sem.Release()` first, then a
closure that calls `recover()`. LIFO means they run in the opposite
order at return — the recover closure fires first, and only after it has
decided whether to turn a panic into a returned error does
`sem.Release()` run. This is exactly the order the contract needs. The
recover closure's job is to stop the panic from propagating and instead
report it as `err`; `sem.Release()`'s job is to give the token back no
matter what happened inside `fn`, panic or not. If the order were
reversed — `Release` registered *after* the recover closure — LIFO would
make `Release` run first and the recover closure second, which still
happens to work here because neither depends on the other's side effect.
The real risk is writing `Release` as a plain, non-deferred call at the
bottom of `Run`: that statement is never reached at all when `fn` panics,
because a panic unwinds straight past it, and the token is gone for
good. `defer` is what makes `Release` run regardless of which of the two
ways `Run` can end.

Create `semaphore.go`:

```go
package semaphore

import (
	"fmt"
	"sync"
)

// Semaphore bounds concurrent access to a resource with a fixed number of
// tokens. Acquire blocks until a token is free; Release returns one.
type Semaphore struct {
	tokens chan struct{}

	mu       sync.Mutex
	inFlight int
	maxSeen  int
}

// New returns a semaphore that admits at most n concurrent holders.
func New(n int) *Semaphore {
	return &Semaphore{tokens: make(chan struct{}, n)}
}

// Acquire takes one token, blocking if none is free.
func (s *Semaphore) Acquire() {
	s.tokens <- struct{}{}
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.maxSeen {
		s.maxSeen = s.inFlight
	}
	s.mu.Unlock()
}

// Release returns one token. It must run exactly once per Acquire,
// regardless of how the holder's critical section exits -- which is why
// every caller in this module reaches Release through a defer.
func (s *Semaphore) Release() {
	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	<-s.tokens
}

// MaxInFlight returns the highest number of concurrently held tokens
// observed so far.
func (s *Semaphore) MaxInFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxSeen
}

// HeldTokens returns how many tokens are currently checked out.
func (s *Semaphore) HeldTokens() int {
	return len(s.tokens)
}

// Run acquires sem, runs fn inside the bound, and guarantees the token is
// released even if fn panics.
//
// The two defers registered here run in reverse of the order they are
// written: sem.Release() is registered first and the recover closure
// second, so at return the recover closure fires first -- converting a
// panic into a returned error -- and only then does sem.Release() run.
// That ordering is what this module exists to prove: the semaphore's
// token bound is never violated by a panicking critical section, because
// Release always runs on the way out no matter which path fn took.
func Run(sem *Semaphore, fn func()) (err error) {
	sem.Acquire()
	defer sem.Release()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic recovered: %v", r)
		}
	}()

	fn()
	return nil
}
```

### The runnable demo

Five tasks share a semaphore bounded to 2. Task 2 panics inside its
critical section; `Run` recovers it and reports it as an error without
letting it leak a token.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/semaphore-bounded-resource-acquire"
)

func main() {
	sem := semaphore.New(2)

	results := make([]error, 5)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = semaphore.Run(sem, func() {
				if i == 2 {
					panic("task 2 exploded")
				}
			})
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		fmt.Printf("task %d error: %v\n", i, err)
	}
	fmt.Printf("tokens held after all tasks finished: %d (semaphore fully released)\n", sem.HeldTokens())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
task 0 error: <nil>
task 1 error: <nil>
task 2 error: panic recovered: task 2 exploded
task 3 error: <nil>
task 4 error: <nil>
tokens held after all tasks finished: 0 (semaphore fully released)
```

### Tests

`TestSemaphoreNeverExceedsBound` forces `bound` goroutines to a barrier
while each still holds its token — this is what makes "the bound was
actually reached" a proven fact rather than a hopeful guess about
scheduling — then asserts `MaxInFlight` equals the bound exactly and
every token is returned afterward.
`TestReleaseRunsEvenWhenCriticalSectionPanics` drives `Run` with a
panicking `fn` and then proves the token came back by successfully
acquiring and releasing it again with a timeout guard, so a regression
that leaks the token fails the test instead of hanging it forever.

Create `semaphore_test.go`:

```go
package semaphore

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSemaphoreNeverExceedsBound forces bound goroutines to arrive at a
// barrier while each still holds its token, proving the semaphore actually
// reaches its configured bound (not just "stays below it by luck"), and
// that it never exceeds it: a bound+1th goroutine cannot acquire until one
// of the first bound releases.
func TestSemaphoreNeverExceedsBound(t *testing.T) {
	const bound = 3
	const workers = 10
	sem := New(bound)

	var arrived int32
	release := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem.Acquire()
			defer sem.Release()
			if atomic.AddInt32(&arrived, 1) == bound {
				close(release)
			}
			<-release
		}()
	}
	wg.Wait()

	if got := sem.MaxInFlight(); got != bound {
		t.Fatalf("max in flight = %d, want exactly %d", got, bound)
	}
	if got := sem.HeldTokens(); got != 0 {
		t.Fatalf("tokens held after all workers finished = %d, want 0", got)
	}
}

// TestReleaseRunsEvenWhenCriticalSectionPanics proves the deferred Release
// still runs on the panic path: if it did not, the token would leak and a
// subsequent Acquire on a full-capacity semaphore would block forever.
func TestReleaseRunsEvenWhenCriticalSectionPanics(t *testing.T) {
	sem := New(1)

	err := Run(sem, func() { panic("boom") })
	if err == nil {
		t.Fatal("want non-nil error from recovered panic")
	}

	done := make(chan struct{})
	go func() {
		sem.Acquire()
		sem.Release()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Acquire blocked: Release did not run after panic, token leaked")
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

`Run` is correct when the semaphore's bound holds under every possible
exit from `fn` — normal return or panic — and never leaks a token in
either case. `defer sem.Release()` is what makes the panic path safe: it
runs during the stack unwind exactly as it would on a normal return,
before the recovered error even finishes being constructed. The mistake
this design avoids is treating `Release()` as a plain statement that "of
course" runs at the end of the function, which is only true for the
success path; the first panic through that code leaks a token that
never comes back, and the semaphore's advertised bound silently becomes
one slot smaller, forever, per panic.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — deferred functions still run while a panic unwinds the stack.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the mechanics `Run`'s two stacked defers rely on.
- [pkg.go.dev: golang.org/x/sync/semaphore](https://pkg.go.dev/golang.org/x/sync/semaphore) — a production-grade weighted semaphore with the same acquire/release contract.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-request-coalescing-singleflight-deferred.md](25-request-coalescing-singleflight-deferred.md) | Next: [27-streaming-etl-pipeline-stage-cleanup.md](27-streaming-etl-pipeline-stage-cleanup.md)
