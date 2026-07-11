# Exercise 3: Config Loader: Validated Env Parsing with Guard Clauses

Every service runs a config bootstrap at startup: read the environment, parse and
validate each variable, fail loudly if anything is wrong before serving a single
request. This is the twelve-factor pattern, and it is a decision tree — each field
is an init-statement `if` that separates "absent" from "present but invalid" and
early-returns a wrapped error naming the offending variable.

This solution lives in the shared `go-solutions` module — no `go mod init`, just a
folder that mirrors this exercise's path, plus a demo and hermetic tests.

## What you'll build

```text
go-solutions/03-control-flow/01-if-else-and-init-statements/03-env-config-loader/
  config.go        Config, LoadConfig(lookup), sentinels, defaults + invariants
  cmd/
    main.go         loads from os.LookupEnv, prints the resolved Config
  config_test.go    hermetic table-driven tests over a fake lookup
```

- Files: `config.go`, `cmd/main.go`, `config_test.go`.
- Implement: `LoadConfig(lookup func(string) (string, bool)) (Config, error)` reading `DB_DSN`, `HTTP_PORT`, `READ_TIMEOUT`, `MAX_CONNS`, `DEBUG`, with defaults for optional keys and cross-field invariants.
- Test: a fake lookup backed by a map; full valid set; each missing-required key; each malformed value; defaults applied when optional keys absent. Assert `errors.Is` against exported sentinels.
- Verify: `go test -count=1 -race ./...`

Create the folder:

```bash
mkdir -p go-solutions/03-control-flow/01-if-else-and-init-statements/03-env-config-loader/cmd
cd go-solutions/03-control-flow/01-if-else-and-init-statements/03-env-config-loader
```

## Absent versus present-but-invalid, one field at a time

The signature is the whole design: `LoadConfig` takes a `lookup func(string) (string, bool)`
rather than calling `os.LookupEnv` directly. That is the seam that makes the loader
testable without mutating real process environment — the demo passes `os.LookupEnv`,
the tests pass a closure over a map. The `bool` in the signature is the same
comma-ok idiom as a map read: it tells absent from present-but-empty, which for a
required variable is the difference between `ErrMissing` and `ErrInvalid`.

Each field is one init-statement `if`. For a required key:
`if raw, ok := lookup("DB_DSN"); !ok || raw == "" { return Config{}, fmt.Errorf(...%w, ErrMissing) }`.
For a key that must parse, the parse and its error handling sit in the same narrow
scope, and any failure early-returns a wrapped error that names the variable — so the
operator reading the crash log sees `HTTP_PORT: invalid: ...`, not a bare
`strconv.Atoi` error. Wrapping with `%w` is what keeps the sentinel matchable by
`errors.Is` while adding the variable name for humans.

Optional keys get a default when absent (`READ_TIMEOUT` defaults to 5s, `MAX_CONNS`
to 10, `DEBUG` to false), but a *present* optional key that is malformed is still an
error — "not set" and "set wrong" are different, and only the first is allowed to
fall back. Finally, cross-field invariants run after parsing: the port must be in
range, the timeout strictly positive, `MAX_CONNS` at least one. These are guard
clauses too, each early-returning `ErrInvalid` for the offending field.

Create `config.go`:

