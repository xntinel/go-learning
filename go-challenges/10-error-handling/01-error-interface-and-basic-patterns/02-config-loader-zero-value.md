# Exercise 2: An Env Config Loader That Returns the Zero Value on Failure

Startup config is the first place the "zero value on the error path" rule bites in
production. A twelve-factor service reads its settings from the environment, and
if any one is missing or malformed the process must refuse to start — never boot
with a half-parsed config where `Port` is set but `ReadTimeout` silently defaulted
to zero. This exercise builds that loader with `getenv` injected so tests need no
process-env mutation.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
configloader/                independent module: example.com/configloader
  go.mod                     go 1.26
  config.go                  type Config; LoadConfig(getenv) (Config, error)
  cmd/
    demo/
      main.go                runnable demo: load from a fixed getenv, print result
  config_test.go             per-field failure table asserting zero Config + named key
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `LoadConfig(getenv func(string) (string, bool)) (Config, error)` reading `PORT`, `READ_TIMEOUT`, `DATABASE_URL`, parsing each and returning a zero `Config{}` plus a descriptive error the moment any field is missing or malformed.
- Test: assert a missing/invalid field returns `Config{}` (equality) and a non-nil error naming the key; assert the happy path returns the fully-populated struct and nil; one row per field's failure so no guard is skipped.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/01-error-interface-and-basic-patterns/02-config-loader-zero-value/cmd/demo
cd go-solutions/10-error-handling/01-error-interface-and-basic-patterns/02-config-loader-zero-value
```

### Why the zero value must be total, and why getenv is injected

The failure mode this guards against is a *partial* config leaking to the caller.
Imagine `LoadConfig` parsed `PORT` successfully, then hit a malformed
`READ_TIMEOUT` and returned `Config{Port: 8080}, err`. A caller that logs the
error but continues — or one that reads `cfg.Port` before checking `err` — now
runs with a config that is 40% real and 60% zero, and the zero `ReadTimeout` means
"no timeout", the most dangerous default there is. Returning a *total* `Config{}`
on every error path removes the temptation entirely: the only way to get a
non-zero field is to get a nil error alongside it.

`getenv func(string) (string, bool)` is injected rather than calling
`os.Getenv`/`os.LookupEnv` directly for one concrete reason: testability without
global mutation. `os.Setenv` mutates process-wide state, which forces test
serialization and leaks between tests. Injecting the lookup lets each test pass a
pure `map`-backed function, so the failure table can run in parallel with no
shared state. The production caller passes `os.LookupEnv`, whose signature —
`func(string) (string, bool)` — matches exactly, so the seam costs nothing at the
call site. The `bool` (present) is distinct from `""` (present but empty), and the
loader treats a missing key and an empty `DATABASE_URL` as different, clearly-named
failures.

Each guard names the offending key in its error so the operator reading the crash
log knows exactly which environment variable to fix. Parse errors are wrapped with
`%w` so a caller could, if it wanted, inspect the underlying `strconv.NumError` or
duration parse error.

Create `config.go`:

```go
package configloader

import (
	"fmt"
	"strconv"
	"time"
)

// Config is the fully-parsed service configuration. It is comparable, so tests
// can assert equality against the zero Config{} directly.
type Config struct {
	Port        int
	ReadTimeout time.Duration
	DatabaseURL string
}

// LoadConfig reads and validates PORT, READ_TIMEOUT, and DATABASE_URL through the
// injected getenv. On any missing or malformed field it returns a total zero
// Config{} and an error naming the offending key. getenv has the same signature
// as os.LookupEnv, so production passes os.LookupEnv and tests pass a fake.
func LoadConfig(getenv func(string) (string, bool)) (Config, error) {
	portStr, ok := getenv("PORT")
	if !ok {
		return Config{}, fmt.Errorf("missing required env PORT")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return Config{}, fmt.Errorf("invalid PORT %q: %w", portStr, err)
	}
	if port < 1 || port > 65535 {
		return Config{}, fmt.Errorf("PORT %d out of range 1-65535", port)
	}

	timeoutStr, ok := getenv("READ_TIMEOUT")
	if !ok {
		return Config{}, fmt.Errorf("missing required env READ_TIMEOUT")
	}
	timeout, err := time.ParseDuration(timeoutStr)
	if err != nil {
		return Config{}, fmt.Errorf("invalid READ_TIMEOUT %q: %w", timeoutStr, err)
	}
	if timeout <= 0 {
		return Config{}, fmt.Errorf("READ_TIMEOUT must be positive, got %s", timeout)
	}

	dbURL, ok := getenv("DATABASE_URL")
	if !ok || dbURL == "" {
		return Config{}, fmt.Errorf("missing required env DATABASE_URL")
	}

	return Config{Port: port, ReadTimeout: timeout, DatabaseURL: dbURL}, nil
}
```

### The runnable demo

The demo builds a fixed `getenv` from a map and loads a valid config, then shows
one failing load to make the zero-value-plus-error behavior visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configloader"
)

func main() {
	env := map[string]string{
		"PORT":         "8080",
		"READ_TIMEOUT": "5s",
		"DATABASE_URL": "postgres://localhost:5432/app",
	}
	getenv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	cfg, err := configloader.LoadConfig(getenv)
	if err != nil {
		fmt.Println("load failed:", err)
		return
	}
	fmt.Printf("port=%d read_timeout=%s db=%s\n", cfg.Port, cfg.ReadTimeout, cfg.DatabaseURL)

	delete(env, "READ_TIMEOUT")
	if _, err := configloader.LoadConfig(getenv); err != nil {
		fmt.Println("load failed:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
port=8080 read_timeout=5s db=postgres://localhost:5432/app
load failed: missing required env READ_TIMEOUT
```

