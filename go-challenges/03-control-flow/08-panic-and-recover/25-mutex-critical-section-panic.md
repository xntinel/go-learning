# Exercise 25: RWMutex Critical Section: Guaranteeing Lock Release on Panic

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A read-write mutex guarding a hot in-memory store — a request-count cache, a
feature-flag snapshot, a rate-limit bucket — is only as safe as its worst
critical section. The moment one goroutine's write-side update panics while
holding the lock, every other goroutine blocked in `Lock` or `RLock` for
that same mutex stalls forever: there is no timeout, no automatic recovery,
just a service that silently stops making progress on one key until the
process is restarted. This module builds `Store.Update`, a write-locked
critical section whose caller-supplied mutation function can panic, and
proves the lock is always released and the panic is always converted into
an observable error rather than a frozen mutex. It is fully self-contained:
its own module, demo, and tests.

## What you'll build

```text
safestore/                  independent module: example.com/safestore
  go.mod                     go 1.24
  safestore.go                Store, NewStore, Update, Get
  cmd/
    demo/
      main.go                runnable demo: a panicking update, then a clean recovery
  safestore_test.go            panic-then-Get liveness check, concurrent panic/success mix
```

Files: `safestore.go`, `cmd/demo/main.go`, `safestore_test.go`.
Implement: `Store.Update(key string, fn func(current int) int) error` that takes the write lock, defers its release before ever calling `fn`, and recovers any panic from `fn` into a returned error; `Store.Get(key string) int` that takes the read lock.
Test: a single `Update` whose `fn` panics, asserting a subsequent `Get` completes without deadlocking (guarded by a timeout); ten concurrent goroutines updating the same key where half panic and half succeed, asserting none of them deadlock and the final value reflects only the successful updates.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/mutex-critical-section-panic/cmd/demo
cd ~/go-exercises/mutex-critical-section-panic
go mod init example.com/safestore
go mod edit -go=1.24
```

### Why the lock must be deferred before fn runs, and why that alone is not the whole story

`Update` takes the write lock and immediately defers `s.mu.Unlock()` as the
very next statement — before `fn` is ever invoked. This ordering is not
stylistic. If a critical section instead called `fn` first and released the
lock as a plain statement afterward ("lock, mutate, unlock" written straight
through), a panic inside `fn` would skip the unlock entirely, because a
panic unwinds past ordinary statements and only stops at deferred calls.
Every other goroutine anywhere in the process that ever calls `Update` or
`Get` on that `Store` again blocks forever the instant that happens — a
write-lock panic starves readers too, since `RLock` cannot proceed while a
writer holds the lock. A `Store` is only ever obtained through `NewStore`
and always shared by pointer, never copied by value: copying a struct that
embeds a `sync.RWMutex` after it has been used is undefined behavior, and
`go vet`'s `copylocks` check exists precisely to catch that mistake before
it reaches production.

Deferring the unlock is necessary but not sufficient for a *usable* API: a
caller also needs to know that the mutation failed, rather than silently
seeing the old value with no explanation. `Update` therefore also defers a
recover — registered *after* the unlock defer, so it runs *before* the
unlock defer executes (deferred calls run last-in-first-out). The recover
converts `fn`'s panic into a returned `error`, then execution continues
unwinding normally through the remaining defers in `Update`, including the
`Unlock` — recovering a panic inside one deferred function does not skip
the *other* deferred calls already registered in that same function; it
only stops the panic from propagating further up the call stack. The net
effect: whether `fn` panics or returns normally, the lock is released
exactly once and the caller always gets a clean answer.

Create `safestore.go`:

```go
package safestore

import (
	"fmt"
	"sync"
)

// Store is a concurrency-safe key/value counter store guarded by a
// read-write mutex: readers (Get) take the read lock and can run
// concurrently with each other, while writers (Update) take the exclusive
// write lock.
type Store struct {
	mu   sync.RWMutex
	data map[string]int
}

// NewStore returns a ready-to-use Store. Always obtain a Store through this
// constructor and share it by pointer - never copy a Store by value, since
// copying its embedded sync.RWMutex after first use is undefined behavior
// (go vet's copylocks check flags it for exactly this reason).
func NewStore() *Store {
	return &Store{data: make(map[string]int)}
}

// Update acquires the write lock, computes the new value for key by calling
// fn with the current value, and stores the result. The write lock is
// always released, even if fn panics: if fn's logic has a bug, Update
// recovers the panic and returns it as an error instead of leaving the
// mutex held forever, which would deadlock every other goroutine currently
// blocked in Update or Get waiting on this same Store.
func (s *Store) Update(key string, fn func(current int) int) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = fmt.Errorf("update %q panicked: %w", key, e)
				return
			}
			err = fmt.Errorf("update %q panicked: %v", key, r)
		}
	}()
	s.data[key] = fn(s.data[key])
	return nil
}

