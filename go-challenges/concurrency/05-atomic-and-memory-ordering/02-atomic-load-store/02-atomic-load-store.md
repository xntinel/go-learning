# Exercise 2: Atomic Load and Store

Feature flags control behavior at runtime -- enable dark mode, route traffic to
a new API version, toggle rate limiting. A background reloader goroutine flips
them while every request handler reads them on every request. The trap is that a
plain write to a shared `bool` or `int32` in one goroutine carries no guarantee
of ever becoming visible in another: the reader can go on seeing the stale value
indefinitely, and the race detector will flag the access. This exercise builds a
feature-flag store with atomic load and store, so a flag the reloader sets is
seen the instant a handler reads it -- along with everything written before it.

## What you'll build

```text
02-atomic-load-store/
  main.go        feature flags updated by a reloader and read by handlers:
                 the stale-read bug, the atomic fix, and ordered stores
```

- Build: a runtime feature-flag store read by handlers and updated by a background reloader.
- Implement: the stale-read bug on plain `int32` fields, an `AtomicFlagStore` over `atomic.LoadInt32`/`StoreInt32`, a `FeatureFlags` type using `atomic.Bool` and `atomic.Int32`, and a `GatewayConfig` that stores the backend URL before the maintenance flag.
- Verify: `go run -race main.go`

### Why atomic load and store guarantee visibility

Without atomic operations, a write to a plain `bool` or `int32` in the reloader goroutine is NOT guaranteed to be visible in handler goroutines. The Go memory model allows the compiler to cache the value in a register, the CPU to reorder writes, or the store buffer to delay flushing. The result: handlers keep using the old flag value for an unpredictable amount of time. On weakly-ordered architectures (ARM), a handler might even see the flag as "enabled" while the associated config data is still the old version.

`atomic.StoreInt32` forces the write to be visible to all goroutines. `atomic.LoadInt32` forces a fresh read from memory. Together, they guarantee that when the reloader sets a flag to 1, any handler that loads the flag and sees 1 also sees all data written before the store.

## Step 1 -- The Stale Read Bug Without Atomics

A reloader goroutine updates a feature flag. Handler goroutines read it. Without atomics, handlers may never see the update:

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"time"
)

const (
	handlerCount    = 4
	requestsPerRun  = 20
	reloadDelayMs   = 50
	requestDelayMs  = 5
)

type UnsafeFlags struct {
	DarkModeEnabled int32
	BetaAPIEnabled  int32
	RateLimitPerSec int32
}

func (f *UnsafeFlags) ApplyUpdate() {
	// BUG: plain writes -- no visibility guarantee to other goroutines
	f.DarkModeEnabled = 1
	f.BetaAPIEnabled = 1
	f.RateLimitPerSec = 500
	fmt.Println("[reloader] Flags updated: darkMode=1, betaAPI=1, rateLimit=500")
}

func (f *UnsafeFlags) ReadFlags() (dm, ba, rl int32) {
	// BUG: plain reads -- may see stale values
	return f.DarkModeEnabled, f.BetaAPIEnabled, f.RateLimitPerSec
}

func runUnsafeReloader(flags *UnsafeFlags, wg *sync.WaitGroup) {
	defer wg.Done()
	time.Sleep(reloadDelayMs * time.Millisecond)
	flags.ApplyUpdate()
}

func runUnsafeHandler(handlerID int, flags *UnsafeFlags, wg *sync.WaitGroup) {
	defer wg.Done()
	for req := 0; req < requestsPerRun; req++ {
		dm, ba, rl := flags.ReadFlags()
		if req == requestsPerRun-1 {
			fmt.Printf("[handler %d] final read: darkMode=%d betaAPI=%d rateLimit=%d\n",
				handlerID, dm, ba, rl)
		}
		runtime.Gosched()
		time.Sleep(requestDelayMs * time.Millisecond)
	}
}