### Tests

The failure table has one row per independent guard, so no single check can be
deleted without a test going red. Each row asserts two things: the returned config
equals the zero `Config{}` and the error names the offending key. The happy-path
test asserts the fully-populated struct and a nil error.

Create `config_test.go`:

```go
package configloader

import (
	"strings"
	"testing"
	"time"
)

func fakeEnv(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoadConfigHappyPath(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(fakeEnv(map[string]string{
		"PORT":         "8080",
		"READ_TIMEOUT": "5s",
		"DATABASE_URL": "postgres://localhost/app",
	}))
	if err != nil {
		t.Fatalf("LoadConfig = %v, want nil", err)
	}
	want := Config{Port: 8080, ReadTimeout: 5 * time.Second, DatabaseURL: "postgres://localhost/app"}
	if cfg != want {
		t.Fatalf("cfg = %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigFailuresReturnZero(t *testing.T) {
	t.Parallel()

	base := map[string]string{
		"PORT":         "8080",
		"READ_TIMEOUT": "5s",
		"DATABASE_URL": "postgres://localhost/app",
	}

	tests := []struct {
		name    string
		mutate  func(m map[string]string)
		wantKey string
	}{
		{"missing PORT", func(m map[string]string) { delete(m, "PORT") }, "PORT"},
		{"non-numeric PORT", func(m map[string]string) { m["PORT"] = "abc" }, "PORT"},
		{"out-of-range PORT", func(m map[string]string) { m["PORT"] = "70000" }, "PORT"},
		{"missing READ_TIMEOUT", func(m map[string]string) { delete(m, "READ_TIMEOUT") }, "READ_TIMEOUT"},
		{"malformed READ_TIMEOUT", func(m map[string]string) { m["READ_TIMEOUT"] = "5" }, "READ_TIMEOUT"},
		{"non-positive READ_TIMEOUT", func(m map[string]string) { m["READ_TIMEOUT"] = "-1s" }, "READ_TIMEOUT"},
		{"missing DATABASE_URL", func(m map[string]string) { delete(m, "DATABASE_URL") }, "DATABASE_URL"},
		{"empty DATABASE_URL", func(m map[string]string) { m["DATABASE_URL"] = "" }, "DATABASE_URL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := make(map[string]string, len(base))
			for k, v := range base {
				env[k] = v
			}
			tt.mutate(env)

			cfg, err := LoadConfig(fakeEnv(env))
			if err == nil {
				t.Fatalf("LoadConfig = nil error, want failure for %s", tt.name)
			}
			if cfg != (Config{}) {
				t.Fatalf("cfg on error path = %+v, want zero Config", cfg)
			}
			if !strings.Contains(err.Error(), tt.wantKey) {
				t.Fatalf("err = %q, want it to name %q", err.Error(), tt.wantKey)
			}
		})
	}
}
```

## Review

The loader is correct when the only way to obtain a non-zero field is to receive a
nil error with it: every guard returns `Config{}`, and the happy path is the sole
producer of a populated struct. The per-field table is what enforces this — remove
any one guard and its row fails, so the coverage is structural rather than
incidental. Naming the key in each error is the operational payoff: a crash log
reading `invalid PORT "abc"` is actionable without opening source.

The mistakes to avoid: returning a partially-filled `Config` alongside an error
(a caller will use it), and reaching for `os.Getenv` directly inside `LoadConfig`
(it collapses "missing" and "empty" into the same `""` and forces tests to mutate
global state). Keep `getenv` injected and pass `os.LookupEnv` in production. Wrap
parse errors with `%w` so the underlying `strconv`/`time` error stays inspectable.

## Resources

- [pkg.go.dev: os.LookupEnv](https://pkg.go.dev/os#LookupEnv) — the injected lookup's production counterpart, distinguishing unset from empty.
- [pkg.go.dev: strconv.Atoi](https://pkg.go.dev/strconv#Atoi) and [time.ParseDuration](https://pkg.go.dev/time#ParseDuration) — the two parsers and the errors they return.
- [The Twelve-Factor App: Config](https://12factor.net/config) — why configuration lives in the environment and must be strict at startup.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-user-service-error-contract.md](01-user-service-error-contract.md) | Next: [03-typed-nil-interface-trap.md](03-typed-nil-interface-trap.md)
