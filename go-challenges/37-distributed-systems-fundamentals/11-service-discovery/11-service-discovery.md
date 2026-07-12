# 11. Service Discovery

Service discovery solves the problem that arises when service addresses are not fixed at deploy time. A registry holds the live set of healthy instances; services register on startup, heartbeat to renew their TTL, and deregister on shutdown. Clients query the registry instead of a static config file and get back only the healthy instances they can use right now.

The hard parts are: TTL expiry must happen in the background without holding the registry lock continuously; health checking is IO-bound and must not block lookups; the watch API must deliver updates without losing events even if the subscriber is slow; and every error must be a named sentinel so callers can branch on it with `errors.Is`.

```text
servicediscovery/
  go.mod
  registry/
    registry.go
    registry_test.go
  cmd/demo/main.go
```

## Concepts

### Registration And TTL

A service instance registers by supplying a name, address, port, and a TTL duration. The registry stores a `lastSeen` timestamp for each instance. A background goroutine sweeps expired instances: any instance whose `lastSeen` is more than TTL in the past is removed and the change is broadcast to all active watch subscribers.

The client calls `Heartbeat` periodically (typically at TTL/2) to renew. If the service crashes without calling `Deregister`, the TTL ensures stale entries are collected automatically.

### Health Checking

Active health checking is distinct from TTL heartbeats. The registry periodically sends an HTTP GET to each instance's health endpoint. If the GET returns a non-200 status or times out, the instance is marked unhealthy after a configurable number of consecutive failures. After a configurable number of consecutive successes, it is returned to the live set.

`Lookup` always returns only instances where `healthy == true`. Health state is a field on `Instance`, not a separate map, so reads and writes happen under the same lock.

### The Watch API

A subscriber calls `Subscribe(service)` and receives a `<-chan []Instance` channel. Every time the live set for that service changes — an instance registers, deregisters, expires, or changes health state — the registry sends the new snapshot on all subscriber channels for that service.

The channel is buffered (capacity 1). If the subscriber is slow and has not drained the previous event, the registry drops the stale snapshot and writes the fresh one. This is safe because the channel carries a full snapshot, not a delta: a slow subscriber misses intermediate states but always converges to the latest.

### Client-Side Load Balancing

The registry returns a slice of `Instance`. The caller picks one. `RoundRobin` keeps an atomic counter and wraps it modulo the length of the live set on each call. Because `sync/atomic` operations are lock-free, multiple callers can advance the counter concurrently without contention.

### Why In-Process For This Lesson

A real service registry (Consul, etcd) runs as a separate process or cluster. This lesson implements the same logic in a single Go package so it can be tested with `go test -race` and `httptest` without any external dependency. The patterns — mutex-protected map, background goroutine, buffered watch channels — translate directly to a networked implementation.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/37-distributed-systems-fundamentals/11-service-discovery/11-service-discovery/registry
mkdir -p go-solutions/37-distributed-systems-fundamentals/11-service-discovery/11-service-discovery/cmd/demo
cd go-solutions/37-distributed-systems-fundamentals/11-service-discovery/11-service-discovery
```

This is a library with a demo command. Verification runs `go test -race ./...`; the demo is for inspection.

### Exercise 1: The Registry Package

Create `registry/registry.go`:

```go
package registry

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Sentinel errors returned by the registry.
var (
	ErrEmptyName     = errors.New("service name must not be empty")
	ErrEmptyAddr     = errors.New("instance address must not be empty")
	ErrInvalidPort   = errors.New("port must be between 1 and 65535")
	ErrInvalidTTL    = errors.New("TTL must be positive")
	ErrNotFound      = errors.New("service not found")
	ErrNotRegistered = errors.New("instance not registered")
)

// Instance is a single registered service endpoint.
type Instance struct {
	ID      string
	Service string
	Addr    string
	Port    int
	Tags    []string

	healthy   bool
	lastSeen  time.Time
	ttl       time.Duration
	healthURL string
	failures  int
	successes int
}

