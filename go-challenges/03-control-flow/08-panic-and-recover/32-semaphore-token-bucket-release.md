# Exercise 32: Semaphore with Panic-Safe Token Release: No Starvation on Failure

**Nivel: Intermedio** — validacion rapida (un test corto).

A bounded-concurrency semaphore — capping how many expensive database
migrations, external API calls, or heavyweight jobs run at once — is a
liability the moment a token acquired for one job is never given back. A
single panicking job that leaks its tokens does not just fail that job; it
permanently shrinks the pool's effective capacity by however many tokens
it held, and after enough failures the semaphore silently starves every
future caller of work it will never release on its own. This module builds
`Semaphore.Do`, which acquires N tokens, runs the caller's work and its
cleanup, and guarantees the tokens come back even if either one panics. It
is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
semaphore/                  independent module: example.com/semaphore
  go.mod                     go 1.24
  semaphore.go                 Semaphore, New, Available, Do, runGuarded
  cmd/
    demo/
      main.go                runnable demo: fn and cleanup both panic, tokens still recoverable
  semaphore_test.go            fn panic releases tokens, fn+cleanup both panic releases tokens, sequential reuse
```

Files: `semaphore.go`, `cmd/demo/main.go`, `semaphore_test.go`.
Implement: `Semaphore.Do(n int, fn, cleanup func()) error` that acquires `n` tokens, defers their release before calling anything else, runs `fn` and `cleanup` each under their own recover boundary, and combines any panics into one returned error via `errors.Join`.
Test: `fn` panicking, asserting all `n` tokens are back in `Available()`; both `fn` and `cleanup` panicking, asserting both messages are present in the returned error and tokens are still fully released; a clean run; a second acquisition succeeding normally right after a panicking first one.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the release is deferred immediately, and why fn and cleanup are still separately guarded

`Do` acquires its `n` tokens first and defers their release as the very
next statement, before `fn` or `cleanup` are ever called. This ordering is
the whole point of the exercise: a naive implementation written as a
straight sequence - "acquire; call fn; call cleanup; release" - loses the
tokens forever the instant `fn` or `cleanup` panics, because the release
statement sits after both calls and a panic unwinds straight past ordinary
statements without executing them. Every acquirer after that is
permanently starved of tokens nothing will ever give back, since nothing
about a panicking goroutine's crash automatically returns anything to a
channel-backed semaphore. Deferring the release before either call runs
makes the return of all `n` tokens structural - guaranteed by the Go
runtime's unwind mechanics - rather than contingent on `fn` and `cleanup`
behaving.

Given that the tokens are already safe by construction, `fn` and `cleanup`
are *additionally* run under their own `runGuarded` recover boundaries for
a second, independent reason: observability. Without it, a panicking `fn`
would still release its tokens correctly, but it would also crash the
entire calling goroutine (recover only helps if something actually calls
it), which is not what "no permanent starvation" is supposed to buy a
caller - the point is graceful degradation, not merely-safe-token-counts
followed by a crash. If both `fn` and `cleanup` panic, `errors.Join` keeps
both messages rather than letting the cleanup panic silently replace `fn`'s
- the same "never lose the original failure to a cleanup-time panic"
discipline that governs every rollback path in this chapter.

Create `semaphore.go`:

```go
package semaphore

import (
	"errors"
	"fmt"
)

// Semaphore is a counting semaphore backed by a buffered channel of tokens.
type Semaphore struct {
	tokens chan struct{}
}

// New returns a Semaphore with capacity tokens, all initially available.
func New(capacity int) *Semaphore {
	s := &Semaphore{tokens: make(chan struct{}, capacity)}
	for i := 0; i < capacity; i++ {
		s.tokens <- struct{}{}
	}
	return s
}

// Available reports how many tokens are currently free.
func (s *Semaphore) Available() int {
	return len(s.tokens)
}

// Do acquires n tokens, runs fn, then always runs cleanup, and always
// releases the n tokens - regardless of whether fn panics, cleanup panics,
// or both. The release is deferred immediately after acquisition,
// structurally guaranteeing it runs even if something above it fails,
// rather than depending on fn and cleanup happening to behave: a naive
// "acquire; fn(); cleanup(); release()" written as a straight sequence
// leaks the n tokens forever the moment either call panics, because the
// release statement is simply never reached. Every acquirer after that is
// permanently starved of tokens that no failure will ever give back.
//
// fn and cleanup are additionally each run under their own recover
// boundary, so a panic in either is reported back as an error instead of
// crashing the caller - and if both panic, both are preserved via
// errors.Join instead of the cleanup panic silently replacing fn's.
func (s *Semaphore) Do(n int, fn func(), cleanup func()) (err error) {
	for i := 0; i < n; i++ {
		<-s.tokens
	}
	defer func() {
		for i := 0; i < n; i++ {
			s.tokens <- struct{}{}
		}
	}()

	fnErr := runGuarded("fn", fn)
	cleanupErr := runGuarded("cleanup", cleanup)
	return errors.Join(fnErr, cleanupErr)
}

