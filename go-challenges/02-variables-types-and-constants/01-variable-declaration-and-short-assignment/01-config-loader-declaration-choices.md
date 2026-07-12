# Exercise 1: Declaration Choices in a Config Loader

A service configuration loader is where `var`, `:=`, and `=` stop being style and
start being policy: `var cfg Config` names a zero value that is a starting policy
not a final one, explicit defaults override the unsafe zeros, narrowly scoped `:=`
intermediates confine parsed values to the branch that consumes them, and a
package-level sentinel `var` lets callers detect a missing dependency by identity.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
appconfig/                     independent module: example.com/appconfig
  go.mod                       module example.com/appconfig
  config.go                    type Config; Load(env) (Config, error); var ErrMissingDatabaseURL
  cmd/
    demo/
      main.go                  loads a sample env map and prints the resolved config
  config_test.go               defaults, overrides, sentinel via errors.Is, wrapped parse errors
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `Load(env map[string]string) (Config, error)` that starts from `var cfg Config`, applies operational defaults, layers env overrides via scoped `:=`, and exposes `ErrMissingDatabaseURL`.
- Test: defaults applied, overrides win, missing `DATABASE_URL` returns `ErrMissingDatabaseURL` via `errors.Is`, invalid `REQUEST_TIMEOUT`/`DEBUG` return wrapped errors without asserting exact strings.
- Verify: `go test -count=1 -race ./...`

### Why the zero value is a starting policy, not the final one

`Load` begins with `var cfg Config`. Every field is its zero value: `ListenAddr`
is `""`, `RequestTimeout` is `0`, `Debug` is `false`. Some of those zeros are
dangerous as operational values. A `RequestTimeout` of `0` means the server
imposes no timeout on requests — a slow-loris away from exhausting the process. So
the loader immediately writes explicit operational defaults (`":8080"`, `2s`) over
the zero value. The zero value is the defined *starting* point that guarantees no
field is uninitialized garbage; the explicit defaults are the *policy*. Never
conflate the two: "the field is zero" is not the same statement as "the operator
chose this default".

### Why `:=` scope is a deliberate lifetime decision

Each override reads its raw string in the *init clause* of an `if`, so the raw
value lives only inside that branch:

```go
if rawTimeout := env["REQUEST_TIMEOUT"]; rawTimeout != "" {
	timeout, err := time.ParseDuration(rawTimeout)
	...
}
```

`rawTimeout`, `timeout`, and this `err` all cease to exist at the closing brace.
That is intentional. A parsed timeout has no business being visible while the code
later parses `DEBUG`; confining it prevents a whole class of "reused a stale
intermediate" bugs and keeps each override a small, independent unit. Because each
`err` here is confined and handled by an immediate `return`, there is no outer
`err` to shadow — the narrow scope sidesteps the shadowing hazard entirely.

### Why the sentinel is a package-level `var`

`ErrMissingDatabaseURL` is declared with `errors.New` at package scope. It cannot
be a `const` (a function call is not a constant expression), and it must not be a
local (a caller cannot match a local by identity). Being a package-level `var` is
what lets a caller — an HTTP bootstrap, a CLI's `main`, a test — write
`errors.Is(err, config.ErrMissingDatabaseURL)` and react to *this specific*
failure without parsing a message. Parse failures are wrapped with `%w` so they
carry context (`parse REQUEST_TIMEOUT: ...`) while still matching the underlying
`time`/`strconv` error via `errors.Is`.

Create `config.go`:

```go
package config

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ErrMissingDatabaseURL is returned by Load when DATABASE_URL is not set. It is a
// package-level var so callers can match it with errors.Is instead of comparing
// strings.
var ErrMissingDatabaseURL = errors.New("DATABASE_URL is required")

// Config is the resolved service configuration. Its zero value is a valid
// starting point but not the intended operational policy; Load fills defaults.
type Config struct {
	ListenAddr     string
	DatabaseURL    string
	RequestTimeout time.Duration
	Debug          bool
}

// Load resolves configuration from an environment map. It starts from the zero
// value, applies explicit operational defaults, then layers overrides.
func Load(env map[string]string) (Config, error) {
	var cfg Config
	cfg.ListenAddr = ":8080"
	cfg.RequestTimeout = 2 * time.Second

	dsn := env["DATABASE_URL"]
	if dsn == "" {
		return Config{}, ErrMissingDatabaseURL
	}
	cfg.DatabaseURL = dsn

	if listenAddr := env["LISTEN_ADDR"]; listenAddr != "" {
		cfg.ListenAddr = listenAddr
	}

	if rawTimeout := env["REQUEST_TIMEOUT"]; rawTimeout != "" {
		timeout, err := time.ParseDuration(rawTimeout)
		if err != nil {
			return Config{}, fmt.Errorf("parse REQUEST_TIMEOUT: %w", err)
		}
		cfg.RequestTimeout = timeout
	}

	if rawDebug := env["DEBUG"]; rawDebug != "" {
		debug, err := strconv.ParseBool(rawDebug)
		if err != nil {
			return Config{}, fmt.Errorf("parse DEBUG: %w", err)
		}
		cfg.Debug = debug
	}

	return cfg, nil
}
```

### The runnable demo

Because `cmd/demo` is a separate `package main`, it can only touch the exported
API. It loads a representative environment map and prints the resolved config,
letting you watch defaults and overrides combine.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/appconfig"
)

