# Exercise 10: Write Deterministic Concurrency Tests with testing/synctest

Concurrency tests that fence with `time.Sleep` are flaky: sleep too little and the
background goroutine has not reacted; sleep too much and the suite crawls. Go 1.25
stabilized `testing/synctest`, which runs goroutines in an isolated bubble with a
fake clock and lets you block deterministically until every other goroutine is
durably blocked. This module rewrites a config hot-reload publish/observe test — the
timing-sensitive kind — as a deterministic `synctest` bubble, still `-race` clean.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. It requires Go 1.25+ for
`testing/synctest`. Nothing here imports any other exercise.

## What you'll build

```text
synctestrace/              independent module: example.com/synctestrace (go 1.25+)
  go.mod                   go 1.25 (synctest needs it)
  synctestrace.go          ConfigStore on atomic.Pointer[Config]; a watcher goroutine that publishes on a ticker
  cmd/
    demo/
      main.go              runnable demo: real-clock watcher publishes generations, reader observes the latest
  synctestrace_test.go     synctest bubble: publish then synctest.Wait, observe deterministically; timeout path
```

- Files: `synctestrace.go`, `cmd/demo/main.go`, `synctestrace_test.go`.
- Implement: a `ConfigStore` on `atomic.Pointer[Config]` and a `Watcher` goroutine that publishes a new generation every interval until its context is cancelled.
- Test: a `synctest.Test` bubble that advances virtual time to trigger a publish, calls `synctest.Wait` to fence, and deterministically asserts the reader observed the new generation; a `context.WithTimeout` path asserted on the fake clock; still under `-race`.
- Verify: `go test -count=1 -race ./...` (requires Go 1.25+).

Set up the module. `testing/synctest` requires Go 1.25+, so pin the language
version:

```bash
go mod edit -go=1.25
```

### The bubble, the fake clock, and synctest.Wait

`synctest.Test(t, func(t *testing.T){...})` runs its function in an isolated
"bubble". Inside, the `time` package is virtualized: the clock starts at a fixed
instant and only advances when *every* goroutine in the bubble is durably blocked
(blocked on an in-bubble channel, a `WaitGroup`, a `Cond`, or `time.Sleep`). So a
`Watcher` that publishes on a `time.NewTicker(time.Second)` does not wait a real
second — when all bubble goroutines are blocked, the clock jumps to the next timer
and fires the tick. A test of "publish every second for a minute" runs in
microseconds and is deterministic in time.

`synctest.Wait()` is the fence that removes the race between "I advanced the clock"
and "the watcher reacted". It blocks the calling goroutine until every *other*
goroutine in the bubble is durably blocked. So the test pattern is:
`time.Sleep(interval)` to let the ticker fire, then `synctest.Wait()` to guarantee
the watcher has finished publishing and parked again, then read what the reader
observed. Without the `Wait`, the assertion might run before the publish landed —
the exact flakiness real-sleep tests suffer, now removed.

Two rules keep the bubble honest. First, no real I/O inside the bubble: a goroutine
blocked on a socket or a syscall is not *durably* blocked (it can be unblocked from
outside the bubble), so the clock cannot advance and the test deadlocks — use
in-memory state only, as here. Second, the `Watcher` must be cancellable: `synctest.Test`
waits for every bubble goroutine to exit before returning, so a ticker goroutine
with no stop is reported as a deadlock, not a silent leak. The watcher watches
`ctx.Done()` and the test defers `cancel()`. Also note `t.Parallel`, `t.Run`, and
`t.Deadline` are forbidden on the bubble's `T`; mark the *outer* test parallel
instead.

The `atomic.Pointer[Config]` copy-on-write store is the same safe read-mostly
pattern from Exercise 4: readers `Load` a snapshot lock-free, the watcher `Store`s a
fresh whole value. synctest makes the *timing* of the publish deterministic without
changing that synchronization, and `-race` confirms the store is still the only
edge needed.

Create `synctestrace.go`:

```go
package synctestrace

import (
	"context"
	"sync/atomic"
	"time"
)

// Config is an immutable published snapshot.
type Config struct {
	Generation int
}

// ConfigStore holds the live config behind an atomic pointer (copy-on-write).
type ConfigStore struct {
	ptr atomic.Pointer[Config]
}

func NewConfigStore(initial *Config) *ConfigStore {
	s := &ConfigStore{}
	s.ptr.Store(initial)
	return s
}

// Load returns the current snapshot lock-free.
func (s *ConfigStore) Load() *Config {
	return s.ptr.Load()
}

// publish swaps in a fresh whole value; the old snapshot is never mutated.
func (s *ConfigStore) publish(next *Config) {
	s.ptr.Store(next)
}

// Watcher publishes a new generation every interval until ctx is cancelled. It
// reads time through time.NewTicker, which a synctest bubble virtualizes.
func Watch(ctx context.Context, s *ConfigStore, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	gen := s.Load().Generation
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			gen++
			s.publish(&Config{Generation: gen})
		}
	}
}
```