func runGuarded(label string, f func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = fmt.Errorf("%s panicked: %w", label, e)
				return
			}
			err = fmt.Errorf("%s panicked: %v", label, r)
		}
	}()
	if f != nil {
		f()
	}
	return nil
}
```

### The runnable demo

A capacity-2 semaphore runs a job whose `fn` panics on an index-out-of-range
read, and whose `cleanup` also panics trying to close an already-closed
connection. Both tokens still come back.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/semaphore"
)

func main() {
	sem := semaphore.New(2)
	fmt.Println("available before:", sem.Available())

	err := sem.Do(2, func() {
		var rows []string
		fmt.Println(rows[0]) // index out of range
	}, func() {
		panic("cleanup: connection already closed")
	})

	fmt.Println("do error:", err)
	fmt.Println("available after:", sem.Available())

	// A later caller can still acquire the tokens - no permanent starvation.
	err = sem.Do(1, func() { fmt.Println("second job: ok") }, nil)
	fmt.Println("second do error:", err)
	fmt.Println("available finally:", sem.Available())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
available before: 2
do error: fn panicked: runtime error: index out of range [0] with length 0
cleanup panicked: cleanup: connection already closed
available after: 2
second job: ok
second do error: <nil>
available finally: 2
```

### Tests

`TestDoReleasesTokensWhenBothFnAndCleanupPanic` is the core case: both
callbacks panic, and the test asserts both messages survive in the combined
error while `Available()` still reports full capacity.
`TestDoSequentialAcquisitionsAfterPanicDoNotStarve` proves a panic in one
call never blocks a completely separate later call from acquiring cleanly.

Create `semaphore_test.go`:

```go
package semaphore

import (
	"errors"
	"strings"
	"testing"
)

func TestDoReleasesTokensWhenFnPanics(t *testing.T) {
	s := New(3)
	err := s.Do(3, func() { panic(errors.New("boom")) }, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want it to wrap boom", err)
	}
	if got := s.Available(); got != 3 {
		t.Fatalf("Available() = %d, want 3 (tokens must be released)", got)
	}
}

func TestDoReleasesTokensWhenBothFnAndCleanupPanic(t *testing.T) {
	s := New(2)
	err := s.Do(2, func() { panic("fn broke") }, func() { panic("cleanup broke") })
	if err == nil {
		t.Fatal("err = nil, want both panics reported")
	}
	if !strings.Contains(err.Error(), "fn broke") || !strings.Contains(err.Error(), "cleanup broke") {
		t.Fatalf("err = %v, want both fn and cleanup panics present", err)
	}
	if got := s.Available(); got != 2 {
		t.Fatalf("Available() = %d, want 2 (no permanent starvation)", got)
	}
}

func TestDoSucceedsAndReleasesTokens(t *testing.T) {
	s := New(1)
	ran := false
	err := s.Do(1, func() { ran = true }, nil)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !ran {
		t.Fatal("fn was never called")
	}
	if got := s.Available(); got != 1 {
		t.Fatalf("Available() = %d, want 1", got)
	}
}

func TestDoSequentialAcquisitionsAfterPanicDoNotStarve(t *testing.T) {
	s := New(1)
	_ = s.Do(1, func() { panic("first job broke") }, nil)
	ran := false
	err := s.Do(1, func() { ran = true }, nil)
	if err != nil || !ran {
		t.Fatalf("err = %v ran = %v, want the second acquire to succeed normally", err, ran)
	}
}
```

## Review

`Do` is correct when the token release is guaranteed by `defer` registered
before any risky call runs, not merely likely because `fn` and `cleanup`
are also individually guarded. The two layers solve different problems:
the `defer` guarantees no permanent starvation no matter what; the
per-callback recover boundaries turn what would otherwise be a full crash
into an observable, joined error. Reviewing this kind of code, the single
question worth asking is "does the release statement appear before or
after the risky calls in program order?" - if it is a plain statement after
them rather than a `defer` before them, the semaphore is one panic away
from permanently losing capacity.

## Resources

- [Bounded concurrency with buffered channels](https://go.dev/blog/pipelines) — the channel-as-semaphore pattern this module builds on.
- [errors.Join](https://pkg.go.dev/errors#Join) — combining an `fn` panic and a `cleanup` panic into one error without losing either.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — why the release must be a deferred call, not a trailing statement.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-fan-in-aggregator-partial-failure.md](31-fan-in-aggregator-partial-failure.md) | Next: [33-pipelined-stages-cascade-stop.md](33-pipelined-stages-cascade-stop.md)
