# Exercise 8: Critical Section That Survives a Panic Without Poisoning the Mutex

A panic between `mu.Lock()` and a non-deferred `mu.Unlock()` is one of the worst
failures in a concurrent server: the mutex stays locked forever, and every future
goroutine that tries to acquire it blocks eternally — one bad callback becomes a
total deadlock. This module builds a guarded store whose critical section runs an
untrusted callback and proves `defer mu.Unlock()` keeps the mutex usable through a
panic, under `-race`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
guardedmap/                  independent module: example.com/guardedmap
  go.mod                     go 1.26
  guardedmap.go              Store; Update(key, fn) error; Get; Ops
  cmd/
    demo/
      main.go                runnable demo: clean update, panicking update, still usable
  guardedmap_test.go         panic does not poison mutex (timeout guard); concurrent invariant under -race
```

Files: `guardedmap.go`, `cmd/demo/main.go`, `guardedmap_test.go`.
Implement: a `Store` with a `sync.Mutex`, and `Update(key string, fn func(cur int) int) error` whose critical section defers `mu.Unlock` so a panic in `fn` cannot leave the mutex locked, converts the panic into an error, and keeps the guarded state consistent (a failed update mutates nothing).
Test: a callback that panics returns an error AND a subsequent lock acquisition by another goroutine succeeds within a timeout (no deadlock); a concurrency test with many goroutines, some panicking, runs under `-race` and asserts the final invariant.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/guardedmap/cmd/demo
cd ~/go-exercises/guardedmap
go mod init example.com/guardedmap
go mod edit -go=1.26
```

### defer mu.Unlock is not stylistic — it is the deadlock fix

The rule "always `defer mu.Unlock()` right after `mu.Lock()`" is often taught as
tidiness. It is not: it is the only thing standing between a panicking critical
section and a permanently deadlocked service. A `sync.Mutex` has no concept of an
owner and no poisoning-and-recovery like some other languages; if the goroutine
that locked it unwinds through a panic without unlocking, the lock is simply held
forever. Deferring the unlock means it runs during the panic unwind — the mutex is
released no matter how the critical section exits.

`Update` layers two deferred functions. `defer s.mu.Unlock()` guarantees the lock
is freed. A second deferred closure calls `recover()` and converts a panic in the
user callback `fn` into a returned error, so a bad callback fails *this one update*
without crashing the process. The ordering matters for reading, not for safety:
deferred functions run LIFO, so the recover runs first (stopping the panic and
setting `err`) and the unlock runs second — but even reversed, the unlock would
still run, because unlocking never panics. The important invariant is that the
mutating writes (`s.data[key] = next`, `s.ops++`) happen *after* the callback
returns: if `fn` panics, neither write executes, so the store is left exactly as it
was — no half-applied state. That is what "keeps the guarded state consistent"
means, and it is why `Ops` counts only clean updates.

Create `guardedmap.go`:

```go
package guardedmap

import (
	"fmt"
	"sync"
)

// Store is a concurrency-safe integer map whose Update runs an untrusted callback
// inside the locked section. A panic in the callback becomes an error and cannot
// poison the mutex or leave partially-applied state.
type Store struct {
	mu   sync.Mutex
	data map[string]int
	ops  int
}

func New() *Store {
	return &Store{data: make(map[string]int)}
}

// Update applies fn to the current value under key and stores the result. If fn
// panics, Update returns an error, the mutex is released (deferred Unlock), and
// neither the value nor the op counter is mutated.
func (s *Store) Update(key string, fn func(cur int) int) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("update %q panicked: %v", key, r)
		}
	}()

	next := fn(s.data[key]) // may panic; nothing below runs if it does
	s.data[key] = next
	s.ops++
	return nil
}

// Get returns the value under key.
func (s *Store) Get(key string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

// Ops reports the number of successful updates.
func (s *Store) Ops() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ops
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/guardedmap"
)

func main() {
	s := guardedmap.New()

	_ = s.Update("hits", func(cur int) int { return cur + 1 })
	_ = s.Update("hits", func(cur int) int { return cur + 1 })

	// A panicking callback fails this update but does not poison the mutex.
	err := s.Update("hits", func(cur int) int { panic("bad computation") })
	fmt.Println("panicking update err:", err)

	// The store is still usable and its state is consistent.
	_ = s.Update("hits", func(cur int) int { return cur + 1 })

	v, _ := s.Get("hits")
	fmt.Printf("hits=%d ops=%d\n", v, s.Ops())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
panicking update err: update "hits" panicked: bad computation
hits=3 ops=3
```

### Tests

`TestPanicDoesNotPoisonMutex` triggers a panic in the critical section, then has
*another* goroutine acquire the lock; a `time.After` timeout fails the test if that
goroutine is stuck, which is exactly what a poisoned mutex would cause.
`TestConcurrentPanicsKeepInvariant` runs hundreds of goroutines, a third of them
panicking, under `-race`, and asserts the final value and op counter equal the
number of clean updates — proving both mutex safety and state consistency.

Create `guardedmap_test.go`:

```go
package guardedmap

import (
	"sync"
	"testing"
	"time"
)

func TestPanicDoesNotPoisonMutex(t *testing.T) {
	t.Parallel()
	s := New()

	err := s.Update("k", func(int) int { panic("boom") })
	if err == nil {
		t.Fatal("panicking Update should return an error")
	}
	// The failed update must not have mutated state.
	if _, ok := s.Get("k"); ok {
		t.Fatal("failed update leaked a value into the store")
	}

	// The mutex must be free: another goroutine can lock it. If it were poisoned,
	// this goroutine would block forever and the timeout would fire.
	done := make(chan struct{})
	go func() {
		_ = s.Update("k", func(cur int) int { return cur + 1 })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("mutex poisoned: a later Update deadlocked after a panic")
	}

	if v, _ := s.Get("k"); v != 1 {
		t.Fatalf("value = %d, want 1 after one clean update", v)
	}
}

func TestConcurrentPanicsKeepInvariant(t *testing.T) {
	t.Parallel()
	s := New()

	const n = 600
	var wg sync.WaitGroup
	var countMu sync.Mutex
	okCount := 0

	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%3 == 0 {
				_ = s.Update("k", func(int) int { panic("boom") })
				return
			}
			_ = s.Update("k", func(cur int) int { return cur + 1 })
			countMu.Lock()
			okCount++
			countMu.Unlock()
		}()
	}
	wg.Wait()

	if got := s.Ops(); got != okCount {
		t.Fatalf("Ops() = %d, want %d (one per clean update)", got, okCount)
	}
	if v, _ := s.Get("k"); v != okCount {
		t.Fatalf("value = %d, want %d", v, okCount)
	}
}
```

## Review

The store is correct when a panicking callback fails only its own update: it
returns an error, leaves the value and op counter untouched, and — critically —
releases the mutex so every subsequent caller proceeds. `TestPanicDoesNotPoisonMutex`
would hang (and time out) if the unlock were not deferred, which is the whole point:
a non-deferred `mu.Unlock()` after the callback would be skipped by the panic and
deadlock the store permanently. Placing the mutating writes after the callback
returns keeps state consistent, so `TestConcurrentPanicsKeepInvariant` can assert
the op count equals the number of clean updates even with a third of the goroutines
panicking. Run `go test -race` — it must pass cleanly, proving the mutex genuinely
guards the map.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — no ownership, no auto-recovery; a held lock stays held.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — deferred unlock during an unwind.
- [Go Code Review Comments: mutex](https://go.dev/wiki/CodeReviewComments) — deferring unlock as the standard idiom.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-assert-panics-testing-helper.md](09-assert-panics-testing-helper.md)
