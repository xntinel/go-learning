# Exercise 4: Atomic Value for Dynamic Configuration

Production services need to change configuration without restarting: update rate
limits, rotate database connection strings, flip feature flags, adjust log
levels. The standard shape is a background goroutine that watches a config
source and swaps the active configuration when it changes. The catch is that a
real config is a whole struct -- strings, durations, booleans, nested fields --
and it has to swap as a unit so no handler ever reads a half-old, half-new
value. This exercise builds that hot-reload with `atomic.Value`, which stores
and loads an entire struct atomically, with no lock and no contention between
readers.

## What you'll build

```text
04-atomic-value-dynamic-config/
  main.go        store/load a config struct, a live-reload watcher,
                 validation with rollback, and a full hot-reload system
```

- Build: a lock-free hot-reloadable configuration system backed by `atomic.Value`.
- Implement: a `ConfigHolder` with `Get`/`Swap`; a watcher goroutine that swaps config while handlers `Load` per request; a validating manager that rejects bad configs and preserves the last good one; a `HotConfigReloader` with change listeners.
- Verify: `go run main.go`, and `go run -race main.go` for the concurrent steps.

### Why a whole struct needs atomic.Value

The atomic functions from previous exercises (`AddInt64`, `LoadInt32`,
`CompareAndSwapInt64`) only operate on primitive numeric types. A real config
struct has strings, durations, booleans, nested structs. You need to swap the
entire struct at once so that no handler goroutine ever sees a half-old,
half-new configuration.

`atomic.Value` stores and loads `any` values atomically. A writer prepares a
complete new config struct, calls `Store()`, and every subsequent `Load()` by
any handler goroutine sees the new value immediately. There is no lock, no
contention between readers, and the swap is instantaneous. Readers never block
each other and never block the writer.

## Step 1 -- Store and Load a Config Struct

The fundamental operations: store an initial config, load it back, swap it with a new version:

```go
package main

import (
	"fmt"
	"sync/atomic"
	"time"
)

type ServiceConfig struct {
	ListenAddr     string
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	MaxConnections int
	LogLevel       string
	DatabaseURL    string
}

type ConfigHolder struct {
	value atomic.Value
}

func NewConfigHolder(initial ServiceConfig) *ConfigHolder {
	ch := &ConfigHolder{}
	ch.value.Store(initial)
	return ch
}

func (ch *ConfigHolder) Get() ServiceConfig {
	return ch.value.Load().(ServiceConfig)
}

func (ch *ConfigHolder) Swap(newCfg ServiceConfig) ServiceConfig {
	return ch.value.Swap(newCfg).(ServiceConfig)
}

func printConfigSummary(cfg ServiceConfig) {
	fmt.Printf("Listen: %s, MaxConns: %d, LogLevel: %s\n",
		cfg.ListenAddr, cfg.MaxConnections, cfg.LogLevel)
}

func printConfigTransition(old, current ServiceConfig) {
	fmt.Printf("Old listen: %s -> New listen: %s\n", old.ListenAddr, current.ListenAddr)
	fmt.Printf("Old maxConns: %d -> New maxConns: %d\n", old.MaxConnections, current.MaxConnections)
}

func main() {
	holder := NewConfigHolder(ServiceConfig{
		ListenAddr:     ":8080",
		ReadTimeout:    5 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxConnections: 100,
		LogLevel:       "info",
		DatabaseURL:    "postgres://localhost:5432/myapp",
	})

	printConfigSummary(holder.Get())

	old := holder.Swap(ServiceConfig{
		ListenAddr:     ":9090",
		ReadTimeout:    3 * time.Second,
		WriteTimeout:   5 * time.Second,
		MaxConnections: 500,
		LogLevel:       "debug",
		DatabaseURL:    "postgres://primary.db:5432/myapp",
	})

	printConfigTransition(old, holder.Get())
}
```

### Verification
```bash
go run main.go
```
Expected output shows the initial config and the swapped values.

## Step 2 -- Config Watcher with Live Reload

A watcher goroutine detects config changes and swaps the active config. Handler goroutines load the current config on every request. Readers never block each other and never block the writer:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	handlerCount       = 4
	requestsPerHandler = 12
	reloadIntervalMs   = 50
	requestIntervalMs  = 20
)

type ServiceConfig struct {
	RateLimit      int
	MaxConnections int
	LogLevel       string
	MaintenanceMsg string
}

type ConfigManager struct {
	current atomic.Value
}

func NewConfigManager(initial ServiceConfig) *ConfigManager {
	cm := &ConfigManager{}
	cm.current.Store(initial)
	return cm
}

func (cm *ConfigManager) Get() ServiceConfig {
	return cm.current.Load().(ServiceConfig)
}

