# Exercise 1: Configuration and Atomic Hot-Reload

Before any policy or listener exists, a data plane needs a source of truth: a validated configuration and a healthy backend pool it can route over, swappable at runtime without dropping requests. This module builds that core — config validation that reports every error at once, round-robin selection that skips unhealthy backends, and an atomic config update that readers never catch half-applied.

This module is fully self-contained. It defines its own configuration types, validation, backend pool, and selection logic in package `dataplane`, plus its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
config.go         Config, Cluster, Validate, sentinel errors
                  Backend: address + atomic health flag
                  DataPlane: backend pool, SelectBackend round-robin,
                             SetHealthy, UpdateConfig validate-then-swap
cmd/
  demo/
    main.go       build a plane, route round-robin, fail a backend, hot-reload
config_test.go    validation collects all errors; round-robin; skip unhealthy;
                  all-unhealthy error; atomic update takes effect immediately
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `Validate`, `New`, `SelectBackend`, `SetHealthy`, and `UpdateConfig`.
- Test: validation returns all errors via `errors.Join`; selection is round-robin and skips unhealthy backends; an all-unhealthy cluster returns an error; `UpdateConfig` swaps the pool and the next selection sees the new backends.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why validation collects every error, and why the swap is atomic

`Validate` returns `errors.Join` of every problem it finds rather than the first. An operator pushing a new config wants the whole list — a missing listen address *and* a duplicate cluster *and* a cluster with no endpoints — so they can fix all of it in one pass instead of rerunning the validator after each single fix. Each joined sub-error still satisfies `errors.Is`, so a caller can branch on `ErrDuplicateCluster` specifically while the human reads the full joined message.

`UpdateConfig` is where the atomic discipline lives. It validates the incoming config, then builds a *complete* new backend map and round-robin counter map off to the side, and only then takes the write lock to swap both in together. A reader holding the read lock therefore sees either the entire old state or the entire new state — never a config that names a cluster whose backends have not been built yet. The wrong shape is to write the config under one lock and the backends under another: a request that slips between the two writes routes against a mismatched pair and fails for no reason the operator can see in the config.

`SelectBackend` is round-robin over the *healthy* members. A per-cluster `atomic.Uint64` counter, taken modulo the backend count, advances the cursor without a lock; the loop runs at most `n` times and returns an error if every backend is unhealthy, so it never spins. Health is a per-backend `atomic.Bool` flipped by `SetHealthy`, which is the seam a real health checker plugs into — it learns a backend is down and calls `SetHealthy(cluster, addr, false)`, and the very next selection skips it.

Create `config.go`:

