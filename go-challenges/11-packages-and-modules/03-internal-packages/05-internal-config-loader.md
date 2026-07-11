# Exercise 5: Keep A Typed Config Struct Off The Public API

Configuration is an implementation detail that changes constantly — a field added,
a default tuned, a variable renamed. If the `Config` struct is on your public API,
every one of those edits is a potential breaking change for someone who coupled to
its shape. Put the loader under `internal/config` and no other team or module can
depend on the struct at all, so it evolves freely while the service exposes only
what callers actually need.

This module is fully self-contained: its own module, its own demo, its own tests.
Nothing here imports any other exercise.

## What you'll build

```text
svcconfig/                         module example.com/svcconfig
  go.mod
  internal/config/config.go        type Config; Load; ErrMissing, ErrInvalid
  internal/config/config_test.go   white-box table test using t.Setenv
  cmd/demo/main.go                 runnable demo loading config from env
```

- Files: `internal/config/config.go`, `internal/config/config_test.go`, `cmd/demo/main.go`.
- Implement: a `Load` that reads environment variables into a typed `Config`, applies defaults, and aggregates validation failures with `errors.Join`, wrapping sentinels with `%w`.
- Test: table-driven cases with `t.Setenv` for all-valid, missing-required, unparseable int/duration, defaults-applied, and multiple aggregated errors, matched via `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/svcconfig/internal/config ~/go-exercises/svcconfig/cmd/demo
cd ~/go-exercises/svcconfig
go mod init example.com/svcconfig
```

### Why the config loader is internal

A `Config` struct is the single most volatile type in a service. If it is exported
from a package another team can import, its field names, types, and zero-value
semantics become a contract: renaming `Timeout` to `RequestTimeout`, or changing it
from `int` seconds to a `time.Duration`, breaks their build. Hiding the loader under
`internal/config` removes that coupling entirely — only your own module's `cmd/` and
packages can construct or read a `Config`, so you can reshape it at will. The service
exposes behavior (an HTTP server, a worker), not its raw configuration.

The loader itself models real production practice: required variables are errors
when absent, optional ones fall back to defaults, and malformed values are reported
with enough context to fix them. Crucially, it does not stop at the first failure.
A caller who forgot three variables wants all three named in one run, not a
whack-a-mole where each restart reveals the next missing key. `errors.Join`
aggregates every failure into one error whose `Error()` lists them all, while
`errors.Is` still matches each wrapped sentinel — so the operator sees every problem
and your tests can assert the category of each.

The two sentinels partition the failure modes: `ErrMissing` for a required variable
that is absent, `ErrInvalid` for one that is present but unparseable. Wrapping them
with `%w` (Go 1.20+ allows multiple `%w` in one `fmt.Errorf`) keeps the human
message rich while preserving machine-matchability.

Create `internal/config/config.go`:

```go
// Package config loads and validates service configuration from the environment.
// It is internal, so its Config shape is not a public contract and may change.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Sentinels partitioning the two failure modes; callers match with errors.Is.
var (
	ErrMissing = errors.New("config: missing required variable")
	ErrInvalid = errors.New("config: invalid variable")
)

// Config is the typed, validated service configuration. Being internal, this
// shape is free to evolve without breaking any downstream module.
type Config struct {
	Port     int
	DBURL    string
	Timeout  time.Duration
	LogLevel string
}

// Load reads APP_* variables into a Config. Required variables (APP_PORT,
// APP_DB_URL) produce ErrMissing when absent; unparseable values produce
// ErrInvalid. Optional variables fall back to defaults. All failures are
// aggregated with errors.Join so a caller sees every problem at once.
func Load() (Config, error) {
	var errs []error
	cfg := Config{LogLevel: "info", Timeout: 5 * time.Second}

	if s := os.Getenv("APP_PORT"); s == "" {
		errs = append(errs, fmt.Errorf("%w: APP_PORT", ErrMissing))
	} else if p, err := strconv.Atoi(s); err != nil {
		errs = append(errs, fmt.Errorf("%w: APP_PORT=%q: %w", ErrInvalid, s, err))
	} else {
		cfg.Port = p
	}

	if s := os.Getenv("APP_DB_URL"); s == "" {
		errs = append(errs, fmt.Errorf("%w: APP_DB_URL", ErrMissing))
	} else {
		cfg.DBURL = s
	}

	if s := os.Getenv("APP_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err != nil {
			errs = append(errs, fmt.Errorf("%w: APP_TIMEOUT=%q: %w", ErrInvalid, s, err))
		} else {
			cfg.Timeout = d
		}
	}

	if s := os.Getenv("APP_LOG_LEVEL"); s != "" {
		cfg.LogLevel = s
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}
```

### The runnable demo