func (cm *ConfigManager) Update(newConfig ServiceConfig) ServiceConfig {
	return cm.current.Swap(newConfig).(ServiceConfig)
}

type ConfigWatcher struct {
	manager *ConfigManager
	updates []ServiceConfig
}

func NewConfigWatcher(manager *ConfigManager, updates []ServiceConfig) *ConfigWatcher {
	return &ConfigWatcher{manager: manager, updates: updates}
}

func (w *ConfigWatcher) Run(wg *sync.WaitGroup) {
	defer wg.Done()
	for i, cfg := range w.updates {
		time.Sleep(reloadIntervalMs * time.Millisecond)
		old := w.manager.Update(cfg)
		fmt.Printf("[watcher] reload %d: rateLimit %d->%d, logLevel %s->%s\n",
			i+1, old.RateLimit, cfg.RateLimit, old.LogLevel, cfg.LogLevel)
	}
}

func routeRequest(handlerID, reqNum int, cfg ServiceConfig) {
	action := "200 OK"
	if cfg.MaintenanceMsg != "" {
		action = fmt.Sprintf("503 %s", cfg.MaintenanceMsg)
	}
	fmt.Printf("[handler %d req %02d] rateLimit=%d logLevel=%s -> %s\n",
		handlerID, reqNum, cfg.RateLimit, cfg.LogLevel, action)
}

func runHandler(handlerID int, manager *ConfigManager, wg *sync.WaitGroup) {
	defer wg.Done()
	for req := 0; req < requestsPerHandler; req++ {
		routeRequest(handlerID, req, manager.Get())
		time.Sleep(requestIntervalMs * time.Millisecond)
	}
}

func main() {
	manager := NewConfigManager(ServiceConfig{
		RateLimit:      100,
		MaxConnections: 50,
		LogLevel:       "info",
		MaintenanceMsg: "",
	})

	watcher := NewConfigWatcher(manager, []ServiceConfig{
		{RateLimit: 200, MaxConnections: 100, LogLevel: "info", MaintenanceMsg: ""},
		{RateLimit: 200, MaxConnections: 100, LogLevel: "debug", MaintenanceMsg: "Deploying v2.1"},
		{RateLimit: 500, MaxConnections: 200, LogLevel: "info", MaintenanceMsg: ""},
	})

	var wg sync.WaitGroup

	wg.Add(1)
	go watcher.Run(&wg)

	for i := 0; i < handlerCount; i++ {
		wg.Add(1)
		go runHandler(i, manager, &wg)
	}

	wg.Wait()
}
```

### Verification
```bash
go run -race main.go
```
Handlers see config changes take effect in real time. During the maintenance window, handlers respond 503. After the final reload, handlers see the new rate limit. No race warnings.

## Step 3 -- Config Validation and Rollback

In production, invalid configs should be rejected. The previous valid config is preserved:

```go
package main

import (
	"fmt"
	"sync/atomic"
	"time"
)

const minReadTimeout = time.Millisecond

type ServiceConfig struct {
	ListenAddr     string
	MaxConnections int
	RateLimit      int
	ReadTimeout    time.Duration
}

func (c ServiceConfig) Validate() error {
	if c.MaxConnections < 1 {
		return fmt.Errorf("MaxConnections must be positive, got %d", c.MaxConnections)
	}
	if c.RateLimit < 1 {
		return fmt.Errorf("RateLimit must be positive, got %d", c.RateLimit)
	}
	if c.ReadTimeout < minReadTimeout {
		return fmt.Errorf("ReadTimeout too small: %v", c.ReadTimeout)
	}
	if c.ListenAddr == "" {
		return fmt.Errorf("ListenAddr cannot be empty")
	}
	return nil
}

type ValidatingConfigManager struct {
	current atomic.Value
	version int
}

func NewValidatingConfigManager(initial ServiceConfig) *ValidatingConfigManager {
	cm := &ValidatingConfigManager{version: 1}
	cm.current.Store(initial)
	return cm
}

func (cm *ValidatingConfigManager) Get() ServiceConfig {
	return cm.current.Load().(ServiceConfig)
}

func (cm *ValidatingConfigManager) Reload(cfg ServiceConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cm.version++
	cm.current.Store(cfg)
	fmt.Printf("[config] v%d loaded: listen=%s maxConns=%d rateLimit=%d\n",
		cm.version, cfg.ListenAddr, cfg.MaxConnections, cfg.RateLimit)
	return nil
}

func tryReload(mgr *ValidatingConfigManager, cfg ServiceConfig, label string) {
	err := mgr.Reload(cfg)
	fmt.Printf("%s error: %v\n", label, err)
	current := mgr.Get()
	fmt.Printf("Current: listen=%s maxConns=%d\n\n", current.ListenAddr, current.MaxConnections)
}

