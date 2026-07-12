# Exercise 30: Atomic Config Reload: Hot-Swap Configuration Without Downtime

**Nivel: Intermedio** — validacion rapida (un test corto).

Restarting a service to pick up a configuration change costs availability
every service would rather not spend, but a naive in-place reload — mutating
fields on a shared config struct while requests are reading it — invites a
reader to observe a half-updated value that never existed as a coherent
whole. Swapping the *entire* config behind an atomic pointer sidesteps both
problems: readers never block, never see a torn value, and a rejected reload
never takes effect at all. This module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
configreload/                independent module: example.com/atomic-config-reload-zero-downtime
  go.mod                    go 1.24
  reload.go                 Config, Store (atomic.Pointer-backed), NewStore, Load, Reload
  cmd/
    demo/
      main.go               a good reload, a parse failure, and a validation failure
  reload_test.go            swap on success; reject on parse/validation error; concurrent reads -race
```

- Files: `reload.go`, `cmd/demo/main.go`, `reload_test.go`.
- Implement: a `Config` struct, a `validate(c *Config) error` guard, and a `Store` wrapping `atomic.Pointer[Config]` with `NewStore(initial *Config) (*Store, error)`, `Load() *Config`, and `Reload(raw []byte, parse func([]byte) (*Config, error)) error` that only swaps in the new config after both parsing and validation succeed.
- Test: a successful reload swaps the visible config; a parse error and a validation error each leave the previous config untouched; a concurrency test hammering `Load` from many goroutines while `Reload` runs repeatedly, asserting `Load` never returns nil, under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/01-if-else-and-init-statements/30-atomic-config-reload-zero-downtime/cmd/demo
cd go-solutions/03-control-flow/01-if-else-and-init-statements/30-atomic-config-reload-zero-downtime
go mod edit -go=1.24
```

### Why validation runs before the swap, not after

`Reload` calls `parse`, then `validate`, and only calls `s.ptr.Store` once
both have returned no error. Both guards return before the swap, on purpose:
the atomic pointer swap is the one operation in this module that is
genuinely irreversible from a reader's point of view the instant it
happens, since the next `Load` anywhere in the process immediately starts
handing out the new value. If validation ran after the swap — or if the
swap happened before parsing finished — a malformed config file dropped by
an automated deploy tool could take effect on every in-flight and future
request before anyone notices, and there is no readers-side lock to buy
back the mistake. Guarding the swap this way means a bad reload's blast
radius is exactly zero: the store keeps serving the last known-good config,
and the caller of `Reload` gets a wrapped error naming exactly which stage
rejected the new value.

Create `reload.go`:

```go
// Package configreload hot-swaps configuration with zero reader downtime and
// no reader-side locking: requests already in flight keep using the config
// pointer they loaded, and new requests see the new config the instant it is
// validated, with never a nil or half-written value in between.
package configreload

import (
	"errors"
	"fmt"
	"sync/atomic"
)

// Config is the application configuration a request path reads.
type Config struct {
	MaxConns int
	Timeout  int // milliseconds; kept as an int to avoid importing time here
	FeatureX bool
}

// validate rejects a config that would be unsafe to serve traffic with.
// Reload runs this before ever exposing the parsed value to a reader.
func validate(c *Config) error {
	if c.MaxConns <= 0 {
		return errors.New("max_conns must be positive")
	}
	if c.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	return nil
}

// Store holds the current Config behind an atomic pointer. The zero value is
// not ready to use; construct with NewStore so Load never returns nil.
type Store struct {
	ptr atomic.Pointer[Config]
}

// NewStore builds a Store seeded with initial, which must be non-nil and
// already valid — the store's whole contract, that Load never returns nil,
// starts from this guard.
func NewStore(initial *Config) (*Store, error) {
	if initial == nil {
		return nil, errors.New("configreload: initial config must not be nil")
	}
	if err := validate(initial); err != nil {
		return nil, fmt.Errorf("configreload: initial config invalid: %w", err)
	}
	s := &Store{}
	s.ptr.Store(initial)
	return s, nil
}

// Load returns the current config. It never blocks and never returns nil:
// every caller reads either the config Reload just installed or the one
// before it, never a torn or partially applied value, because atomic.Pointer
// swaps the whole pointer in one indivisible step.
func (s *Store) Load() *Config {
	return s.ptr.Load()
}

// Reload parses raw with parse, validates the result, and only then swaps it
// in atomically. A parse failure or a validation failure returns before the
// swap, so a bad deploy of a config file never takes effect — every reader
// keeps serving the last known-good config, and the caller learns exactly
// why the reload was rejected.
func (s *Store) Reload(raw []byte, parse func([]byte) (*Config, error)) error {
	cfg, err := parse(raw)
	if err != nil {
		return fmt.Errorf("configreload: parse failed, keeping current config: %w", err)
	}
	if err := validate(cfg); err != nil {
		return fmt.Errorf("configreload: validation failed, keeping current config: %w", err)
	}
	s.ptr.Store(cfg)
	return nil
}
```

### The runnable demo

The demo reloads a config successfully, then attempts two bad reloads — a
malformed JSON payload and a well-formed but invalid config — and shows the
store keeps serving the last good config after each rejection.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	configreload "example.com/atomic-config-reload-zero-downtime"
)