```go
package envconfig

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Sentinels the caller matches with errors.Is.
var (
	ErrMissing = errors.New("required variable not set")
	ErrInvalid = errors.New("variable has invalid value")
)

// Config is the validated startup configuration.
type Config struct {
	DBDSN       string
	HTTPPort    int
	ReadTimeout time.Duration
	MaxConns    int
	Debug       bool
}

// Defaults for optional variables.
const (
	defaultReadTimeout = 5 * time.Second
	defaultMaxConns    = 10
)

// LoadConfig reads and validates configuration through lookup, which is
// os.LookupEnv in production and a fake map in tests. It returns the zero Config
// and a wrapped sentinel on the first problem.
func LoadConfig(lookup func(string) (string, bool)) (Config, error) {
	var cfg Config

	if raw, ok := lookup("DB_DSN"); !ok || raw == "" {
		return Config{}, fmt.Errorf("DB_DSN: %w", ErrMissing)
	} else {
		cfg.DBDSN = raw
	}

	if raw, ok := lookup("HTTP_PORT"); !ok || raw == "" {
		return Config{}, fmt.Errorf("HTTP_PORT: %w", ErrMissing)
	} else if port, err := strconv.Atoi(raw); err != nil {
		return Config{}, fmt.Errorf("HTTP_PORT: %w: %q", ErrInvalid, raw)
	} else if port < 1 || port > 65535 {
		return Config{}, fmt.Errorf("HTTP_PORT: %w: out of range %d", ErrInvalid, port)
	} else {
		cfg.HTTPPort = port
	}

	cfg.ReadTimeout = defaultReadTimeout
	if raw, ok := lookup("READ_TIMEOUT"); ok && raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("READ_TIMEOUT: %w: %q", ErrInvalid, raw)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("READ_TIMEOUT: %w: must be positive", ErrInvalid)
		}
		cfg.ReadTimeout = d
	}

	cfg.MaxConns = defaultMaxConns
	if raw, ok := lookup("MAX_CONNS"); ok && raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("MAX_CONNS: %w: %q", ErrInvalid, raw)
		}
		if n < 1 {
			return Config{}, fmt.Errorf("MAX_CONNS: %w: must be >= 1", ErrInvalid)
		}
		cfg.MaxConns = int(n)
	}

	cfg.Debug = false
	if raw, ok := lookup("DEBUG"); ok && raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("DEBUG: %w: %q", ErrInvalid, raw)
		}
		cfg.Debug = b
	}

	return cfg, nil
}
```

The `if raw, ok := ...; !ok { ... } else { cfg.X = raw }` shape here is a legitimate
use of `else`: `raw` is scoped to the `if`, so the assignment must happen inside the
chain where `raw` is visible. This is different from `else`-after-`return`, which is
dead code — here neither branch has returned unconditionally in a way that makes the
`else` redundant, and the value is only in scope inside the chain.

### The runnable demo

The demo loads from a fixed in-memory map (so the output is deterministic) and
prints the resolved config, then shows a missing-variable failure.

Create `cmd/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/envconfig"
)

func main() {
	full := map[string]string{
		"DB_DSN":       "postgres://localhost/app",
		"HTTP_PORT":    "8080",
		"READ_TIMEOUT": "3s",
		"MAX_CONNS":    "25",
		"DEBUG":        "true",
	}
	lookup := func(key string) (string, bool) {
		v, ok := full[key]
		return v, ok
	}

	cfg, err := envconfig.LoadConfig(lookup)
	if err != nil {
		fmt.Println("load failed:", err)
		return
	}
	fmt.Printf("port=%d timeout=%s maxconns=%d debug=%v\n",
		cfg.HTTPPort, cfg.ReadTimeout, cfg.MaxConns, cfg.Debug)

	empty := func(string) (string, bool) { return "", false }
	if _, err := envconfig.LoadConfig(empty); errors.Is(err, envconfig.ErrMissing) {
		fmt.Println("empty env:", err)
	}
}
```

Run it:

```bash
go run ./cmd
```

Expected output:

```
port=8080 timeout=3s maxconns=25 debug=true
empty env: DB_DSN: required variable not set
```

### Tests

The tests are hermetic: a `fakeLookup` closure over a map means no real environment
is touched and subtests can run in parallel. The table covers the full valid set,
each missing-required key, each malformed value, out-of-range port, zero
`MAX_CONNS`, and defaults applied when optional keys are absent. Failures are
asserted with `errors.Is` against the exported sentinels, and the success case
checks every resolved field.

Create `config_test.go`:

```go
package envconfig

import (
	"errors"
	"testing"
	"time"
)

func fakeLookup(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func base() map[string]string {
	return map[string]string{
		"DB_DSN":       "postgres://localhost/app",
		"HTTP_PORT":    "8080",
		"READ_TIMEOUT": "3s",
		"MAX_CONNS":    "25",
		"DEBUG":        "true",
	}
}

func TestLoadConfigValid(t *testing.T) {
	t.Parallel()
	cfg, err := LoadConfig(fakeLookup(base()))
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	want := Config{
		DBDSN:       "postgres://localhost/app",
		HTTPPort:    8080,
		ReadTimeout: 3 * time.Second,
		MaxConns:    25,
		Debug:       true,
	}
	if cfg != want {
		t.Fatalf("cfg = %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"DB_DSN":    "postgres://localhost/app",
		"HTTP_PORT": "9000",
	}
	cfg, err := LoadConfig(fakeLookup(env))
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	if cfg.ReadTimeout != defaultReadTimeout {
		t.Fatalf("ReadTimeout = %s, want default %s", cfg.ReadTimeout, defaultReadTimeout)
	}
	if cfg.MaxConns != defaultMaxConns {
		t.Fatalf("MaxConns = %d, want default %d", cfg.MaxConns, defaultMaxConns)
	}
	if cfg.Debug {
		t.Fatal("Debug = true, want default false")
	}
}

func TestLoadConfigErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		wantErr error
	}{
		{"missing dsn", func(m map[string]string) { delete(m, "DB_DSN") }, ErrMissing},
		{"missing port", func(m map[string]string) { delete(m, "HTTP_PORT") }, ErrMissing},
		{"empty port", func(m map[string]string) { m["HTTP_PORT"] = "" }, ErrMissing},
		{"non-numeric port", func(m map[string]string) { m["HTTP_PORT"] = "abc" }, ErrInvalid},
		{"port out of range", func(m map[string]string) { m["HTTP_PORT"] = "70000" }, ErrInvalid},
		{"bad timeout", func(m map[string]string) { m["READ_TIMEOUT"] = "later" }, ErrInvalid},
		{"non-positive timeout", func(m map[string]string) { m["READ_TIMEOUT"] = "0s" }, ErrInvalid},
		{"zero maxconns", func(m map[string]string) { m["MAX_CONNS"] = "0" }, ErrInvalid},
		{"bad maxconns", func(m map[string]string) { m["MAX_CONNS"] = "lots" }, ErrInvalid},
		{"bad debug", func(m map[string]string) { m["DEBUG"] = "maybe" }, ErrInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := base()
			tc.mutate(env)
			_, err := LoadConfig(fakeLookup(env))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
			}
		})
	}
}
```

## Review

The loader is correct when it fails on the first bad or missing variable, names that
variable in the message, and keeps the sentinel matchable through `%w`. The seam
that makes it testable is the injected `lookup`: never call `os.Setenv` in a test —
pass a map. The mistakes to avoid are collapsing absent and empty into one branch
(they are `ErrMissing` versus a possible `ErrInvalid`), letting a malformed *optional*
key silently fall back to its default instead of erroring, and returning a bare
parse error the operator cannot trace to a variable. Cross-field invariants (port
range, positive timeout, `MaxConns >= 1`) belong here at startup, not scattered
through the code that later reads the config.

## Resources

- [os.LookupEnv (present-vs-empty comma-ok)](https://pkg.go.dev/os#LookupEnv)
- [strconv package](https://pkg.go.dev/strconv)
- [fmt.Errorf and %w wrapping](https://pkg.go.dev/fmt#Errorf)
- [The Twelve-Factor App: Config](https://12factor.net/config)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-guard-middleware-status-mapping.md](02-guard-middleware-status-mapping.md) | Next: [04-cache-comma-ok-ttl.md](04-cache-comma-ok-ttl.md)