```go
package dataplane

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Sentinel errors for configuration validation.
var (
	ErrInvalidConfig    = errors.New("invalid config")
	ErrDuplicateCluster = errors.New("duplicate cluster name")
	ErrNoEndpoints      = errors.New("cluster has no endpoints")
)

// Cluster is an upstream cluster definition: a name and its endpoint addresses.
type Cluster struct {
	Name      string
	Endpoints []string
}

// Config is the data-plane configuration. Build it and call Validate before
// passing it to New or UpdateConfig.
type Config struct {
	ListenAddr string
	AdminAddr  string
	Clusters   []Cluster
}

// Validate returns all configuration errors, not just the first. It uses
// errors.Join so callers can still test individual errors with errors.Is.
func Validate(cfg Config) error {
	var errs []error
	if cfg.ListenAddr == "" {
		errs = append(errs, fmt.Errorf("%w: ListenAddr required", ErrInvalidConfig))
	}
	if cfg.AdminAddr == "" {
		errs = append(errs, fmt.Errorf("%w: AdminAddr required", ErrInvalidConfig))
	}
	seen := map[string]bool{}
	for _, c := range cfg.Clusters {
		if seen[c.Name] {
			errs = append(errs, fmt.Errorf("%w: %q", ErrDuplicateCluster, c.Name))
		}
		seen[c.Name] = true
		if len(c.Endpoints) == 0 {
			errs = append(errs, fmt.Errorf("%w: cluster %q", ErrNoEndpoints, c.Name))
		}
	}
	return errors.Join(errs...)
}

// Backend is an upstream endpoint with an atomic health flag.
type Backend struct {
	Addr    string
	healthy atomic.Bool
}

// Healthy reports the current health flag.
func (b *Backend) Healthy() bool { return b.healthy.Load() }

// SetHealthy updates the health flag. A health checker calls this when a probe
// result changes.
func (b *Backend) SetHealthy(v bool) { b.healthy.Store(v) }

// DataPlane owns the validated configuration and the backend pool, and serves
// healthy backends by round-robin. The pool can be replaced atomically with
// UpdateConfig.
type DataPlane struct {
	mu       sync.RWMutex
	cfg      Config
	backends map[string][]*Backend
	rrIdx    map[string]*atomic.Uint64
}

// New validates cfg and builds the backend pool. It returns an error without
// allocating any pool if validation fails.
func New(cfg Config) (*DataPlane, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	dp := &DataPlane{
		cfg:      cfg,
		backends: make(map[string][]*Backend, len(cfg.Clusters)),
		rrIdx:    make(map[string]*atomic.Uint64, len(cfg.Clusters)),
	}
	for _, c := range cfg.Clusters {
		dp.backends[c.Name], dp.rrIdx[c.Name] = buildPool(c.Endpoints)
	}
	return dp, nil
}

// buildPool turns a list of endpoint addresses into a backend slice (all
// optimistically healthy until the first probe) and a fresh round-robin counter.
func buildPool(endpoints []string) ([]*Backend, *atomic.Uint64) {
	bs := make([]*Backend, 0, len(endpoints))
	for _, ep := range endpoints {
		b := &Backend{Addr: ep}
		b.healthy.Store(true) // optimistic until the first health probe
		bs = append(bs, b)
	}
	return bs, new(atomic.Uint64)
}

// SelectBackend returns the next healthy backend for cluster using round-robin.
// It returns an error when the cluster is unknown or every backend is unhealthy.
func (dp *DataPlane) SelectBackend(cluster string) (*Backend, error) {
	dp.mu.RLock()
	bs := dp.backends[cluster]
	ctr := dp.rrIdx[cluster]
	dp.mu.RUnlock()

	if len(bs) == 0 {
		return nil, fmt.Errorf("no backends for cluster %q", cluster)
	}
	n := uint64(len(bs))
	for i := uint64(0); i < n; i++ {
		idx := ctr.Add(1) % n
		if b := bs[idx]; b.Healthy() {
			return b, nil
		}
	}
	return nil, fmt.Errorf("no healthy backend for cluster %q", cluster)
}

// SetHealthy flips the health flag of the backend at addr in cluster. It returns
// false when no such backend exists. This is the seam a health checker uses to
// push probe results into the routing pool.
func (dp *DataPlane) SetHealthy(cluster, addr string, healthy bool) bool {
	dp.mu.RLock()
	defer dp.mu.RUnlock()
	for _, b := range dp.backends[cluster] {
		if b.Addr == addr {
			b.SetHealthy(healthy)
			return true
		}
	}
	return false
}

// UpdateConfig replaces the active configuration atomically. Validate runs
// first; if it fails the live state is untouched. The new backend map is built
// in full before the swap, so a concurrent SelectBackend never sees a config
// that names a cluster whose pool has not been built.
func (dp *DataPlane) UpdateConfig(cfg Config) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	nb := make(map[string][]*Backend, len(cfg.Clusters))
	nr := make(map[string]*atomic.Uint64, len(cfg.Clusters))
	for _, c := range cfg.Clusters {
		nb[c.Name], nr[c.Name] = buildPool(c.Endpoints)
	}
	dp.mu.Lock()
	dp.cfg = cfg
	dp.backends = nb
	dp.rrIdx = nr
	dp.mu.Unlock()
	return nil
}

// Clusters returns the names of the configured clusters, for status reporting.
func (dp *DataPlane) Clusters() []string {
	dp.mu.RLock()
	defer dp.mu.RUnlock()
	names := make([]string, 0, len(dp.cfg.Clusters))
	for _, c := range dp.cfg.Clusters {
		names = append(names, c.Name)
	}
	return names
}
```

The no-allocation-on-failure rule in `New` matters: validation runs before the maps are built, so a rejected config leaves no half-constructed object for a caller to mistakenly use. `buildPool` is shared by `New` and `UpdateConfig` so the two construction paths cannot drift apart — a subtle bug class where the initial pool and a reloaded pool are built by two slightly different loops.

### The runnable demo

The demo builds a plane with three backends, routes a few requests to show round-robin, fails one backend to show it is skipped, then hot-reloads to a fresh cluster and routes again. Selection is deterministic because the round-robin counter starts at zero, so the output is identical on every machine.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"log"

	"example.com/dataplane"
)

