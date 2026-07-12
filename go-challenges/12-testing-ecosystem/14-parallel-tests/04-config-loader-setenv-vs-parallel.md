# Exercise 4: Making an Env-Driven Config Loader Parallelizable

Config loaders read `os.Getenv`. Tests for them reach for `t.Setenv` — which
*panics* the moment the test is parallel or has a parallel ancestor, because it
mutates process-global state. The senior fix is not to abandon parallelism but to
split the loader: a thin env-reading adapter tested once serially, and pure
parsing logic that takes an injected source and can be tested in parallel. This
module builds that split.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
configload/                 independent module: example.com/configload
  go.mod
  config.go                 Config; Load(Source); LoadFromEnv(); sentinels
  cmd/
    demo/
      main.go               runnable demo loading from an in-memory Source
  config_test.go            serial t.Setenv test + parallel injected-source tests
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `Load(Source) (Config, error)` (pure, injected key/value lookup) and
`LoadFromEnv() (Config, error)` (thin adapter over `os.Getenv`), parsing an int,
a duration, and applying defaults/validation.
Test: `TestLoadFromEnv` serial with `t.Setenv`; `TestLoad_Injected` parallel with
a table driving the injected `Source`.
Verify: `go test -count=1 -race ./...`

### Why t.Setenv and t.Parallel are mutually exclusive

Environment variables are a single, process-wide table. Two goroutines that set
and read it concurrently race, and there is no per-goroutine environment to
isolate them. `t.Setenv` sets a var and registers cleanup to restore it, but the
mutation is global for the duration — so if another parallel test read the env at
the same time, it would see a value some *other* test set. The standard library
refuses to allow that footgun: `t.Setenv` calls `t.Parallel`-detection and panics
with "testing: t.Setenv called after t.Parallel" if the test is parallel or has a
parallel ancestor. `t.Chdir` (the working directory, equally process-global)
behaves the same way. This is a guardrail, not an inconvenience.

For illustration only — never assemble this, it panics:

```go
func TestPanics(t *testing.T) {
	t.Parallel()
	t.Setenv("PORT", "8080") // panics: t.Setenv called after t.Parallel
}
```

### The design fix: inject the source

The reason `LoadFromEnv` cannot be parallel is that it reads `os.Getenv` — a
process-global dependency baked into the function. Remove the dependency and the
constraint disappears. Define a `Source` interface with a single `Lookup(key)
(value, ok)` method; `Load(Source)` does *all* the parsing, validation, and
defaulting against that abstract source. `LoadFromEnv` becomes a one-line adapter:
wrap `os.LookupEnv` in a `Source` and call `Load`. Now the entire parsing matrix —
missing keys, bad integers, invalid durations, out-of-range values, defaults — is
tested through `Load` with an in-memory `mapSource`, in parallel, with no
`t.Setenv` anywhere. Only the trivial adapter needs the one serial `t.Setenv`
test, and it needs just enough cases to prove the wiring, because the logic is
already covered in parallel.

This is the general shape of design-for-testability: push the process-global
edge (env, CWD, wall clock, filesystem) to a thin adapter, and make the logic
underneath depend on an injected interface. The logic then parallelizes.

Create `config.go`:

```go
package configload

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

var (
	ErrMissingDBURL = errors.New("DATABASE_URL is required")
	ErrBadPort      = errors.New("PORT is not a valid integer")
	ErrPortRange    = errors.New("PORT out of range 1..65535")
	ErrBadTimeout   = errors.New("TIMEOUT is not a valid duration")
)

// Config is the parsed application configuration.
type Config struct {
	DatabaseURL string
	Port        int
	Timeout     time.Duration
}

// Source abstracts a key/value lookup so the parsing logic does not depend on
// os.Getenv. Lookup reports whether the key was present.
type Source interface {
	Lookup(key string) (string, bool)
}

// Load parses and validates a Config from any Source, applying defaults for
// absent optional keys. This function is pure with respect to process globals,
// so tests can drive it in parallel with an in-memory Source.
func Load(src Source) (Config, error) {
	var cfg Config

	dbURL, ok := src.Lookup("DATABASE_URL")
	if !ok || dbURL == "" {
		return Config{}, ErrMissingDBURL
	}
	cfg.DatabaseURL = dbURL

	cfg.Port = 8080
	if raw, ok := src.Lookup("PORT"); ok && raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("parse PORT %q: %w", raw, ErrBadPort)
		}
		if port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("PORT %d: %w", port, ErrPortRange)
		}
		cfg.Port = port
	}

	cfg.Timeout = 30 * time.Second
	if raw, ok := src.Lookup("TIMEOUT"); ok && raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("parse TIMEOUT %q: %w", raw, ErrBadTimeout)
		}
		cfg.Timeout = d
	}

	return cfg, nil
}

// envSource adapts the process environment to the Source interface.
type envSource struct{}

func (envSource) Lookup(key string) (string, bool) { return os.LookupEnv(key) }

// LoadFromEnv is the thin process-global adapter: it reads real environment
// variables and delegates all logic to Load. Because it touches os.Getenv, its
// tests must be serial and use t.Setenv.
func LoadFromEnv() (Config, error) {
	return Load(envSource{})
}
```