### The runnable demo

The demo runs the watcher against the real clock so you can watch generations
advance; it uses short intervals to stay quick.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/synctestrace"
)

func main() {
	store := synctestrace.NewConfigStore(&synctestrace.Config{Generation: 0})

	ctx, cancel := context.WithCancel(context.Background())
	go synctestrace.Watch(ctx, store, 10*time.Millisecond)

	time.Sleep(35 * time.Millisecond) // let ~3 ticks fire
	cancel()
	time.Sleep(5 * time.Millisecond) // let the watcher exit

	fmt.Printf("observed generation >= 3: %v\n", store.Load().Generation >= 3)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
observed generation >= 3: true
```

### Tests

`TestPublishObserveDeterministic` runs inside a `synctest` bubble: it starts the
watcher, sleeps one interval in virtual time to trigger a publish, calls
`synctest.Wait()` to fence until the watcher has published and parked, and then
asserts the reader observes exactly generation 1 — no arbitrary sleep, no flake.
Advancing another interval and fencing again pins generation 2. `TestTimeoutPath`
asserts a `context.WithTimeout` fires on the fake clock deterministically. The outer
tests are `t.Parallel`; the bubble functions are not.

Create `synctestrace_test.go`:

```go
package synctestrace

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestPublishObserveDeterministic(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := NewConfigStore(&Config{Generation: 0})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go Watch(ctx, store, time.Second)

		// Advance one interval in virtual time; the ticker fires.
		time.Sleep(time.Second)
		synctest.Wait() // fence: watcher has published and parked again

		if got := store.Load().Generation; got != 1 {
			t.Fatalf("after one tick, generation = %d, want 1", got)
		}

		time.Sleep(time.Second)
		synctest.Wait()

		if got := store.Load().Generation; got != 2 {
			t.Fatalf("after two ticks, generation = %d, want 2", got)
		}
	})
}

func TestTimeoutPath(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		done := make(chan error, 1) // buffered so the loser of the select can exit
		go func() {
			<-ctx.Done()
			done <- ctx.Err()
		}()

		time.Sleep(time.Second) // reach the deadline in virtual time
		synctest.Wait()

		select {
		case err := <-done:
			if err != context.DeadlineExceeded {
				t.Fatalf("ctx error = %v, want context.DeadlineExceeded", err)
			}
		default:
			t.Fatal("context did not time out on the fake clock")
		}
	})
}

func TestLoadNeverNil(t *testing.T) {
	t.Parallel()
	store := NewConfigStore(&Config{Generation: 0})
	if store.Load() == nil {
		t.Fatal("Load returned nil from a seeded store")
	}
}
```

## Review

The test is correct when the publish/observe assertion is deterministic — no
`time.Sleep`-and-hope — and still `-race` clean. The mechanism is the bubble's fake
clock plus `synctest.Wait()`: advance virtual time to fire the ticker, then `Wait`
to fence until the watcher has published and re-blocked, then read. The mistakes to
avoid are the bubble's rules: do not call `t.Parallel`/`t.Run` on the bubble's `T`
(mark the outer test parallel), do not do real I/O inside the bubble (a goroutine
blocked outside the bubble never becomes durably blocked and the clock stalls), and
give every background goroutine a way to stop (`ctx.Done` + `defer cancel`) or
`synctest.Test` reports a deadlock waiting for it to exit. Buffer the timeout test's
result channel so the select's loser can still send and exit. This requires Go
1.25+; the module pins `go 1.25`.

## Resources

- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — `Test`, `Wait`, the bubble, and the fake clock.
- [Go Blog: Testing concurrent code with testing/synctest](https://go.dev/blog/synctest) — the design and why no clock interface is needed.
- [`sync/atomic` — `Pointer[T]`](https://pkg.go.dev/sync/atomic#Pointer) — the copy-on-write store under test.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../10-designing-pointer-safe-apis/00-concepts.md](../10-designing-pointer-safe-apis/00-concepts.md)