The demo sets a minimal valid environment and loads it, showing the defaults filling
in for the optional variables. `cmd/demo` sits under the module root, so it is a
legal importer of `internal/config`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/svcconfig/internal/config"
)

func main() {
	os.Setenv("APP_PORT", "8080")
	os.Setenv("APP_DB_URL", "postgres://localhost/app")

	cfg, err := config.Load()
	if err != nil {
		fmt.Println("load:", err)
		return
	}
	fmt.Printf("port=%d timeout=%s log=%s\n", cfg.Port, cfg.Timeout, cfg.LogLevel)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
port=8080 timeout=5s log=info
```

### Tests

The test is a white-box `package config` test using `t.Setenv` to control the
environment per case. Because `t.Setenv` forbids parallel tests, none of these call
`t.Parallel`. The success cases set all four variables explicitly (an empty string
stands in for "unset") so their result is fully pinned against the ambient
environment. The error-path cases set only the keys that drive the failure and assert
just two things: that the aggregated error matches the expected sentinel via
`errors.Is`, and that its message names the offending variable. That assertion is
robust to any stray ambient `APP_*` value — an extra unrelated variable could only add
another joined error, never remove the one under test — so these cases stay
deterministic without spelling out all four keys. Cases cover all-valid, each
missing-required, each unparseable value, defaults, and a two-error aggregation matched
through `errors.Is`.

Create `internal/config/config_test.go`:

```go
package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr error
		wantMsg string // substring required in the aggregated error
	}{
		{
			name: "all valid with overrides",
			env:  map[string]string{"APP_PORT": "9000", "APP_DB_URL": "db://x", "APP_TIMEOUT": "2s", "APP_LOG_LEVEL": "debug"},
			want: Config{Port: 9000, DBURL: "db://x", Timeout: 2 * time.Second, LogLevel: "debug"},
		},
		{
			name: "defaults applied",
			env:  map[string]string{"APP_PORT": "80", "APP_DB_URL": "db://y", "APP_TIMEOUT": "", "APP_LOG_LEVEL": ""},
			want: Config{Port: 80, DBURL: "db://y", Timeout: 5 * time.Second, LogLevel: "info"},
		},
		{
			name:    "missing port names the variable",
			env:     map[string]string{"APP_PORT": "", "APP_DB_URL": "db://z"},
			wantErr: ErrMissing,
			wantMsg: "APP_PORT",
		},
		{
			name:    "unparseable port",
			env:     map[string]string{"APP_PORT": "eighty", "APP_DB_URL": "db://z"},
			wantErr: ErrInvalid,
			wantMsg: "APP_PORT",
		},
		{
			name:    "unparseable timeout",
			env:     map[string]string{"APP_PORT": "80", "APP_DB_URL": "db://z", "APP_TIMEOUT": "soon"},
			wantErr: ErrInvalid,
			wantMsg: "APP_TIMEOUT",
		},
		{
			name:    "both required missing are aggregated",
			env:     map[string]string{"APP_PORT": "", "APP_DB_URL": ""},
			wantErr: ErrMissing,
			wantMsg: "APP_DB_URL",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			got, err := Load()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Load err = %v, want to match %v", err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("Load err = %q, want it to name %q", err.Error(), tc.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load unexpected err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("Load = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBothMissingListsBoth(t *testing.T) {
	t.Setenv("APP_PORT", "")
	t.Setenv("APP_DB_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("want error when both required vars missing")
	}
	msg := err.Error()
	if !strings.Contains(msg, "APP_PORT") || !strings.Contains(msg, "APP_DB_URL") {
		t.Fatalf("aggregated err = %q, want it to name both variables", msg)
	}
}
```

## Review

The loader is correct when it aggregates rather than short-circuits: a run with two
problems reports both, each matchable by its sentinel through `errors.Join`, and
each named in the message so an operator can fix them in one pass. Defaults apply
only to optional variables; a missing required variable is always an error. The
`t.Setenv`-per-case pattern makes the table deterministic without a parallel-test
hazard.

The traps: do not call `t.Parallel` in a `t.Setenv` test — the runtime forbids it
and the test panics. Do not short-circuit on the first error, or you force operators
into serial guess-and-restart. And keep the `Config` struct under `internal` — the
moment it becomes importable, its shape is a contract and the freedom this exercise
buys you is gone.

## Resources

- [`os.Getenv`](https://pkg.go.dev/os#Getenv) and [`testing.T.Setenv`](https://pkg.go.dev/testing#T.Setenv) — reading and (in tests) controlling the environment.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — aggregating multiple validation failures into one error.
- [`fmt.Errorf`](https://pkg.go.dev/fmt#Errorf) — wrapping with `%w` (multiple wraps allowed since Go 1.20).

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-internal-postgres-repository.md](04-internal-postgres-repository.md) | Next: [06-scoping-internal-by-depth.md](06-scoping-internal-by-depth.md)
