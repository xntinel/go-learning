# Exercise 1: Load Service Config From The Environment With Typed Errors

Every backend service boots by reading its configuration from the environment.
This is the baseline artifact the rest of the lesson builds on: a `Load` that
reads `APP_HOST` and `APP_PORT`, returns a typed `Config`, and reports failures
as wrapped sentinel errors an operator can act on. It is tested with `t.Setenv`,
serially — the exact suite the injection exercise later makes parallel.

## What you'll build

```text
envconfig/                 independent module: example.com/envconfig
  go.mod                   go directive supplied by the gate
  config.go                Config{Host,Port}; Load(); ErrMissingHost, ErrInvalidPort
  cmd/
    demo/
      main.go              runnable demo: set env, Load, print; then a failure path
  config_test.go           table-driven t.Setenv cases: success, missing host, bad/negative port
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `Load() (Config, error)` reading `APP_HOST` and `APP_PORT`, wrapping `ErrMissingHost` / `ErrInvalidPort` with `%w`.
Test: a table of `(env, want Config, wantErr)` driven by `t.Setenv` per case; assert with `errors.Is` and field equality.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/17-testing-with-environment-variables/01-env-config-loader/cmd/demo
cd go-solutions/12-testing-ecosystem/17-testing-with-environment-variables/01-env-config-loader
```

## The design

`Load` reads two variables. `APP_HOST` is required, so an empty value maps to
`ErrMissingHost`. `APP_PORT` must parse as a positive integer, so both a
non-numeric value and a non-positive one map to `ErrInvalidPort`. The two failure
modes are *sentinel errors* — package-level `error` values a caller can test with
`errors.Is` — and every return path wraps them with `fmt.Errorf("...: %w", ...)`
so the message carries context (the offending value) while the sentinel remains
matchable through the wrap.

Why sentinels and `%w` rather than returning a bare `errors.New` each time? Because
the caller of `Load` — a startup routine, or a test — wants to branch on *which*
kind of failure happened without string-matching the message. `errors.Is(err,
ErrInvalidPort)` is stable; `strings.Contains(err.Error(), "port")` is not.
Wrapping with `%w` preserves both: a human-readable message and a machine-matchable
identity.

This loader is deliberately serial. It reads `os.Getenv` directly, so any test of
it must mutate the process environment with `t.Setenv`, and `t.Setenv` forbids
`t.Parallel`. That is the correct baseline; Exercise 6 shows how to lift the
parallelism restriction by injecting the reader.

Create `config.go`:

```go
package envconfig

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// Sentinel errors let callers branch on the failure mode with errors.Is.
var (
	ErrMissingHost = errors.New("missing host")
	ErrInvalidPort = errors.New("invalid port")
)

// Config is the resolved service configuration.
type Config struct {
	Host string
	Port int
}

// Load reads APP_HOST and APP_PORT from the process environment. APP_HOST must
// be non-empty; APP_PORT must parse as a positive integer. Failures are wrapped
// sentinel errors carrying the offending value.
func Load() (Config, error) {
	host := os.Getenv("APP_HOST")
	if host == "" {
		return Config{}, fmt.Errorf("load config: %w", ErrMissingHost)
	}

	portStr := os.Getenv("APP_PORT")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return Config{}, fmt.Errorf("load config: APP_PORT=%q: %w", portStr, ErrInvalidPort)
	}
	if port <= 0 {
		return Config{}, fmt.Errorf("load config: APP_PORT=%d not positive: %w", port, ErrInvalidPort)
	}

	return Config{Host: host, Port: port}, nil
}
```

## The runnable demo

The demo sets the two variables (a demo is a `package main`, so it may call
`os.Setenv` directly), loads, prints the result, then unsets `APP_HOST` to show
the failure path an operator sees when a required variable is missing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/envconfig"
)

func main() {
	os.Setenv("APP_HOST", "db.internal")
	os.Setenv("APP_PORT", "5432")

	cfg, err := envconfig.Load()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("host=%s port=%d\n", cfg.Host, cfg.Port)

	os.Unsetenv("APP_HOST")
	if _, err := envconfig.Load(); err != nil {
		fmt.Println("error:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
host=db.internal port=5432
error: load config: missing host
```

## Tests

The tests are table-driven. Each case sets its environment with `t.Setenv`
inside its subtest, so the values are restored automatically when that subtest
ends. None of these subtests may call `t.Parallel` — `t.Setenv` would panic — so
the suite is serial by design. The success case asserts the returned `Config`
field by field; the failure cases assert the sentinel with `errors.Is`. The
negative-port case pins the "reject non-positive port" contract that a bare
`strconv.Atoi` success would otherwise let through.

Create `config_test.go`:

```go
package envconfig

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		port    string
		want    Config
		wantErr error
	}{
		{
			name: "valid",
			host: "db.local",
			port: "5432",
			want: Config{Host: "db.local", Port: 5432},
		},
		{
			name:    "missing host",
			host:    "",
			port:    "5432",
			wantErr: ErrMissingHost,
		},
		{
			name:    "non-numeric port",
			host:    "db.local",
			port:    "abc",
			wantErr: ErrInvalidPort,
		},
		{
			name:    "negative port",
			host:    "db.local",
			port:    "-1",
			wantErr: ErrInvalidPort,
		},
		{
			name:    "zero port",
			host:    "db.local",
			port:    "0",
			wantErr: ErrInvalidPort,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv restores the prior value after the subtest; no t.Parallel.
			t.Setenv("APP_HOST", tt.host)
			t.Setenv("APP_PORT", tt.port)

			got, err := Load()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Load() err = %v, want errors.Is %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Load() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func ExampleLoad() {
	os.Setenv("APP_HOST", "db.internal")
	os.Setenv("APP_PORT", "5432")

	cfg, _ := Load()
	fmt.Printf("%s:%d\n", cfg.Host, cfg.Port)
	// Output: db.internal:5432
}
```

## Review

The loader is correct when each failure mode maps to its sentinel and the success
path returns exactly the parsed fields. The two things worth checking: `errors.Is`
must succeed against the sentinel because every return path wraps with `%w`
(switch to `%v` and the sentinel becomes unreachable and the table fails); and the
port must reject `0` and negatives, not just non-numeric strings, because a port
of `0` is a real deployment mistake `strconv.Atoi` accepts. The suite is serial
on purpose — `t.Setenv` forbids `t.Parallel` — which is the friction Exercise 6
removes by inverting the dependency on `os`.

## Resources

- [testing.T.Setenv](https://pkg.go.dev/testing#T.Setenv) — sets an env var for the test and restores it on cleanup.
- [os.Getenv](https://pkg.go.dev/os#Getenv) — reads an env var, returning `""` when unset.
- [errors.Is and %w wrapping](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel error.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-hermetic-env-restore.md](02-hermetic-env-restore.md)
