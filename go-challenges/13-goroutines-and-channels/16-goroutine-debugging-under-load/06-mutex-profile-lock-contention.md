# Exercise 6: Find the hot lock with the mutex contention profile

The block profile tells you goroutines are waiting; the mutex profile tells you
*which lock* they are waiting on and who was holding it. This exercise hammers a
mutex-guarded counter from many goroutines, enables mutex profiling to localize the
contended critical section, and shows that an atomic rewrite of the same counter
produces no mutex contention at all.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
mutexprof/                independent module: example.com/mutexprof
  go.mod
  mutexprof.go            LockedCounter, AtomicCounter, Hammer, Profile, MutexCount
  cmd/demo/main.go        hammer the locked counter under profiling, show it names the lock
  mutexprof_test.go       locked path is profiled; atomic path is absent from the profile
```

- Files: `mutexprof.go`, `cmd/demo/main.go`, `mutexprof_test.go`.
- Implement: a `LockedCounter` (mutex-guarded) and an `AtomicCounter`; `Hammer(writers, perWriter, inc)`; `Profile(fraction, fn)` that calls `runtime.SetMutexProfileFraction(fraction)`, runs `fn`, captures `pprof.Lookup("mutex")` as text, then restores the previous fraction; `MutexCount()`.
- Test: under fraction=1 the locked hammer yields `MutexCount() > 0` and a profile naming `LockedCounter`; the atomic hammer's profile does not name `AtomicCounter`.
- Verify: `go test -count=1 -race ./...`

### What the mutex profile records

The mutex profile samples lock *contention*: when a goroutine has to wait because
another holds the lock, the runtime records the stack at the point the holder
*unlocked* — the critical section that made others wait. That is the useful framing:
the profile points at the code that holds the hot lock, not merely at the callers who
queued behind it. Like the block profile it is off by default;
`runtime.SetMutexProfileFraction(n)` enables it by sampling `1/n` of contention
events (`1` samples all, `0` disables) and returns the previous fraction, so
`Profile` restores it rather than blindly zeroing a value another part of the program
may have set.

`Hammer` runs `writers` goroutines that each call `inc` `perWriter` times. Passed a
`LockedCounter`'s method value bound to a single shared counter, all writers contend
on one mutex — the reproduction of a hot lock. Passed an `AtomicCounter`'s method
value, the same workload uses `atomic.Int64.Add` and never blocks on a mutex, so it
never appears in the mutex profile. That contrast is the lesson: reading the profile
localizes the lock, and the fix (a sharded or atomic counter) is confirmed by the
lock's disappearance from the profile.

Because the mutex profile is process-global and cumulative, these tests do not run in
parallel with each other, and the atomic assertion is phrased as "the atomic
function is *absent* from the profile" rather than a fragile count comparison — the
atomic path adds no records naming it, whatever earlier tests recorded.

Create `mutexprof.go`:

```go
package mutexprof

import (
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
)

// LockedCounter is guarded by a single mutex; under many concurrent writers the
// mutex is the contended critical section.
type LockedCounter struct {
	mu sync.Mutex
	n  int64
}

func (c *LockedCounter) Inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *LockedCounter) Value() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// AtomicCounter is the contention-free alternative: no lock to profile.
type AtomicCounter struct {
	n atomic.Int64
}

func (c *AtomicCounter) Inc()         { c.n.Add(1) }
func (c *AtomicCounter) Value() int64 { return c.n.Load() }

// Hammer runs writers goroutines, each calling inc perWriter times.
func Hammer(writers, perWriter int, inc func()) {
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWriter {
				inc()
			}
		}()
	}
	wg.Wait()
}

// Profile enables mutex profiling at fraction (1 samples every contention event),
// runs fn, captures the mutex profile as text (debug=1), then restores the prior
// fraction. It returns the profile text.
func Profile(fraction int, fn func()) string {
	prev := runtime.SetMutexProfileFraction(fraction)
	defer runtime.SetMutexProfileFraction(prev)
	fn()
	var b strings.Builder
	_ = pprof.Lookup("mutex").WriteTo(&b, 1)
	return b.String()
}