func main() {
	cfg := dataplane.Config{
		ListenAddr: "127.0.0.1:18080",
		AdminAddr:  "127.0.0.1:19902",
		Clusters: []dataplane.Cluster{{
			Name:      "web",
			Endpoints: []string{"10.0.0.1:8080", "10.0.0.2:8080", "10.0.0.3:8080"},
		}},
	}

	dp, err := dataplane.New(cfg)
	if err != nil {
		log.Fatalf("New: %v", err)
	}

	fmt.Println("round-robin over three healthy backends:")
	for i := 0; i < 3; i++ {
		b, err := dp.SelectBackend("web")
		if err != nil {
			log.Fatalf("SelectBackend: %v", err)
		}
		fmt.Printf("  request %d -> %s\n", i+1, b.Addr)
	}

	dp.SetHealthy("web", "10.0.0.2:8080", false)
	fmt.Println("after 10.0.0.2:8080 fails its health check:")
	for i := 0; i < 3; i++ {
		b, err := dp.SelectBackend("web")
		if err != nil {
			log.Fatalf("SelectBackend: %v", err)
		}
		fmt.Printf("  request %d -> %s\n", i+1, b.Addr)
	}

	bad := dataplane.Config{ListenAddr: "127.0.0.1:18080"}
	fmt.Printf("validating an incomplete config: invalid=%v noEndpoints=%v\n",
		errors.Is(dataplane.Validate(bad), dataplane.ErrInvalidConfig),
		errors.Is(dataplane.Validate(dataplane.Config{
			ListenAddr: "a", AdminAddr: "b",
			Clusters: []dataplane.Cluster{{Name: "empty"}},
		}), dataplane.ErrNoEndpoints))

	newCfg := cfg
	newCfg.Clusters = []dataplane.Cluster{{
		Name:      "web",
		Endpoints: []string{"10.1.0.1:9090", "10.1.0.2:9090"},
	}}
	if err := dp.UpdateConfig(newCfg); err != nil {
		log.Fatalf("UpdateConfig: %v", err)
	}
	b, err := dp.SelectBackend("web")
	if err != nil {
		log.Fatalf("SelectBackend: %v", err)
	}
	fmt.Printf("after hot-reload, next request -> %s\n", b.Addr)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
round-robin over three healthy backends:
  request 1 -> 10.0.0.2:8080
  request 2 -> 10.0.0.3:8080
  request 3 -> 10.0.0.1:8080
after 10.0.0.2:8080 fails its health check:
  request 1 -> 10.0.0.3:8080
  request 2 -> 10.0.0.1:8080
  request 3 -> 10.0.0.3:8080
validating an incomplete config: invalid=true noEndpoints=true
after hot-reload, next request -> 10.1.0.2:9090
```

### Tests

The tests pin the validation contract (every error reported, each reachable with `errors.Is`), the round-robin distribution, the unhealthy-skip and all-unhealthy-error behavior, and the immediacy of an atomic update.

Create `config_test.go`:

```go
package dataplane

import (
	"errors"
	"fmt"
	"testing"
)

func validCfg(endpoints ...string) Config {
	return Config{
		ListenAddr: "127.0.0.1:18080",
		AdminAddr:  "127.0.0.1:19902",
		Clusters:   []Cluster{{Name: "svc", Endpoints: endpoints}},
	}
}

func TestValidateRejectsEmptyListenAddr(t *testing.T) {
	t.Parallel()
	err := Validate(Config{AdminAddr: ":9902"})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestValidateRejectsDuplicateCluster(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ListenAddr: ":8080",
		AdminAddr:  ":9902",
		Clusters: []Cluster{
			{Name: "svc-a", Endpoints: []string{"127.0.0.1:9001"}},
			{Name: "svc-a", Endpoints: []string{"127.0.0.1:9002"}},
		},
	}
	if !errors.Is(Validate(cfg), ErrDuplicateCluster) {
		t.Fatal("expected ErrDuplicateCluster")
	}
}

func TestValidateRejectsClusterWithNoEndpoints(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ListenAddr: ":8080",
		AdminAddr:  ":9902",
		Clusters:   []Cluster{{Name: "svc-a"}},
	}
	if !errors.Is(Validate(cfg), ErrNoEndpoints) {
		t.Fatal("expected ErrNoEndpoints")
	}
}