func main() {
	mgr := NewValidatingConfigManager(ServiceConfig{
		ListenAddr:     ":8080",
		MaxConnections: 100,
		RateLimit:      200,
		ReadTimeout:    5 * time.Second,
	})

	fmt.Printf("Initial: %+v\n\n", mgr.Get())

	tryReload(mgr, ServiceConfig{
		ListenAddr:     ":9090",
		MaxConnections: 500,
		RateLimit:      1000,
		ReadTimeout:    3 * time.Second,
	}, "Valid reload")

	tryReload(mgr, ServiceConfig{
		ListenAddr:     ":9090",
		MaxConnections: -1,
		RateLimit:      1000,
		ReadTimeout:    3 * time.Second,
	}, "Invalid reload (negative maxConns)")

	tryReload(mgr, ServiceConfig{
		ListenAddr:     "",
		MaxConnections: 100,
		RateLimit:      500,
		ReadTimeout:    3 * time.Second,
	}, "Invalid reload (empty addr)")
}
```

### Verification
```bash
go run main.go
```
Valid config loads succeed. Invalid configs are rejected with descriptive errors. The active config remains unchanged after a rejected reload.

## Step 4 -- Full Hot-Reload System with File Simulation

A complete hot-reload system: initial config, periodic watcher, concurrent handlers, graceful config transitions. Uses `atomic.Value` to ensure handlers always see a consistent config snapshot:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	handlerCount       = 3
	requestsPerHandler = 10
	watchIntervalMs    = 40
	requestIntervalMs  = 15
)

type ChangeListener func(old, updated AppConfig)

type AppConfig struct {
	Version       int
	DatabasePool  int
	CacheEnabled  bool
	CacheTTL      time.Duration
	RateLimitRPS  int
	AllowedOrigin string
}

func (c AppConfig) String() string {
	return fmt.Sprintf("v%d pool=%d cache=%v cacheTTL=%v rps=%d origin=%s",
		c.Version, c.DatabasePool, c.CacheEnabled,
		c.CacheTTL, c.RateLimitRPS, c.AllowedOrigin)
}

type HotConfigReloader struct {
	config    atomic.Value
	listeners []ChangeListener
}

func NewHotConfigReloader(initial AppConfig) *HotConfigReloader {
	hc := &HotConfigReloader{}
	hc.config.Store(initial)
	return hc
}

func (hc *HotConfigReloader) Get() AppConfig {
	return hc.config.Load().(AppConfig)
}

func (hc *HotConfigReloader) OnChange(fn ChangeListener) {
	hc.listeners = append(hc.listeners, fn)
}

func (hc *HotConfigReloader) Swap(newCfg AppConfig) {
	old := hc.config.Swap(newCfg).(AppConfig)
	for _, fn := range hc.listeners {
		fn(old, newCfg)
	}
}

func logConfigChanges(old, updated AppConfig) {
	fmt.Printf("[event] config changed: v%d -> v%d\n", old.Version, updated.Version)
	if old.DatabasePool != updated.DatabasePool {
		fmt.Printf("[event]   pool size: %d -> %d\n", old.DatabasePool, updated.DatabasePool)
	}
	if old.CacheEnabled != updated.CacheEnabled {
		fmt.Printf("[event]   cache: %v -> %v\n", old.CacheEnabled, updated.CacheEnabled)
	}
}

func runConfigWatcher(reloader *HotConfigReloader, revisions []AppConfig, wg *sync.WaitGroup) {
	defer wg.Done()
	for _, rev := range revisions {
		time.Sleep(watchIntervalMs * time.Millisecond)
		reloader.Swap(rev)
	}
}

func runRequestHandler(handlerID int, reloader *HotConfigReloader, wg *sync.WaitGroup) {
	defer wg.Done()
	for req := 0; req < requestsPerHandler; req++ {
		fmt.Printf("[handler %d req %02d] %s\n", handlerID, req, reloader.Get())
		time.Sleep(requestIntervalMs * time.Millisecond)
	}
}

func main() {
	reloader := NewHotConfigReloader(AppConfig{
		Version:       1,
		DatabasePool:  10,
		CacheEnabled:  false,
		CacheTTL:      0,
		RateLimitRPS:  100,
		AllowedOrigin: "https://app.example.com",
	})

	reloader.OnChange(logConfigChanges)

	revisions := []AppConfig{
		{Version: 2, DatabasePool: 20, CacheEnabled: true, CacheTTL: 5 * time.Minute,
			RateLimitRPS: 100, AllowedOrigin: "https://app.example.com"},
		{Version: 3, DatabasePool: 20, CacheEnabled: true, CacheTTL: 10 * time.Minute,
			RateLimitRPS: 500, AllowedOrigin: "https://app.example.com"},
		{Version: 4, DatabasePool: 50, CacheEnabled: true, CacheTTL: 10 * time.Minute,
			RateLimitRPS: 500, AllowedOrigin: "https://v2.example.com"},
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go runConfigWatcher(reloader, revisions, &wg)

	for i := 0; i < handlerCount; i++ {
		wg.Add(1)
		go runRequestHandler(i, reloader, &wg)
	}

	wg.Wait()
	fmt.Printf("\n[final] %s\n", reloader.Get())
}
```