func main() {
	flags := &UnsafeFlags{}
	var wg sync.WaitGroup

	wg.Add(1)
	go runUnsafeReloader(flags, &wg)

	for i := 0; i < handlerCount; i++ {
		wg.Add(1)
		go runUnsafeHandler(i, flags, &wg)
	}

	wg.Wait()
}
```

### Verification
```bash
go run -race main.go
```
The race detector flags `DATA RACE` on the flag variables. On some runs, handlers may still show old values even after the reloader finishes. The code is broken per the Go memory model.

## Step 2 -- Fix with atomic.LoadInt32 and atomic.StoreInt32

Replace every plain read with `atomic.LoadInt32` and every plain write with `atomic.StoreInt32`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	handlerCount   = 4
	requestsPerRun = 20
	reloadDelayMs  = 50
	requestDelayMs = 5
)

type AtomicFlagStore struct {
	darkModeEnabled int32
	betaAPIEnabled  int32
	rateLimitPerSec int32
}

func (s *AtomicFlagStore) SetDarkMode(val int32)  { atomic.StoreInt32(&s.darkModeEnabled, val) }
func (s *AtomicFlagStore) SetBetaAPI(val int32)   { atomic.StoreInt32(&s.betaAPIEnabled, val) }
func (s *AtomicFlagStore) SetRateLimit(val int32)  { atomic.StoreInt32(&s.rateLimitPerSec, val) }

func (s *AtomicFlagStore) DarkMode() int32  { return atomic.LoadInt32(&s.darkModeEnabled) }
func (s *AtomicFlagStore) BetaAPI() int32   { return atomic.LoadInt32(&s.betaAPIEnabled) }
func (s *AtomicFlagStore) RateLimit() int32 { return atomic.LoadInt32(&s.rateLimitPerSec) }

func (s *AtomicFlagStore) ApplyUpdate() {
	s.SetDarkMode(1)
	s.SetBetaAPI(1)
	s.SetRateLimit(500)
	fmt.Println("[reloader] Flags updated: darkMode=1, betaAPI=1, rateLimit=500")
}

func runReloader(store *AtomicFlagStore, wg *sync.WaitGroup) {
	defer wg.Done()
	time.Sleep(reloadDelayMs * time.Millisecond)
	store.ApplyUpdate()
}

func runHandler(handlerID int, store *AtomicFlagStore, wg *sync.WaitGroup) {
	defer wg.Done()
	for req := 0; req < requestsPerRun; req++ {
		dm := store.DarkMode()
		ba := store.BetaAPI()
		rl := store.RateLimit()

		if req == requestsPerRun-1 {
			fmt.Printf("[handler %d] final read: darkMode=%d betaAPI=%d rateLimit=%d\n",
				handlerID, dm, ba, rl)
		}
		time.Sleep(requestDelayMs * time.Millisecond)
	}
}

func main() {
	store := &AtomicFlagStore{}
	var wg sync.WaitGroup

	wg.Add(1)
	go runReloader(store, &wg)

	for i := 0; i < handlerCount; i++ {
		wg.Add(1)
		go runHandler(i, store, &wg)
	}

	wg.Wait()
}
```

### Verification
```bash
go run -race main.go
```
No race warnings. All handlers see the updated values after the reloader runs.

## Step 3 -- Build a Complete Feature Flag System with atomic.Bool

Use Go 1.19+ typed wrappers (`atomic.Bool`, `atomic.Int32`) for a cleaner, production-style feature flag system:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultRateLimit  int32 = 100
	handlerCount            = 3
	requestsPerHandler      = 15
	reloadCycles            = 4
	reloadIntervalMs        = 40
	requestIntervalMs       = 15
)

type FeatureFlags struct {
	DarkMode    atomic.Bool
	BetaAPI     atomic.Bool
	NewCheckout atomic.Bool
	RateLimit   atomic.Int32
}

func NewFeatureFlags() *FeatureFlags {
	ff := &FeatureFlags{}
	ff.RateLimit.Store(defaultRateLimit)
	return ff
}

func (ff *FeatureFlags) Snapshot() string {
	return fmt.Sprintf("darkMode=%-5v betaAPI=%-5v newCheckout=%-5v rateLimit=%d",
		ff.DarkMode.Load(), ff.BetaAPI.Load(),
		ff.NewCheckout.Load(), ff.RateLimit.Load())
}

type FlagReloader struct {
	flags *FeatureFlags
}

func NewFlagReloader(flags *FeatureFlags) *FlagReloader {
	return &FlagReloader{flags: flags}
}