func TestValidateCollectsAllErrors(t *testing.T) {
	t.Parallel()
	// ListenAddr missing AND a cluster with no endpoints: both must appear.
	cfg := Config{
		AdminAddr: ":9902",
		Clusters:  []Cluster{{Name: "svc-a"}},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("ErrInvalidConfig missing from %v", err)
	}
	if !errors.Is(err, ErrNoEndpoints) {
		t.Errorf("ErrNoEndpoints missing from %v", err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected New to reject an empty config")
	}
}

func TestSelectBackendRoundRobin(t *testing.T) {
	t.Parallel()
	dp, err := New(validCfg("ep1:80", "ep2:80", "ep3:80"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		b, err := dp.SelectBackend("svc")
		if err != nil {
			t.Fatalf("SelectBackend: %v", err)
		}
		seen[b.Addr]++
	}
	for addr, count := range seen {
		if count != 3 {
			t.Errorf("%s: count = %d, want 3", addr, count)
		}
	}
}

func TestSelectBackendSkipsUnhealthy(t *testing.T) {
	t.Parallel()
	dp, err := New(validCfg("ep1:80", "ep2:80"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !dp.SetHealthy("svc", "ep1:80", false) {
		t.Fatal("SetHealthy returned false for a known backend")
	}
	for i := 0; i < 4; i++ {
		b, err := dp.SelectBackend("svc")
		if err != nil {
			t.Fatalf("SelectBackend: %v", err)
		}
		if b.Addr != "ep2:80" {
			t.Errorf("got %q, want ep2:80", b.Addr)
		}
	}
}

func TestSelectBackendAllUnhealthyReturnsError(t *testing.T) {
	t.Parallel()
	dp, err := New(validCfg("ep1:80"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dp.SetHealthy("svc", "ep1:80", false)
	if _, err := dp.SelectBackend("svc"); err == nil {
		t.Fatal("expected error when all backends unhealthy")
	}
}

func TestSelectBackendUnknownCluster(t *testing.T) {
	t.Parallel()
	dp, err := New(validCfg("ep1:80"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := dp.SelectBackend("nope"); err == nil {
		t.Fatal("expected error for unknown cluster")
	}
}

func TestUpdateConfigAtomic(t *testing.T) {
	t.Parallel()
	dp, err := New(validCfg("old:80"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := dp.UpdateConfig(Config{
		ListenAddr: "127.0.0.1:18080",
		AdminAddr:  "127.0.0.1:19902",
		Clusters:   []Cluster{{Name: "svc", Endpoints: []string{"new:80"}}},
	}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	b, err := dp.SelectBackend("svc")
	if err != nil {
		t.Fatalf("SelectBackend: %v", err)
	}
	if b.Addr != "new:80" {
		t.Fatalf("addr = %q, want new:80", b.Addr)
	}
}

func TestUpdateConfigRejectsInvalidLeavesLiveState(t *testing.T) {
	t.Parallel()
	dp, err := New(validCfg("good:80"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// An invalid update (duplicate cluster) must not touch the live pool.
	bad := Config{
		ListenAddr: ":8080", AdminAddr: ":9902",
		Clusters: []Cluster{
			{Name: "svc", Endpoints: []string{"x:80"}},
			{Name: "svc", Endpoints: []string{"y:80"}},
		},
	}
	if err := dp.UpdateConfig(bad); !errors.Is(err, ErrDuplicateCluster) {
		t.Fatalf("err = %v, want ErrDuplicateCluster", err)
	}
	b, err := dp.SelectBackend("svc")
	if err != nil {
		t.Fatalf("SelectBackend after rejected update: %v", err)
	}
	if b.Addr != "good:80" {
		t.Fatalf("live state changed after rejected update: addr = %q", b.Addr)
	}
}

func ExampleValidate() {
	err := Validate(Config{AdminAddr: ":9902"})
	fmt.Println(errors.Is(err, ErrInvalidConfig))
	// Output:
	// true
}
```

## Review

The core is correct when validation is exhaustive and the swap is indivisible. `TestValidateCollectsAllErrors` is the one that matters most: it proves `errors.Join` reports both a missing listen address and an empty cluster in a single result, and that each is independently reachable with `errors.Is` — the property that lets an operator fix everything in one pass. The selection tests pin the routing contract: `TestSelectBackendRoundRobin` checks an even spread, `TestSelectBackendSkipsUnhealthy` checks that a failed backend is never returned, and `TestSelectBackendAllUnhealthyReturnsError` checks the loop terminates with an error instead of spinning. The two update tests pin atomicity from both sides: `TestUpdateConfigAtomic` proves the new pool is live on the very next call, and `TestUpdateConfigRejectsInvalidLeavesLiveState` proves a rejected update changes nothing. The most common way to break this code is to swap the config and the backend map under two separate locks; run the suite with `-race` to catch a reader that observes the mismatched pair.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — joins multiple errors into one whose `Unwrap() []error` lets `errors.Is` match each constituent; the basis of all-errors-at-once validation.
- [`sync/atomic` `Bool` and `Uint64`](https://pkg.go.dev/sync/atomic) — the lock-free health flag and round-robin counter used here without taking a mutex on the hot path.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read-mostly lock that lets many selectors run concurrently while an update swaps the pool exclusively.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-wired-data-plane.md](02-wired-data-plane.md)