// Healthy reports whether the instance passed the most recent health check.
func (i Instance) Healthy() bool { return i.healthy }

// HealthURL returns the URL the registry polls for active health checking.
func (i Instance) HealthURL() string { return i.healthURL }

// Registry holds registered service instances and manages their lifecycle.
type Registry struct {
	mu          sync.RWMutex
	instances   map[string]*Instance
	byService   map[string]map[string]*Instance
	subscribers map[string][]chan []Instance

	hcClient    *http.Client
	hcThreshold int
	okThreshold int
}

// New creates a Registry. hcClient is used for active health checks; pass nil
// to use http.DefaultClient.
func New(hcClient *http.Client) *Registry {
	if hcClient == nil {
		hcClient = http.DefaultClient
	}
	return &Registry{
		instances:   make(map[string]*Instance),
		byService:   make(map[string]map[string]*Instance),
		subscribers: make(map[string][]chan []Instance),
		hcClient:    hcClient,
		hcThreshold: 3,
		okThreshold: 2,
	}
}

// Register adds an instance to the registry. id must be unique per instance.
func (r *Registry) Register(id, service, addr string, port int, ttl time.Duration, tags ...string) error {
	if service == "" {
		return fmt.Errorf("register: %w", ErrEmptyName)
	}
	if addr == "" {
		return fmt.Errorf("register: %w", ErrEmptyAddr)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("register: %w", ErrInvalidPort)
	}
	if ttl <= 0 {
		return fmt.Errorf("register: %w", ErrInvalidTTL)
	}

	inst := &Instance{
		ID:        id,
		Service:   service,
		Addr:      addr,
		Port:      port,
		Tags:      tags,
		healthy:   true,
		lastSeen:  time.Now(),
		ttl:       ttl,
		healthURL: fmt.Sprintf("http://%s:%d/health", addr, port),
	}

	r.mu.Lock()
	r.instances[id] = inst
	if r.byService[service] == nil {
		r.byService[service] = make(map[string]*Instance)
	}
	r.byService[service][id] = inst
	r.mu.Unlock()

	r.notify(service)
	return nil
}

// Heartbeat resets the TTL clock for an instance.
func (r *Registry) Heartbeat(id string) error {
	r.mu.Lock()
	inst, ok := r.instances[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("heartbeat: %w", ErrNotRegistered)
	}
	inst.lastSeen = time.Now()
	r.mu.Unlock()
	return nil
}

// Deregister removes an instance immediately.
func (r *Registry) Deregister(id string) error {
	r.mu.Lock()
	inst, ok := r.instances[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("deregister: %w", ErrNotRegistered)
	}
	svc := inst.Service
	delete(r.instances, id)
	delete(r.byService[svc], id)
	r.mu.Unlock()

	r.notify(svc)
	return nil
}

// Lookup returns healthy instances for the named service.
func (r *Registry) Lookup(service string) ([]Instance, error) {
	if service == "" {
		return nil, fmt.Errorf("lookup: %w", ErrEmptyName)
	}
	r.mu.RLock()
	svcMap, ok := r.byService[service]
	if !ok || len(svcMap) == 0 {
		r.mu.RUnlock()
		return nil, fmt.Errorf("lookup %q: %w", service, ErrNotFound)
	}
	out := make([]Instance, 0, len(svcMap))
	for _, inst := range svcMap {
		if inst.healthy {
			out = append(out, *inst)
		}
	}
	r.mu.RUnlock()
	if len(out) == 0 {
		return nil, fmt.Errorf("lookup %q: %w", service, ErrNotFound)
	}
	return out, nil
}

// Subscribe returns a channel that receives a fresh snapshot of healthy
// instances every time the live set for service changes. The channel is
// buffered (1); slow subscribers drop stale snapshots.
func (r *Registry) Subscribe(service string) <-chan []Instance {
	ch := make(chan []Instance, 1)
	r.mu.Lock()
	r.subscribers[service] = append(r.subscribers[service], ch)
	r.mu.Unlock()
	return ch
}