// Get reads the current value for key under the read lock.
func (s *Store) Get(key string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data[key]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/safestore"
)

func main() {
	store := safestore.NewStore()

	err := store.Update("requests", func(current int) int {
		return current + 1
	})
	fmt.Println("update ok:", err)

	err = store.Update("requests", func(current int) int {
		var counters []int
		return counters[current] // index out of range: current is 1
	})
	fmt.Println("update panicked:", err)

	// Prove the write lock was released: this RLock must not deadlock.
	fmt.Println("value after panic:", store.Get("requests"))

	err = store.Update("requests", func(current int) int {
		return current + 1
	})
	fmt.Println("update ok again:", err)
	fmt.Println("final value:", store.Get("requests"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
update ok: <nil>
update panicked: update "requests" panicked: runtime error: index out of range [1] with length 0
value after panic: 1
update ok again: <nil>
final value: 2
```

### Tests

`TestUpdatePanicReturnsErrorAndReleasesLock` proves the liveness guarantee
directly: after a panicking `Update`, a `Get` performed on its own goroutine
must complete within a short timeout, not hang — a real deadlock would fail
this test by timing out rather than by an assertion mismatch.
`TestConcurrentUpdatesWithPanicNeverDeadlock` drives ten goroutines
concurrently against the same key, half of them panicking and half
succeeding, and asserts the whole batch completes (again guarded by a
timeout) with the final value reflecting exactly the successful half.

Create `safestore_test.go`:

```go
package safestore

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUpdateAppliesFn(t *testing.T) {
	s := NewStore()
	if err := s.Update("k", func(current int) int { return current + 5 }); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if got := s.Get("k"); got != 5 {
		t.Fatalf("Get(k) = %d, want 5", got)
	}
}

func TestUpdatePanicReturnsErrorAndReleasesLock(t *testing.T) {
	s := NewStore()
	err := s.Update("k", func(current int) int {
		var bad []int
		return bad[current] // index out of range
	})
	if err == nil {
		t.Fatal("Update err = nil, want a panic error")
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %v, want it to mention panicked", err)
	}

	// The write lock must have been released: an RLock-based Get must
	// complete immediately, not deadlock.
	done := make(chan int, 1)
	go func() { done <- s.Get("k") }()
	select {
	case got := <-done:
		if got != 0 {
			t.Fatalf("Get(k) = %d, want 0 (fn panicked before mutating)", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Get deadlocked: write lock was not released after panic")
	}
}

func TestConcurrentUpdatesWithPanicNeverDeadlock(t *testing.T) {
	s := NewStore()
	const n = 10
	var wg sync.WaitGroup
	errCh := make(chan error, n)

	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				err = s.Update("shared", func(current int) int {
					var bad []int
					return bad[current] // always panics
				})
			} else {
				err = s.Update("shared", func(current int) int {
					return current + 1
				})
			}
			errCh <- err
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("goroutines deadlocked: write lock was never released after a panic")
	}
	close(errCh)

	panicked, ok := 0, 0
	for err := range errCh {
		if err != nil {
			panicked++
		} else {
			ok++
		}
	}
	if panicked != n/2 {
		t.Fatalf("panicked = %d, want %d", panicked, n/2)
	}
	if ok != n/2 {
		t.Fatalf("ok = %d, want %d", ok, n/2)
	}
	if got := s.Get("shared"); got != n/2 {
		t.Fatalf("Get(shared) = %d, want %d (only successful updates increment)", got, n/2)
	}
}
```

## Review

`Update` is correct when the lock release does not depend on `fn`
behaving — it is structurally guaranteed by `defer`, registered before
`fn` is ever called, not by hoping the caller-supplied mutation never
panics. The recover is a second, independent concern layered on top:
without it, a panicking `fn` would still release the lock correctly, but
it would also crash the entire process (recover has no effect unless it is
actually invoked), taking down every other in-flight request with it. The
error most likely to slip through review is locking, then calling `fn`,
then unlocking as three separate plain statements — it reads identically
to the correct version until a panic actually occurs in production, at
which point every future request touching that `Store` hangs instead of
one request failing cleanly.

## Resources

- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — the read-write lock whose `Lock`/`Unlock` pairing this exercise depends on getting right under panic.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — why deferred calls still run in order after one of them recovers.
- [go vet: copylocks](https://pkg.go.dev/cmd/vet#hdr-Copying_locks) — the static check that catches copying a struct embedding a mutex after first use.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-streaming-json-recovery.md](24-streaming-json-recovery.md) | Next: [26-multi-phase-rollback-transaction.md](26-multi-phase-rollback-transaction.md)