func (r *FlagReloader) ApplyUpdate(cycle int) {
	switch cycle {
	case 1:
		r.flags.DarkMode.Store(true)
		fmt.Println("[reloader] Enabled: dark mode")
	case 2:
		r.flags.BetaAPI.Store(true)
		r.flags.RateLimit.Store(500)
		fmt.Println("[reloader] Enabled: beta API, rate limit -> 500")
	case 3:
		r.flags.NewCheckout.Store(true)
		fmt.Println("[reloader] Enabled: new checkout flow")
	case 4:
		r.flags.BetaAPI.Store(false)
		r.flags.RateLimit.Store(defaultRateLimit)
		fmt.Println("[reloader] Disabled: beta API, rate limit -> 100")
	}
}

func (r *FlagReloader) RunReloadLoop(wg *sync.WaitGroup) {
	defer wg.Done()
	for cycle := 1; cycle <= reloadCycles; cycle++ {
		time.Sleep(reloadIntervalMs * time.Millisecond)
		r.ApplyUpdate(cycle)
	}
}

func handleRequest(handlerID, reqNum int, flags *FeatureFlags) {
	fmt.Printf("[handler %d req %02d] %s\n", handlerID, reqNum, flags.Snapshot())
}

func runRequestHandler(handlerID int, flags *FeatureFlags, wg *sync.WaitGroup) {
	defer wg.Done()
	for req := 0; req < requestsPerHandler; req++ {
		handleRequest(handlerID, req, flags)
		time.Sleep(requestIntervalMs * time.Millisecond)
	}
}

func main() {
	flags := NewFeatureFlags()
	reloader := NewFlagReloader(flags)
	var wg sync.WaitGroup

	wg.Add(1)
	go reloader.RunReloadLoop(&wg)

	for i := 0; i < handlerCount; i++ {
		wg.Add(1)
		go runRequestHandler(i, flags, &wg)
	}

	wg.Wait()
	fmt.Printf("\n[final state] %s\n", flags.Snapshot())
}
```

### Verification
```bash
go run -race main.go
```
Handlers see flag changes take effect in real time as the reloader applies them. The output shows the transition points where handlers start seeing new flag values. No race warnings.

## Step 4 -- Demonstrate Why Stale Reads Matter in Practice

Show a concrete bug: a handler checks a flag and then uses a value that was supposed to be updated together. Without atomics, the handler can see the flag as "enabled" but the associated config as the old value:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	initialBackendURL     = "https://api-v1.example.com"
	upgradedBackendURL    = "https://api-v2.example.com"
	maintenanceStartDelay = 30 * time.Millisecond
	maintenanceDuration   = 80 * time.Millisecond
	handlerCount          = 3
	requestsPerHandler    = 20
	requestInterval       = 10 * time.Millisecond
)

type GatewayConfig struct {
	maintenanceMode atomic.Bool
	backendURL      atomic.Value // stores string
}

func NewGatewayConfig() *GatewayConfig {
	gc := &GatewayConfig{}
	gc.backendURL.Store(initialBackendURL)
	return gc
}

func (gc *GatewayConfig) EnableMaintenance(newBackend string) {
	// Order matters: set the backend URL BEFORE enabling maintenance mode
	gc.backendURL.Store(newBackend)
	gc.maintenanceMode.Store(true)
	fmt.Println("[ops] Maintenance mode ON, backend switched to v2")
}

func (gc *GatewayConfig) DisableMaintenance() {
	gc.maintenanceMode.Store(false)
	fmt.Println("[ops] Maintenance mode OFF")
}

func (gc *GatewayConfig) IsMaintenanceMode() bool {
	return gc.maintenanceMode.Load()
}

func (gc *GatewayConfig) BackendURL() string {
	return gc.backendURL.Load().(string)
}

func runOpsWorkflow(config *GatewayConfig, wg *sync.WaitGroup) {
	defer wg.Done()
	time.Sleep(maintenanceStartDelay)
	config.EnableMaintenance(upgradedBackendURL)
	time.Sleep(maintenanceDuration)
	config.DisableMaintenance()
}

func routeRequest(handlerID, reqNum int, config *GatewayConfig) {
	if config.IsMaintenanceMode() {
		fmt.Printf("[handler %d req %02d] 503 Service Unavailable (maintenance)\n", handlerID, reqNum)
	} else {
		fmt.Printf("[handler %d req %02d] 200 OK -> %s\n", handlerID, reqNum, config.BackendURL())
	}
}

func runRequestHandler(handlerID int, config *GatewayConfig, wg *sync.WaitGroup) {
	defer wg.Done()
	for req := 0; req < requestsPerHandler; req++ {
		routeRequest(handlerID, req, config)
		time.Sleep(requestInterval)
	}
}

func main() {
	config := NewGatewayConfig()
	var wg sync.WaitGroup

	wg.Add(1)
	go runOpsWorkflow(config, &wg)

	for i := 0; i < handlerCount; i++ {
		wg.Add(1)
		go runRequestHandler(i, config, &wg)
	}

	wg.Wait()
}
```