### The runnable demo

The demo loads from an in-memory `MapSource` so it is deterministic and needs no
environment — the same source the parallel tests use.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configload"
)

func main() {
	src := configload.MapSource{
		"DATABASE_URL": "postgres://localhost/app",
		"PORT":         "9090",
	}
	cfg, err := configload.Load(src)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("db=%s port=%d timeout=%s\n", cfg.DatabaseURL, cfg.Port, cfg.Timeout)
}
```

`MapSource` is an exported in-memory `Source` the demo and tests share. Add it to
`config.go`.

Append to `config.go`:

```go
// MapSource is an in-memory Source backed by a map, used by tests and the demo
// to drive Load without touching the process environment.
type MapSource map[string]string

func (m MapSource) Lookup(key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
db=postgres://localhost/app port=9090 timeout=30s
```

### Tests

`TestLoadFromEnv` is serial (no `t.Parallel()`) and uses `t.Setenv` to exercise
the real adapter path once: it sets the env, loads, and checks the wired values
and the default timeout. Every other case — defaults, bad port, out-of-range,
missing DB URL, bad duration — runs through `TestLoad_Injected`, which *is*
parallel and drives the injected `MapSource` with a table. This is the payoff:
the whole validation matrix parallelizes because it never touches the env.

Create `config_test.go`:

```go
package configload

import (
	"errors"
	"testing"
	"time"
)

// Serial: t.Setenv forbids t.Parallel. One case proves the env adapter wiring;
// the logic matrix is covered in parallel by TestLoad_Injected.
func TestLoadFromEnv(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("PORT", "7000")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if cfg.DatabaseURL != "postgres://localhost/db" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.Port != 7000 {
		t.Errorf("Port = %d, want 7000", cfg.Port)
	}
	if cfg.Timeout != 30*time.Second {
		t.Errorf("Timeout = %s, want default 30s", cfg.Timeout)
	}
}

func TestLoad_Injected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		src     MapSource
		want    Config
		wantErr error
	}{
		{
			"defaults applied",
			MapSource{"DATABASE_URL": "db"},
			Config{DatabaseURL: "db", Port: 8080, Timeout: 30 * time.Second},
			nil,
		},
		{
			"all set",
			MapSource{"DATABASE_URL": "db", "PORT": "9000", "TIMEOUT": "5s"},
			Config{DatabaseURL: "db", Port: 9000, Timeout: 5 * time.Second},
			nil,
		},
		{"missing db url", MapSource{"PORT": "9000"}, Config{}, ErrMissingDBURL},
		{"bad port", MapSource{"DATABASE_URL": "db", "PORT": "abc"}, Config{}, ErrBadPort},
		{"port out of range", MapSource{"DATABASE_URL": "db", "PORT": "70000"}, Config{}, ErrPortRange},
		{"bad timeout", MapSource{"DATABASE_URL": "db", "TIMEOUT": "5 hours"}, Config{}, ErrBadTimeout},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Load(tc.src)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Load = %v, want errors.Is %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: unexpected error %v", err)
			}
			if got != tc.want {
				t.Fatalf("Load = %+v, want %+v", got, tc.want)
			}
		})
	}
}
```

## Review

The split is correct when `Load` contains all the logic and depends only on the
injected `Source`, while `LoadFromEnv` is a one-line adapter over `os.LookupEnv`.
The evidence it worked: `TestLoad_Injected` is `t.Parallel()` with parallel
subtests and covers the whole matrix, and yet the suite runs clean under `-race`
because no test touches a process global. The single serial `TestLoadFromEnv`
exists only to prove the env wiring; if you tried to mark it parallel, `t.Setenv`
would panic — which is the runtime telling you the dependency belongs behind an
interface.

The anti-pattern is testing the whole loader through `t.Setenv` and then wondering
why nothing can be parallel. Push the process-global read to the edge; test the
logic through an injected source.

## Resources

- [`testing.T.Setenv`](https://pkg.go.dev/testing#T.Setenv) — and its documented panic under a parallel ancestor.
- [`os.LookupEnv`](https://pkg.go.dev/os#LookupEnv) — presence-reporting env lookup, the adapter's core.
- [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration) — duration parsing and its accepted grammar.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-shared-server-fixture-teardown.md](03-shared-server-fixture-teardown.md) | Next: [05-race-on-parallel-ttl-cache.md](05-race-on-parallel-ttl-cache.md)
