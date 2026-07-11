# Exercise 9: Killing Hidden Global State — init(env) to Constructor with Options

The most common `init()` abuse is reading `os.Getenv` and stashing the result in a
package-level global. This exercise refactors that anti-pattern into an explicit
`Load(...Option) (*Config, error)` constructor with functional options and an
injected `getenv`, making configuration deterministic, injectable, and free of
import-time side effects — and leaving no `init()` in the package.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
appconfig/                   independent module: example.com/appconfig
  go.mod                     module example.com/appconfig
  config.go                  Config, Option, Load(...Option) (*Config, error); injected getenv
  cmd/demo/main.go           loads from the real process env via os.Getenv
  config_test.go             fake getenv -> populated Config; missing keys -> joined error; independence
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `Load(...Option) (*Config, error)` reading through an injected `getenv func(string) string`, with functional options, required-key validation aggregated via `errors.Join`, and no `init()` and no package global.
Test: a fake `getenv` yields a populated `Config` with no reliance on the process env; missing required keys return a joined error each matchable with `errors.Is`; two `Load` calls with different env produce independent `Config`s.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/appconfig/cmd/demo
cd ~/go-exercises/appconfig
go mod init example.com/appconfig
```

### The anti-pattern and the fix

The starting point (shown here only as an illustrative, non-compiled block) is the
code this exercise exists to eliminate:

```text
// WRONG: hidden global filled from the environment at import time.
var Config struct {
	Addr string
	DSN  string
}

func init() {
	Config.Addr = os.Getenv("APP_ADDR")   // runs before flags/logging/context exist
	Config.DSN = os.Getenv("APP_DSN")     // cannot be injected; a test cannot reset it
	// a missing required key has nowhere to report an error except a panic
}
```

Every problem in this lesson converges here. The read happens at import time, so it
cannot be parameterized and freezes whatever the process env was. The value is a
package global, so two tests cannot each load a different configuration and a test
cannot reset it between runs. Validation has nowhere to send an error except a
panic during load. And it silently does nothing useful in a test binary that reads
the global without ever setting the env.

The fix is an explicit constructor. `Load(opts ...Option) (*Config, error)` reads
through an injected `getenv func(string) string` rather than calling `os.Getenv`
directly, so a test passes a fake map-backed lookup and never touches the real
environment. Functional options (`WithGetenv`, `WithDefaultAddr`) customize the
load without a forest of constructor parameters. Required-key validation collects
*every* failure and returns them with `errors.Join`, so a single `Load` reports all
missing keys at once and each is recoverable with `errors.Is`. There is no `init()`
and no package global: each `Load` returns an independent `*Config` the caller owns.

The injected-`getenv` seam is the key move. In production `Load()` defaults its
`getenv` to `os.Getenv`; in tests you pass `WithGetenv(fakeEnv.lookup)`. The
configuration logic is identical in both, but the test controls the inputs
completely and deterministically.

Create `config.go`:

```go
// config.go
package appconfig

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// Sentinel errors for each required key, so a caller can match a specific
// missing value with errors.Is even though Load joins them.
var (
	ErrMissingAddr = errors.New("APP_ADDR is required")
	ErrMissingDSN  = errors.New("APP_DSN is required")
)

// Config is the loaded application configuration. It is returned by Load and
// owned by the caller; there is no package-level global.
type Config struct {
	Addr     string
	DSN      string
	MaxConns int
}

// options carries the knobs Load reads. getenv is injected so tests supply a
// fake lookup instead of mutating the process environment.
type options struct {
	getenv      func(string) string
	defaultAddr string
	maxConns    int
}

// Option customizes a Load call.
type Option func(*options)

// WithGetenv injects the environment lookup. Defaults to os.Getenv.
func WithGetenv(f func(string) string) Option {
	return func(o *options) { o.getenv = f }
}

// WithDefaultAddr sets the address used when APP_ADDR is unset (making Addr
// optional). Without it, APP_ADDR is required.
func WithDefaultAddr(addr string) Option {
	return func(o *options) { o.defaultAddr = addr }
}

// WithMaxConns sets the fallback pool size when APP_MAX_CONNS is unset or unparseable.
func WithMaxConns(n int) Option {
	return func(o *options) { o.maxConns = n }
}