### Verification
```bash
go run -race main.go
```
Handlers correctly show 503 during maintenance and route to v2 after maintenance ends. The atomic store of `backendURL` before `maintenanceMode` ensures that once handlers see maintenance=false, they also see the new backend URL. No race warnings.

## Verification

Run the race detector on each step:
```bash
go run -race main.go
```
Step 1 should report races. Steps 2-4 should be clean.

## Common Mistakes

### Reading a Flag Without Atomic After Writing With Atomic

**Wrong:**
```go
package main

import (
	"fmt"
	"sync/atomic"
)

func enableFeature(flag *int32) {
	atomic.StoreInt32(flag, 1)
}

func checkFeatureUnsafe(flag int32) {
	if flag == 1 { // BUG: non-atomic read -- data race
		fmt.Println("feature enabled")
	}
}

func main() {
	var enabled int32
	go enableFeature(&enabled)
	checkFeatureUnsafe(enabled)
}
```

**What happens:** The read is not synchronized. The race detector flags it, and on ARM the read may return stale data indefinitely.

**Fix:** Always pair `atomic.StoreInt32` with `atomic.LoadInt32`. If any goroutine uses atomic access, ALL goroutines must use atomic access for that variable.

### Busy-Waiting Without Yielding

**Wrong:**
```go
for atomic.LoadInt32(&flag) == 0 {} // tight loop, burns 100% CPU on one core
```

**Fix:** Add `runtime.Gosched()` to yield the processor, or use a channel or `time.Ticker` for polling. Busy-waiting with atomics is acceptable only in very short, performance-critical paths.

### Assuming Store Order Across Multiple Variables

**Subtlety:** When you store multiple flags, handlers might see them in any combination during the transition. If flag A and flag B must always be consistent with each other, use `atomic.Value` to swap the entire config struct atomically (covered in exercise 04).

## Review

An atomic store does two jobs at once: it makes the write indivisible, and --
the part that trips people up -- it makes the write visible. The Go memory model
gives no visibility guarantee for a plain shared read or write, so a handler
reading a non-atomic flag can keep seeing the old value indefinitely while the
reloader believes it published the new one; the race detector flags the access
precisely because the outcome is undefined. `atomic.StoreInt32` publishes the
write to every goroutine and `atomic.LoadInt32` forces a fresh read, and their
real power is the ordering they carry: a handler that loads the flag and sees
the new value is also guaranteed to see everything written before the store.
That is why the gateway must store `backendURL` before flipping
`maintenanceMode` -- so that once a handler observes maintenance is off, the new
backend is already visible. The typed wrappers `atomic.Bool` and `atomic.Int32`
(Go 1.19+) express all of this more cleanly than the function API.

Make sure you can answer the four questions the exercise leaves you. Atomicity
is about a read-modify-write being indivisible; visibility is about one
goroutine's write reaching another -- distinct problems that atomic operations
happen to solve together. A `time.Sleep` after a plain store is no substitute,
because it changes timing but grants no happens-before edge; the compiler and
CPU are still free to hide the write. The gateway stores `backendURL` before
`maintenanceMode` so the flag acts as the release that publishes the URL. And
you would reach for `atomic.Bool` over a channel when the flag is a single
shared value that many readers poll, where a channel's hand-off semantics would
be needless overhead.

## Resources
- [sync/atomic package](https://pkg.go.dev/sync/atomic) -- the full set of atomic load, store, add, and swap primitives, plus the typed wrappers.
- [Go Memory Model](https://go.dev/ref/mem) -- the happens-before rules that explain why plain reads see stale data and atomics do not.
- [atomic.Bool type](https://pkg.go.dev/sync/atomic#Bool) -- the Go 1.19+ typed boolean flag used in the feature-flag store.

---

Back to [Concurrency](../../concurrency.md) | Next: [03-atomic-compare-and-swap](../03-atomic-compare-and-swap/03-atomic-compare-and-swap.md)