// notify sends a snapshot of healthy instances to all subscribers of service.
// Must be called without holding r.mu.
func (r *Registry) notify(service string) {
	r.mu.RLock()
	svcMap := r.byService[service]
	out := make([]Instance, 0, len(svcMap))
	for _, inst := range svcMap {
		if inst.healthy {
			out = append(out, *inst)
		}
	}
	subs := make([]chan []Instance, len(r.subscribers[service]))
	copy(subs, r.subscribers[service])
	r.mu.RUnlock()

	for _, ch := range subs {
		select {
		case ch <- out:
		default:
			// Channel full: drain the stale snapshot and write the fresh one.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- out:
			default:
			}
		}
	}
}

// ExpireOnce removes instances whose TTL has elapsed. Call from a ticker loop.
func (r *Registry) ExpireOnce() {
	now := time.Now()
	var expired []string
	r.mu.RLock()
	for id, inst := range r.instances {
		if now.Sub(inst.lastSeen) > inst.ttl {
			expired = append(expired, id)
		}
	}
	r.mu.RUnlock()

	services := map[string]struct{}{}
	r.mu.Lock()
	for _, id := range expired {
		if inst, ok := r.instances[id]; ok {
			services[inst.Service] = struct{}{}
			delete(r.byService[inst.Service], id)
			delete(r.instances, id)
		}
	}
	r.mu.Unlock()

	for svc := range services {
		r.notify(svc)
	}
}

// CheckHealthOnce runs one round of active HTTP health checks and updates
// each instance's healthy field. Pass a short-timeout client in tests.
func (r *Registry) CheckHealthOnce() {
	r.mu.RLock()
	snapshot := make([]*Instance, 0, len(r.instances))
	for _, inst := range r.instances {
		snapshot = append(snapshot, inst)
	}
	r.mu.RUnlock()

	type result struct {
		id  string
		ok  bool
		svc string
	}
	results := make([]result, 0, len(snapshot))
	for _, inst := range snapshot {
		resp, err := r.hcClient.Get(inst.healthURL)
		ok := err == nil && resp.StatusCode == http.StatusOK
		if err == nil {
			resp.Body.Close()
		}
		results = append(results, result{id: inst.ID, ok: ok, svc: inst.Service})
	}

	changed := map[string]struct{}{}
	r.mu.Lock()
	for _, res := range results {
		inst, exists := r.instances[res.id]
		if !exists {
			continue
		}
		prevHealthy := inst.healthy
		if res.ok {
			inst.failures = 0
			inst.successes++
			if inst.successes >= r.okThreshold {
				inst.healthy = true
			}
		} else {
			inst.successes = 0
			inst.failures++
			if inst.failures >= r.hcThreshold {
				inst.healthy = false
			}
		}
		if inst.healthy != prevHealthy {
			changed[res.svc] = struct{}{}
		}
	}
	r.mu.Unlock()

	for svc := range changed {
		r.notify(svc)
	}
}