func parseJSON(raw []byte) (*configreload.Config, error) {
	var c configreload.Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func main() {
	store, err := configreload.NewStore(&configreload.Config{MaxConns: 10, Timeout: 500})
	if err != nil {
		fmt.Println("unexpected error building store:", err)
		return
	}
	fmt.Printf("initial config: %+v\n", *store.Load())

	// A good reload takes effect immediately.
	if err := store.Reload([]byte(`{"max_conns":0}`), func(raw []byte) (*configreload.Config, error) {
		return &configreload.Config{MaxConns: 50, Timeout: 750, FeatureX: true}, nil
	}); err != nil {
		fmt.Println("unexpected error on good reload:", err)
	}
	fmt.Printf("after good reload: %+v\n", *store.Load())

	// A malformed payload never reaches validate; the old config stays live.
	err = store.Reload([]byte(`not json`), parseJSON)
	fmt.Println("reload with malformed payload:", err)
	fmt.Printf("config after failed parse: %+v\n", *store.Load())

	// A well-formed but invalid config (zero max_conns) is rejected too.
	err = store.Reload([]byte(`{"MaxConns":0,"Timeout":100}`), parseJSON)
	fmt.Println("reload with invalid config:", err)
	fmt.Printf("config after failed validation: %+v\n", *store.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial config: {MaxConns:10 Timeout:500 FeatureX:false}
after good reload: {MaxConns:50 Timeout:750 FeatureX:true}
reload with malformed payload: configreload: parse failed, keeping current config: invalid character 'o' in literal null (expecting 'u')
config after failed parse: {MaxConns:50 Timeout:750 FeatureX:true}
reload with invalid config: configreload: validation failed, keeping current config: max_conns must be positive
config after failed validation: {MaxConns:50 Timeout:750 FeatureX:true}
```

### Tests

Beyond `NewStore`'s own guards, a successful reload, a parse-error reload,
and a validation-error reload each get their own test, and a concurrency
test hammers `Load` from eight goroutines while a hundred reloads run in
sequence, asserting `Load` never returns nil, under `-race`.

Create `reload_test.go`:

```go
package configreload

import (
	"errors"
	"sync"
	"testing"
)

func validConfig() *Config { return &Config{MaxConns: 10, Timeout: 500} }

func TestNewStoreRejectsNilAndInvalid(t *testing.T) {
	t.Parallel()

	if _, err := NewStore(nil); err == nil {
		t.Error("NewStore(nil) = nil error, want an error")
	}
	if _, err := NewStore(&Config{MaxConns: 0, Timeout: 500}); err == nil {
		t.Error("NewStore with MaxConns=0 = nil error, want an error")
	}
}

func TestReloadSwapsConfig(t *testing.T) {
	t.Parallel()

	store, err := NewStore(validConfig())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	err = store.Reload(nil, func([]byte) (*Config, error) {
		return &Config{MaxConns: 99, Timeout: 1000, FeatureX: true}, nil
	})
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}

	got := store.Load()
	if got.MaxConns != 99 || got.Timeout != 1000 || !got.FeatureX {
		t.Fatalf("Load() = %+v, want the newly reloaded config", got)
	}
}

func TestReloadRejectsParseError(t *testing.T) {
	t.Parallel()

	store, err := NewStore(validConfig())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	wantErr := errors.New("boom")

	err = store.Reload(nil, func([]byte) (*Config, error) {
		return nil, wantErr
	})
	if err == nil {
		t.Fatal("Reload with a parse error = nil, want an error")
	}
	if got := store.Load(); got.MaxConns != 10 {
		t.Fatalf("Load() after failed parse = %+v, want the original config unchanged", got)
	}
}

func TestReloadRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	store, err := NewStore(validConfig())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	err = store.Reload(nil, func([]byte) (*Config, error) {
		return &Config{MaxConns: -1, Timeout: 500}, nil
	})
	if err == nil {
		t.Fatal("Reload with an invalid config = nil, want an error")
	}
	if got := store.Load(); got.MaxConns != 10 {
		t.Fatalf("Load() after failed validation = %+v, want the original config unchanged", got)
	}
}

func TestConcurrentReadsNeverSeeNil(t *testing.T) {
	store, err := NewStore(validConfig())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers hammer Load concurrently with a writer reloading repeatedly.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if store.Load() == nil {
						t.Error("Load() returned nil during concurrent reload")
						return
					}
				}
			}
		}()
	}

	for i := 0; i < 100; i++ {
		n := i + 1
		_ = store.Reload(nil, func([]byte) (*Config, error) {
			return &Config{MaxConns: n, Timeout: 500}, nil
		})
	}
	close(stop)
	wg.Wait()
}
```

Verify: `go test -count=1 -race ./...`

## Review

`NewStore` runs the exact same `validate` guard `Reload` runs, which matters
more than it looks: without that guard, a `Store` could be constructed with
an already-invalid config, and `Load` — which promises callers a config
that's always safe to use — would be lying from the very first call. Carry
this forward: whenever a type's read path documents an invariant ("never
nil," "always validated"), every constructor and every mutator has to enforce
that invariant, not just the mutator that happens to be the one under test
today.

## Resources

- [sync/atomic: Pointer](https://pkg.go.dev/sync/atomic#Pointer) — the primitive that makes the swap itself torn-value-free.
- [NGINX: Reloading a Configuration](https://nginx.org/en/docs/control.html#reconfiguration) — a production system built around exactly this "validate first, swap atomically" reload contract.
- [The Twelve-Factor App: Config](https://12factor.net/config) — the operational reason config reload needs to be a first-class, safe primitive rather than a restart.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-cron-job-schedule-evaluator.md](29-cron-job-schedule-evaluator.md) | Next: [31-consistent-hashing-partition-routing.md](31-consistent-hashing-partition-routing.md)