// MutexCount reports the number of distinct contended stacks recorded so far.
func MutexCount() int {
	return pprof.Lookup("mutex").Count()
}
```

### The runnable demo

The demo hammers a shared `LockedCounter` with 50 writers under mutex profiling,
prints the final count (deterministic: `writers * perWriter`), that the profile
recorded contention, and that its text names the guarded type.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/mutexprof"
)

func main() {
	lc := &mutexprof.LockedCounter{}
	prof := mutexprof.Profile(1, func() {
		mutexprof.Hammer(50, 2000, lc.Inc)
	})

	fmt.Println("final count:", lc.Value())
	fmt.Println("mutex samples > 0:", mutexprof.MutexCount() > 0)
	fmt.Println("names locked counter:", strings.Contains(prof, "LockedCounter"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
final count: 100000
mutex samples > 0: true
names locked counter: true
```

### Tests

`TestMutexProfileLocalizesLock` hammers the shared locked counter and asserts the
profile recorded contention and names `LockedCounter`, plus that the counter's final
value is correct (the mutex actually serialized the increments).
`TestAtomicCounterAbsentFromProfile` runs the same workload on the atomic counter and
asserts its type does not appear in the mutex profile. Neither is parallel: they
toggle the global profiling fraction.

Create `mutexprof_test.go`:

```go
package mutexprof

import (
	"fmt"
	"strings"
	"testing"
)

func TestMutexProfileLocalizesLock(t *testing.T) {
	lc := &LockedCounter{}
	prof := Profile(1, func() {
		Hammer(50, 2000, lc.Inc)
	})

	if lc.Value() != 50*2000 {
		t.Fatalf("counter = %d; want %d", lc.Value(), 50*2000)
	}
	if MutexCount() == 0 {
		t.Fatal("mutex profile has no records; expected lock contention")
	}
	if !strings.Contains(prof, "LockedCounter") {
		t.Errorf("mutex profile does not name the contended lock:\n%s", prof)
	}
}

func TestAtomicCounterAbsentFromProfile(t *testing.T) {
	ac := &AtomicCounter{}
	prof := Profile(1, func() {
		Hammer(50, 2000, ac.Inc)
	})

	if ac.Value() != 50*2000 {
		t.Fatalf("counter = %d; want %d", ac.Value(), 50*2000)
	}
	if strings.Contains(prof, "AtomicCounter") {
		t.Error("atomic counter must not appear in the mutex profile")
	}
}

func ExampleAtomicCounter() {
	c := &AtomicCounter{}
	c.Inc()
	c.Inc()
	fmt.Println(c.Value())
	// Output: 2
}
```

## Review

The profile is correct when the contended lock shows up under the mutex-guarded
workload and vanishes under the atomic one. The two counters make the diagnosis
actionable: reading the profile names `LockedCounter.Inc` as the hot critical
section, and the fix is confirmed not by a smaller number but by `AtomicCounter`
being entirely absent from the profile — no lock, nothing to sample. Two switches
matter: mutex profiling must be enabled before the workload or the profile is
misleadingly empty, and `SetMutexProfileFraction` returns the prior fraction so you
restore rather than clobber it. The final-value assertions double as a correctness
check that the mutex actually serialized the increments. Run `go test -race` to
confirm both counters are race-free.

## Resources

- [`runtime.SetMutexProfileFraction`](https://pkg.go.dev/runtime#SetMutexProfileFraction) — enabling mutex profiling; the fraction and the returned previous value.
- [`runtime/pprof.Lookup`](https://pkg.go.dev/runtime/pprof#Lookup) — reading the `mutex` profile.
- [`sync/atomic.Int64`](https://pkg.go.dev/sync/atomic#Int64) — the lock-free counter the fix uses.

---

Prev: [05-block-profile-channel-contention.md](05-block-profile-channel-contention.md) | Back to [00-concepts.md](00-concepts.md) | Next: [07-diagnose-wedged-worker-pool.md](07-diagnose-wedged-worker-pool.md)