// RoundRobin returns an instance selector that cycles through instances in
// order. It is safe for concurrent use via an atomic counter.
func RoundRobin() func([]Instance) (Instance, error) {
	var n uint64
	return func(list []Instance) (Instance, error) {
		if len(list) == 0 {
			return Instance{}, ErrNotFound
		}
		idx := int(atomic.AddUint64(&n, 1)-1) % len(list)
		return list[idx], nil
	}
}
```

The package is pure in-memory: no goroutines are started automatically. The caller drives TTL expiry and health checking by calling `ExpireOnce` and `CheckHealthOnce` from a ticker loop. This makes the package fully deterministic under test.

`RoundRobin` wraps an `atomic.AddUint64` counter. Subtracting 1 before the modulo gives the index for the current call. `len(list)` is checked first to avoid a divide-by-zero.

### Exercise 2: Tests

Create `registry/registry_test.go`:

```go
package registry

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestRegisterAndLookup(t *testing.T) {
	t.Parallel()

	r := New(nil)
	if err := r.Register("svc-1", "api", "127.0.0.1", 8080, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	instances, err := r.Lookup("api")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 {
		t.Fatalf("len = %d, want 1", len(instances))
	}
	if instances[0].ID != "svc-1" {
		t.Fatalf("ID = %q, want svc-1", instances[0].ID)
	}
}

func TestRegisterRejectsEmptyName(t *testing.T) {
	t.Parallel()

	r := New(nil)
	err := r.Register("id", "", "127.0.0.1", 8080, time.Second)
	if !errors.Is(err, ErrEmptyName) {
		t.Fatalf("err = %v, want ErrEmptyName", err)
	}
}

func TestRegisterRejectsEmptyAddr(t *testing.T) {
	t.Parallel()

	r := New(nil)
	err := r.Register("id", "api", "", 8080, time.Second)
	if !errors.Is(err, ErrEmptyAddr) {
		t.Fatalf("err = %v, want ErrEmptyAddr", err)
	}
}

func TestRegisterRejectsInvalidPort(t *testing.T) {
	t.Parallel()

	r := New(nil)
	for _, port := range []int{0, -1, 65536} {
		err := r.Register("id", "api", "127.0.0.1", port, time.Second)
		if !errors.Is(err, ErrInvalidPort) {
			t.Errorf("port %d: err = %v, want ErrInvalidPort", port, err)
		}
	}
}

func TestRegisterRejectsZeroTTL(t *testing.T) {
	t.Parallel()

	r := New(nil)
	err := r.Register("id", "api", "127.0.0.1", 8080, 0)
	if !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("err = %v, want ErrInvalidTTL", err)
	}
}

func TestDeregisterRemovesInstance(t *testing.T) {
	t.Parallel()

	r := New(nil)
	_ = r.Register("svc-1", "api", "127.0.0.1", 8080, 30*time.Second)
	if err := r.Deregister("svc-1"); err != nil {
		t.Fatal(err)
	}
	_, err := r.Lookup("api")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDeregisterUnknownReturnsNotRegistered(t *testing.T) {
	t.Parallel()

	r := New(nil)
	err := r.Deregister("does-not-exist")
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("err = %v, want ErrNotRegistered", err)
	}
}

func TestHeartbeatResetsExpiry(t *testing.T) {
	t.Parallel()

	ttl := 50 * time.Millisecond
	r := New(nil)

	// Register, let it expire without a heartbeat.
	_ = r.Register("svc-1", "api", "127.0.0.1", 8080, ttl)
	time.Sleep(ttl + 10*time.Millisecond)
	r.ExpireOnce()
	if _, err := r.Lookup("api"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after TTL, got %v", err)
	}

	// Register again and heartbeat before expiry.
	_ = r.Register("svc-2", "api2", "127.0.0.1", 8081, ttl)
	time.Sleep(ttl / 2)
	if err := r.Heartbeat("svc-2"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(ttl / 2)
	r.ExpireOnce()
	if _, err := r.Lookup("api2"); err != nil {
		t.Fatalf("expected instance alive after heartbeat, got %v", err)
	}
}

func TestTTLExpiry(t *testing.T) {
	t.Parallel()

	r := New(nil)
	ttl := 30 * time.Millisecond
	_ = r.Register("svc-1", "api", "127.0.0.1", 8080, ttl)

	time.Sleep(ttl + 10*time.Millisecond)
	r.ExpireOnce()

	_, err := r.Lookup("api")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound after TTL expiry", err)
	}
}

// testHealthServer returns an httptest.Server whose response code can be set
// dynamically via the returned pointer.
func testHealthServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	code := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv, &code
}

// parseHostPort splits "http://host:port" into host string and port int.
func parseHostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("bad URL %q: %v", rawURL, err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("bad host:port %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port %q: %v", portStr, err)
	}
	return host, port
}