// Load builds a Config from the injected environment. It aggregates every
// validation failure with errors.Join instead of returning only the first, and
// has no import-time side effect: nothing runs until you call it.
func Load(opts ...Option) (*Config, error) {
	o := options{getenv: os.Getenv, maxConns: 10}
	for _, opt := range opts {
		opt(&o)
	}

	cfg := &Config{
		Addr: o.getenv("APP_ADDR"),
		DSN:  o.getenv("APP_DSN"),
	}

	var errs []error
	if cfg.Addr == "" {
		if o.defaultAddr != "" {
			cfg.Addr = o.defaultAddr
		} else {
			errs = append(errs, ErrMissingAddr)
		}
	}
	if cfg.DSN == "" {
		errs = append(errs, ErrMissingDSN)
	}

	cfg.MaxConns = o.maxConns
	if raw := o.getenv("APP_MAX_CONNS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("APP_MAX_CONNS %q is not an integer: %w", raw, err))
		} else {
			cfg.MaxConns = n
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}
```

### The runnable demo

In production, `Load` is called from `main` with no options, so it reads the real
process environment through the default `os.Getenv`. The demo sets a couple of env
vars first so the run is self-contained and deterministic.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"os"

	"example.com/appconfig"
)

func main() {
	// Set the env this process will read, so the demo is reproducible.
	os.Setenv("APP_ADDR", ":8080")
	os.Setenv("APP_DSN", "postgres://localhost/app")
	os.Setenv("APP_MAX_CONNS", "25")

	cfg, err := appconfig.Load()
	if err != nil {
		fmt.Println("config error:", err)
		return
	}
	fmt.Printf("addr=%s maxConns=%d\n", cfg.Addr, cfg.MaxConns)
	fmt.Println("dsn set:", cfg.DSN != "")

	// A second load against an empty injected env is independent of the first
	// and reports every missing key at once.
	if _, err := appconfig.Load(appconfig.WithGetenv(func(string) string { return "" })); err != nil {
		fmt.Println("empty env rejected:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
addr=:8080 maxConns=25
dsn set: true
empty env rejected: APP_ADDR is required
APP_DSN is required
```

### Tests

The tests pass a fake `getenv` and never touch the process environment, so they are
deterministic and can run in parallel. `TestLoadFromFakeEnv` proves a populated
`Config`; `TestMissingKeysAreJoined` proves both sentinels come back from one
`Load` and each is matchable with `errors.Is`; `TestTwoLoadsAreIndependent` proves
the global is gone — two loads with different env yield different configs.

Create `config_test.go`:

```go
// config_test.go
package appconfig

import (
	"errors"
	"testing"
)

// fakeEnv is a map-backed getenv, injected via WithGetenv.
func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadFromFakeEnv(t *testing.T) {
	t.Parallel()

	cfg, err := Load(WithGetenv(fakeEnv(map[string]string{
		"APP_ADDR":      ":9000",
		"APP_DSN":       "postgres://db/app",
		"APP_MAX_CONNS": "50",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":9000" || cfg.DSN != "postgres://db/app" || cfg.MaxConns != 50 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestDefaultMaxConns(t *testing.T) {
	t.Parallel()

	cfg, err := Load(WithGetenv(fakeEnv(map[string]string{
		"APP_ADDR": ":9000",
		"APP_DSN":  "dsn",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != 10 {
		t.Fatalf("MaxConns = %d, want default 10", cfg.MaxConns)
	}
}

func TestMissingKeysAreJoined(t *testing.T) {
	t.Parallel()

	_, err := Load(WithGetenv(fakeEnv(map[string]string{})))
	if err == nil {
		t.Fatal("Load with empty env returned nil error")
	}
	if !errors.Is(err, ErrMissingAddr) {
		t.Errorf("joined error missing ErrMissingAddr: %v", err)
	}
	if !errors.Is(err, ErrMissingDSN) {
		t.Errorf("joined error missing ErrMissingDSN: %v", err)
	}
}

func TestDefaultAddrSatisfiesRequirement(t *testing.T) {
	t.Parallel()

	cfg, err := Load(
		WithGetenv(fakeEnv(map[string]string{"APP_DSN": "dsn"})),
		WithDefaultAddr(":8080"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8080" {
		t.Fatalf("Addr = %q, want default :8080", cfg.Addr)
	}
}

func TestBadMaxConnsIsReported(t *testing.T) {
	t.Parallel()

	_, err := Load(WithGetenv(fakeEnv(map[string]string{
		"APP_ADDR":      ":9000",
		"APP_DSN":       "dsn",
		"APP_MAX_CONNS": "not-a-number",
	})))
	if err == nil {
		t.Fatal("bad APP_MAX_CONNS did not produce an error")
	}
}

func TestTwoLoadsAreIndependent(t *testing.T) {
	t.Parallel()

	a, err := Load(WithGetenv(fakeEnv(map[string]string{"APP_ADDR": ":1", "APP_DSN": "d1"})))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(WithGetenv(fakeEnv(map[string]string{"APP_ADDR": ":2", "APP_DSN": "d2"})))
	if err != nil {
		t.Fatal(err)
	}
	if a.Addr == b.Addr || a == b {
		t.Fatalf("loads not independent: a=%+v b=%+v", a, b)
	}
}
```

## Review

The refactor is correct when configuration is fully determined by `Load`'s inputs
and nothing else: `TestLoadFromFakeEnv` builds a `Config` from an injected map with
no reference to the process environment, and `TestTwoLoadsAreIndependent` builds two
different configs in one test — impossible against an `init()`-filled global. The
`errors.Join` aggregation means `TestMissingKeysAreJoined` recovers *both* sentinels
from a single `Load` via `errors.Is`, so a caller learns every problem at once
rather than fixing them one restart at a time.

The mistakes to avoid are the ones the anti-pattern block embodies: reading
`os.Getenv` at import time (freezes and hides the config), storing the result in a
package global (untestable, unresettable), and returning only the first validation
error (forces a fix-restart-repeat loop). The injected `getenv` seam, the functional
options, and `errors.Join` remove all three. Confirm no `init()` remains in the
package — the whole point is that nothing runs until `main` (or a test) calls
`Load`.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — aggregate multiple validation failures into one error that `errors.Is` can unwrap.
- [Functional options (Rob Pike)](https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html) — the self-referential option pattern.
- [os.Getenv](https://pkg.go.dev/os#Getenv) — the default lookup that `Load` injects and tests replace.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-derived-lookup-table-init-order.md](10-derived-lookup-table-init-order.md)