### Verification
```bash
go run -race main.go
```
Handlers see config transitions in real time. Change events fire on each swap. Each handler always sees a complete, consistent config snapshot -- never a mix of old and new fields. No race warnings.

## Verification

Run the race detector on each step:
```bash
go run -race main.go
```
All steps should pass with zero race warnings.

## Common Mistakes

### Mutating a Struct After Storing It

**Wrong:**
```go
package main

import (
	"fmt"
	"sync/atomic"
)

type Config struct {
	Port int
	Host string
}

func storeAndMutate(v *atomic.Value) {
	cfg := &Config{Port: 8080, Host: "localhost"}
	v.Store(cfg)
	cfg.Port = 9090 // BUG: mutates the stored value -- data race!
}

func main() {
	var v atomic.Value
	storeAndMutate(&v)
	fmt.Println(v.Load().(*Config).Port) // readers may see 8080 or 9090
}
```

**What happens:** Storing a pointer and then modifying the struct it points to is a data race. Readers see a partially modified struct.

**Fix:** Create a new struct for every update. Treat stored values as immutable:
```go
v.Store(&Config{Port: 9090, Host: "localhost"}) // new allocation
```

### Storing Different Concrete Types

**Wrong:**
```go
package main

import "sync/atomic"

func storeInconsistentTypes(v *atomic.Value) {
	v.Store("hello")
	v.Store(42) // PANIC: store of inconsistently typed value
}

func main() {
	var v atomic.Value
	storeInconsistentTypes(&v)
}
```

**Fix:** Always store the same concrete type. If you need flexibility, define an interface and always store that interface type.

### Loading Before Any Store

**Wrong:**
```go
package main

import "sync/atomic"

type Config struct{ Port int }

func loadBeforeStore(v *atomic.Value) Config {
	cfg := v.Load()    // returns nil -- no value stored yet
	return cfg.(Config) // PANIC: type assertion on nil
}

func main() {
	var config atomic.Value
	_ = loadBeforeStore(&config)
}
```

**Fix:** Always store an initial value before any goroutine calls Load, or check for nil:
```go
val := config.Load()
if val == nil { return }
cfg := val.(Config)
```

### Storing nil

**Wrong:**
```go
package main

import "sync/atomic"

func storeNilValue(v *atomic.Value) {
	v.Store(nil) // PANIC: store of nil value
}

func main() {
	var v atomic.Value
	storeNilValue(&v)
}
```

**Fix:** Use a zero-value struct or a pointer type as the stored type. If "no config" is a valid state, use a pointer: `v.Store((*Config)(nil))` after first storing a non-nil `*Config`.

## Review

`atomic.Value` stores and loads an arbitrary type atomically, which is exactly
what a config struct needs and what the numeric atomics -- `AddInt64`,
`LoadInt32` -- cannot give you, since they operate on single machine words. The
hot-reload shape is three moves: store an initial value, let a watcher goroutine
`Swap` in new ones, and have every handler `Load` per request. Two invariants
keep it safe. Every `Store` must carry the same concrete type, and storing `nil`
panics, so the stored value's type is fixed from the first write. And a stored
value must be treated as immutable -- mutating a struct after storing a pointer
to it is a data race, so each update allocates a fresh value. `Swap` returns the
previous value, which is what lets a validating manager reject a bad config and
keep the last good one while firing change listeners on every transition.
Because readers never block each other or the writer, it is ideal for read-heavy
server configuration.

You should be able to answer the four questions the exercise poses without
scrolling back. Why can't an `atomic.Int64` or `atomic.Bool` hold a multi-field
config -- what would tear if you spread the fields across several of them? What
happens if you `Load` before any `Store`, and why does the type assertion that
follows panic? Why is it critical never to modify a struct after storing a
pointer to it? And how does `atomic.Value` compare with a mutex under a
read-heavy workload -- where does the lock-free version win, and where does it
not?

## Resources
- [atomic.Value type](https://pkg.go.dev/sync/atomic#Value) -- the Store/Load/Swap API and its rules on consistent types and nil.
- [Go Memory Model](https://go.dev/ref/mem) -- the happens-before guarantees that make a stored value visible to every subsequent Load.
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- the broader philosophy behind immutable swaps over shared mutable state.

---

Back to [Concurrency](../../concurrency.md) | Next: [05-spinlock-with-atomic](../05-spinlock-with-atomic/05-spinlock-with-atomic.md)