func TestHealthCheckMarksUnhealthy(t *testing.T) {
	t.Parallel()

	srv, code := testHealthServer(t)
	*code = http.StatusServiceUnavailable

	r := New(srv.Client())
	r.hcThreshold = 1

	host, port := parseHostPort(t, srv.URL)
	_ = r.Register("svc-1", "api", host, port, 30*time.Second)
	r.mu.Lock()
	r.instances["svc-1"].healthURL = srv.URL
	r.mu.Unlock()

	r.CheckHealthOnce()

	_, err := r.Lookup("api")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (instance marked unhealthy)", err)
	}
}

func TestHealthCheckRestoresHealthy(t *testing.T) {
	t.Parallel()

	srv, code := testHealthServer(t)

	r := New(srv.Client())
	r.hcThreshold = 1
	r.okThreshold = 1

	host, port := parseHostPort(t, srv.URL)
	_ = r.Register("svc-1", "api", host, port, 30*time.Second)
	r.mu.Lock()
	r.instances["svc-1"].healthURL = srv.URL
	r.mu.Unlock()

	// First: mark unhealthy.
	*code = http.StatusServiceUnavailable
	r.CheckHealthOnce()
	if _, err := r.Lookup("api"); !errors.Is(err, ErrNotFound) {
		t.Fatal("expected unhealthy after 503")
	}

	// Then: restore healthy.
	*code = http.StatusOK
	r.CheckHealthOnce()
	instances, err := r.Lookup("api")
	if err != nil {
		t.Fatalf("expected healthy after 200, got %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("len = %d, want 1", len(instances))
	}
}

func TestSubscribeReceivesUpdates(t *testing.T) {
	t.Parallel()

	r := New(nil)
	ch := r.Subscribe("api")

	_ = r.Register("svc-1", "api", "127.0.0.1", 8080, 30*time.Second)

	select {
	case snap := <-ch:
		if len(snap) != 1 {
			t.Fatalf("snapshot len = %d, want 1", len(snap))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for register notification")
	}

	_ = r.Deregister("svc-1")

	select {
	case snap := <-ch:
		if len(snap) != 0 {
			t.Fatalf("snapshot len = %d, want 0 after deregister", len(snap))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for deregister notification")
	}
}

func TestLookupRejectsEmptyName(t *testing.T) {
	t.Parallel()

	r := New(nil)
	_, err := r.Lookup("")
	if !errors.Is(err, ErrEmptyName) {
		t.Fatalf("err = %v, want ErrEmptyName", err)
	}
}

func TestLookupUnknownServiceReturnsNotFound(t *testing.T) {
	t.Parallel()

	r := New(nil)
	_, err := r.Lookup("unknown")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRoundRobinCycles(t *testing.T) {
	t.Parallel()

	list := []Instance{
		{ID: "a", Service: "api", Addr: "127.0.0.1", Port: 8080},
		{ID: "b", Service: "api", Addr: "127.0.0.1", Port: 8081},
		{ID: "c", Service: "api", Addr: "127.0.0.1", Port: 8082},
	}
	pick := RoundRobin()
	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		inst, err := pick(list)
		if err != nil {
			t.Fatal(err)
		}
		seen[inst.ID]++
	}
	for _, id := range []string{"a", "b", "c"} {
		if seen[id] != 3 {
			t.Errorf("ID %q picked %d times, want 3", id, seen[id])
		}
	}
}

func TestRoundRobinEmptyReturnsNotFound(t *testing.T) {
	t.Parallel()

	pick := RoundRobin()
	_, err := pick(nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func ExampleRegistry_Lookup() {
	r := New(nil)
	_ = r.Register("api-1", "api", "127.0.0.1", 8080, 30*time.Second)
	instances, err := r.Lookup("api")
	if err != nil {
		panic(err)
	}
	fmt.Printf("found %d instance(s) of %q\n", len(instances), instances[0].Service)
	// Output: found 1 instance(s) of "api"
}
```

### Exercise 3: Demo Command

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/servicediscovery/registry"
)

func main() {
	r := registry.New(nil)

	// Register two instances.
	if err := r.Register("api-1", "api", "127.0.0.1", 8080, 30*time.Second, "v2"); err != nil {
		log.Fatal(err)
	}
	if err := r.Register("api-2", "api", "127.0.0.1", 8081, 30*time.Second, "v2"); err != nil {
		log.Fatal(err)
	}

	// Subscribe before changes.
	ch := r.Subscribe("api")

	// Lookup healthy instances.
	instances, err := r.Lookup("api")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("healthy instances: %d\n", len(instances))

	// Deregister one; subscriber receives update.
	if err := r.Deregister("api-1"); err != nil {
		log.Fatal(err)
	}
	snap := <-ch
	// Drain registration notification if present before deregister one.
	select {
	case snap = <-ch:
	default:
	}
	fmt.Printf("after deregister: %d healthy\n", len(snap))

	// Round-robin over remaining instances.
	pick := registry.RoundRobin()
	for i := 0; i < 3; i++ {
		remaining, err := r.Lookup("api")
		if err != nil {
			log.Fatal(err)
		}
		inst, err := pick(remaining)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  pick %d: %s:%d\n", i+1, inst.Addr, inst.Port)
	}
}
```

## Common Mistakes

### Holding The Lock While Notifying Subscribers

Wrong: calling `notify` inside an `r.mu.Lock()` block. If a subscriber channel is full and the drain-and-replace path does any work that also needs the lock, the goroutine deadlocks.

Fix: release the lock before calling `notify`. Collect the data you need under the lock into a local variable, unlock, then notify. `CheckHealthOnce` and `ExpireOnce` both follow this pattern.

### Returning A Snapshot With Pointer Values

Wrong: `out = append(out, inst)` where `inst` is `*Instance`. The caller holds a slice of pointers; a later health-check cycle mutates the pointed-to struct and the caller's snapshot changes silently.

Fix: dereference the pointer — `out = append(out, *inst)` — so the caller owns an independent copy. Every `Lookup` and `notify` in this lesson copies by value.

### Modulo On A Possibly-Zero Length

Wrong: `idx := counter % len(list)`. If the caller passes an empty slice, this panics with a divide-by-zero.

Fix: check `len(list) == 0` first and return `ErrNotFound`. `RoundRobin` does this.

### Using An Unbuffered Watch Channel

Wrong: `ch := make(chan []Instance)`. A send on an unbuffered channel blocks until the subscriber drains it. If the subscriber is slow, the registry stalls and holds the mutex.

Fix: buffer of 1, drop-and-replace for slow subscribers. The subscriber always converges to the latest snapshot even if it misses intermediate states. This lesson's `notify` uses this pattern.

## Verification

From `~/go-exercises/servicediscovery`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. Run the demo with:

```bash
go run ./cmd/demo
```

Your turn: add `TestMultipleServicesDoNotInterfere` — register instances for two different service names and assert that `Lookup("api")` does not return instances registered under `"worker"`.

## Summary

- A service registry stores live instances with TTL; the caller drives expiry via `ExpireOnce` on a ticker.
- Health checking is separate from heartbeating: heartbeats prove the instance is alive; health checks probe the service endpoint.
- `Lookup` returns only healthy instances; callers use helpers like `RoundRobin` to pick one.
- The watch channel carries full snapshots (not deltas) and is buffered so slow subscribers do not block the registry.
- Lock discipline: always unlock before notifying subscribers to avoid deadlock.

## What's Next

Next: [Distributed Rate Limiter](../12-distributed-rate-limiter/12-distributed-rate-limiter.md).

## Resources

- [Go sync package](https://pkg.go.dev/sync) — Mutex and RWMutex documentation
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — in-process HTTP servers for testing without network
- [sync/atomic](https://pkg.go.dev/sync/atomic) — lock-free counters used in RoundRobin
- [Consul architecture](https://developer.hashicorp.com/consul/docs/architecture) — production service discovery design
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) — channel vs mutex tradeoffs
