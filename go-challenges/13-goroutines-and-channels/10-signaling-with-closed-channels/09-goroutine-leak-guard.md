# Exercise 9: Proving Shutdown Terminates Every Goroutine (No Leak)

Every close-to-signal design in this lesson is only correct if the receivers
actually exit. This exercise codifies the operational rule: pair each broadcast
with a *proof* of termination. It builds a manager that parks `K` goroutines on a
shared closed-channel signal, and a test harness that proves they all exit — the
reliable way (a `WaitGroup`) and the flaky secondary way (`runtime.NumGoroutine`
polled in a bounded settle loop), with a clear account of why the first is the
contract and the second is only a guard.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
leakguard/                   independent module: example.com/leakguard
  go.mod                     go mod init example.com/leakguard
  manager.go                 type Manager; New, Start(k), Shutdown, Active
  cmd/
    demo/
      main.go                runnable demo: start K, shutdown, settle to baseline
  manager_test.go            WaitGroup contract + NumGoroutine secondary guard
```

Files: `manager.go`, `cmd/demo/main.go`, `manager_test.go`.
Implement: a `Manager` that starts `K` goroutines each parked on a shared `quit chan struct{}`; `Shutdown()` closes `quit` once and blocks on a `WaitGroup` until all exit. A test helper polls `runtime.NumGoroutine` with a bounded backoff until it settles to baseline.
Test: capture a baseline; start `K` workers; `Shutdown`; assert the `WaitGroup` returned (primary) and `NumGoroutine` returned to baseline (secondary, bounded retry).
Verify: `go test -count=1 -race ./...`

### Why WaitGroup is the contract and NumGoroutine is not

When `Shutdown()` calls `close(quit)`, every parked worker's `<-quit` becomes
ready and each returns, running its `defer wg.Done()`. `wg.Wait()` returning is a
*hard* guarantee: it cannot return until every `Add` has been matched by a `Done`,
so it is a synchronization point that provably follows every worker's exit. That
is the termination contract. Assert on it and the test is deterministic.

`runtime.NumGoroutine()` is different. It reports a live count, but the scheduler
reclaims an exited goroutine *asynchronously* — a goroutine that has run its last
statement may still be counted for a short window. So:

```go
// Flaky: races the scheduler.
m.Shutdown()
if runtime.NumGoroutine() != baseline {
	t.Fatal("leak")
}
```

can fail even when there is no leak, because the workers finished but have not yet
been reaped. The honest way to use `NumGoroutine` is as a *secondary* guard inside
a bounded settle loop: poll it, yield with `runtime.Gosched()`, sleep a
millisecond, and give up after a fixed number of attempts. Even then it is a
sanity check, not the contract — the `WaitGroup` is what actually proves no leak.
The `NumGoroutine` guard catches a different class of bug: a goroutine that the
`WaitGroup` does not track at all (someone forgot the `Add`), which `wg.Wait()`
alone would silently miss.

Set up the module:

```bash
mkdir -p ~/go-exercises/leakguard/cmd/demo
cd ~/go-exercises/leakguard
go mod init example.com/leakguard
```

Create `manager.go`:

```go
package leakguard

import (
	"sync"
)

// Manager starts a set of goroutines that all park on a shared quit channel and
// exit together when Shutdown closes it.
type Manager struct {
	quit chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

// New returns a ready Manager.
func New() *Manager {
	return &Manager{quit: make(chan struct{})}
}

// Start launches k goroutines, each parked on the shared quit channel. A single
// close(quit) in Shutdown releases all of them.
func (m *Manager) Start(k int) {
	for range k {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			<-m.quit
		}()
	}
}

// Shutdown broadcasts stop with a single close and blocks until every started
// goroutine has exited. It is idempotent.
func (m *Manager) Shutdown() {
	m.once.Do(func() { close(m.quit) })
	m.wg.Wait()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime"
	"time"

	"example.com/leakguard"
)

func main() {
	before := runtime.NumGoroutine()

	m := leakguard.New()
	m.Start(20)
	during := runtime.NumGoroutine()

	m.Shutdown()

	// Settle: NumGoroutine reclaims exited goroutines asynchronously, so poll.
	for range 100 {
		if runtime.NumGoroutine() <= before {
			break
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	after := runtime.NumGoroutine()

	fmt.Printf("workers raised goroutine count: %v\n", during > before)
	fmt.Printf("back to baseline after shutdown: %v\n", after <= before)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
workers raised goroutine count: true
back to baseline after shutdown: true
```

### Tests

These tests deliberately do not call `t.Parallel()`: `runtime.NumGoroutine` is a
process-global count, so a parallel sibling would inflate it and make the
secondary guard flaky. The primary `WaitGroup` contract is unaffected by that, but
keeping the whole file serial makes the `NumGoroutine` assertion meaningful.

Create `manager_test.go`:

```go
package leakguard

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// waitForBaseline is the bounded settle loop: it polls NumGoroutine until it is
// at or below baseline, yielding and sleeping between attempts, and fails after a
// fixed number of tries. It is a secondary guard, not the primary contract.
func waitForBaseline(t *testing.T, baseline int) {
	t.Helper()
	for range 200 {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("goroutines did not settle to baseline %d; still %d", baseline, runtime.NumGoroutine())
}

func TestShutdownTerminatesAllViaWaitGroup(t *testing.T) {
	// Primary contract: Shutdown blocks on wg.Wait, so its return proves every
	// one of the 50 workers exited. If close reached fewer than all, this hangs.
	m := New()
	m.Start(50)
	m.Shutdown()
}

func TestNoGoroutineLeakSecondaryGuard(t *testing.T) {
	runtime.GC()
	baseline := runtime.NumGoroutine()

	m := New()
	m.Start(30)
	m.Shutdown() // WaitGroup contract: all 30 have exited here.

	// Secondary guard: confirm the runtime reclaimed them back to baseline.
	waitForBaseline(t, baseline)
}

func TestShutdownIdempotent(t *testing.T) {
	m := New()
	m.Start(10)
	m.Shutdown()
	m.Shutdown() // second close guarded by sync.Once; must not panic
}

func ExampleManager() {
	m := New()
	m.Start(5)
	m.Shutdown()
	fmt.Println("all workers stopped")
	// Output: all workers stopped
}
```

## Review

The manager is correct when `Shutdown()` provably terminates every worker: the
`WaitGroup` makes `Shutdown()` return only after all `K` workers have exited, so
`TestShutdownTerminatesAllViaWaitGroup` would hang rather than pass if the close
reached fewer than all. Treat that `WaitGroup` completion as the contract and
`runtime.NumGoroutine` as a bounded-retry secondary check — asserting on
`NumGoroutine` immediately after `Shutdown()` races the scheduler and flakes,
which is exactly the mistake `waitForBaseline` avoids. The operational takeaway
for every close-to-signal design in this lesson: pair the broadcast with a proof
of receiver termination, and run it under `go test -race` to catch an
unsynchronized close.

## Resources

- [pkg.go.dev: runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine) — the live count, reclaimed asynchronously.
- [pkg.go.dev: sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — the reliable termination contract.
- [pkg.go.dev: runtime.Gosched](https://pkg.go.dev/runtime#Gosched) — yielding the processor inside the settle loop.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-admission-drain-gate-load-shedding.md](10-admission-drain-gate-load-shedding.md)