func main() {
	cfg, err := config.Load(map[string]string{
		"DATABASE_URL":    "postgres://app@localhost/app",
		"REQUEST_TIMEOUT": "750ms",
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("listen=%s\n", cfg.ListenAddr)
	fmt.Printf("timeout=%s\n", cfg.RequestTimeout)
	fmt.Printf("debug=%t\n", cfg.Debug)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
listen=:8080
timeout=750ms
debug=false
```

The `ListenAddr` shows the default (no `LISTEN_ADDR` supplied), the timeout shows
the override, and `Debug` shows the zero value surviving because no `DEBUG` was
set.

### Tests

The tests assert *behavior*, never exact error strings: defaults are applied when
env is sparse, overrides win when present, a missing `DATABASE_URL` is detectable
by identity, and a malformed value fails without the test depending on wording.

Create `config_test.go`:

```go
package config

import (
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"
)

func TestLoadAppliesDefaultsAndOverrides(t *testing.T) {
	t.Parallel()

	got, err := Load(map[string]string{
		"DATABASE_URL":    "postgres://app@localhost/app",
		"LISTEN_ADDR":     ":9090",
		"REQUEST_TIMEOUT": "750ms",
		"DEBUG":           "true",
	})
	if err != nil {
		t.Fatal(err)
	}

	want := Config{
		ListenAddr:     ":9090",
		DatabaseURL:    "postgres://app@localhost/app",
		RequestTimeout: 750 * time.Millisecond,
		Debug:          true,
	}
	if got != want {
		t.Fatalf("Config = %+v, want %+v", got, want)
	}
}

func TestLoadUsesOperationalDefaults(t *testing.T) {
	t.Parallel()

	got, err := Load(map[string]string{
		"DATABASE_URL": "postgres://app@localhost/app",
	})
	if err != nil {
		t.Fatal(err)
	}

	if got.ListenAddr != ":8080" {
		t.Fatalf("ListenAddr = %q, want :8080", got.ListenAddr)
	}
	if got.RequestTimeout != 2*time.Second {
		t.Fatalf("RequestTimeout = %s, want 2s", got.RequestTimeout)
	}
	if got.Debug {
		t.Fatal("Debug should default to false")
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Parallel()

	_, err := Load(nil)
	if !errors.Is(err, ErrMissingDatabaseURL) {
		t.Fatalf("error = %v, want ErrMissingDatabaseURL", err)
	}
}

func TestLoadWrapsParseErrors(t *testing.T) {
	t.Parallel()

	cases := map[string]map[string]string{
		"bad timeout": {
			"DATABASE_URL":    "postgres://app@localhost/app",
			"REQUEST_TIMEOUT": "soon",
		},
		"bad debug": {
			"DATABASE_URL": "postgres://app@localhost/app",
			"DEBUG":        "not-a-bool",
		},
	}

	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(env)
			if err == nil {
				t.Fatal("expected an error for invalid input")
			}
		})
	}
}

func TestLoadWrappedDebugMatchesStrconv(t *testing.T) {
	t.Parallel()

	_, err := Load(map[string]string{
		"DATABASE_URL": "postgres://app@localhost/app",
		"DEBUG":        "maybe",
	})
	var numErr *strconv.NumError
	if !errors.As(err, &numErr) {
		t.Fatalf("error = %v, want a wrapped *strconv.NumError", err)
	}
}

func ExampleLoad() {
	cfg, _ := Load(map[string]string{
		"DATABASE_URL": "postgres://app@localhost/app",
	})
	fmt.Println(cfg.ListenAddr, cfg.RequestTimeout, cfg.Debug)
	// Output: :8080 2s false
}
```

The last test proves the `%w` wrapping is real: `errors.As` unwraps
`parse DEBUG: ...` down to the concrete `*strconv.NumError` that `ParseBool`
returned, which only works because the chain was preserved.

## Review

The loader is correct when the zero value is treated as a starting point rather
than a policy: `var cfg Config` then explicit `":8080"` and `2s` defaults, so a
sparse environment yields safe operational values instead of an empty listen
address and a zero (unlimited) timeout. The `:=` intermediates are confined to
their `if` init clauses, which is what keeps parsed values from leaking and
sidesteps shadowing. The sentinel is a package-level `var` so
`errors.Is(err, ErrMissingDatabaseURL)` works, and parse failures are wrapped with
`%w` so `errors.As` can still reach the underlying `strconv`/`time` error.

The mistakes to avoid: do not declare the sentinel as a local or compare
`err.Error()` strings; do not push these values into mutable package globals in an
`init()` (return the `Config` value instead); and do not assume the zero value is
the intended default. Run `go test -race` to confirm the behavioral contract, and
add a case with `LISTEN_ADDR` empty to prove the default survives.

## Resources

- [Go Specification: Variable declarations](https://go.dev/ref/spec#Variable_declarations)
- [Go Specification: Short variable declarations](https://go.dev/ref/spec#Short_variable_declarations)
- [errors package (errors.New, Is, As)](https://pkg.go.dev/errors)
- [strconv.ParseBool](https://pkg.go.dev/strconv#ParseBool)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-sentinel-errors-repository-layer.md](02-sentinel-errors-repository-layer.md)
