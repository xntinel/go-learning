# Exercise 1: Validate a Config Constructor and Aggregate Every Error with errors.Join

The entrypoint of a deployed service takes three untyped strings — a host, a port,
a mode — and must turn them into a trusted `Config` or reject them with an error
an operator can act on in one pass. This exercise builds that constructor: split
per-field parse helpers, package-level sentinels, and `errors.Join` so a config
with three typos reports all three at once instead of one per redeploy.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
configload/                  independent module: example.com/config-load-errors-join
  go.mod
  config.go                  Config, Load, parsePort, parseMode, sentinel errors
  cmd/
    demo/
      main.go                loads a valid config, then a triply-broken one
  config_test.go             table-driven: valid, each sentinel, joined, trim, all modes
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `Load(host, port, mode string) (Config, error)` that validates every field, aggregates failures with `errors.Join`, and wraps sentinels with `%w`.
- Test: valid input; each sentinel via `errors.Is` under empty host / non-numeric / out-of-range port / unknown mode; all three sentinels found in the joined error; whitespace trimmed; message mentions host, port, mode; all three modes accepted.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/06-constructor-functions-and-validation/01-config-load-errors-join/cmd/demo
cd go-solutions/07-structs-and-methods/06-constructor-functions-and-validation/01-config-load-errors-join
```

### Why aggregate instead of fail-fast

The naive constructor validates the host, returns if it is empty, then validates
the port, and so on. Each `return` short-circuits the rest, so a config with a
missing host, a non-numeric port, and an unknown mode reports only the host on the
first run. The operator fixes it, redeploys, learns about the port, redeploys, and
learns about the mode — three deploy cycles for three typos that were all visible
at once. Accumulating into a `[]error` and returning `errors.Join(errs...)` at the
end turns that into a single round trip. `errors.Join` returns `nil` when the slice
is empty, so the happy path falls through to the real return with no special case.

The second design choice is that each field's failure is a wrapped sentinel.
`parsePort` returns `fmt.Errorf("%w: %d out of range", ErrInvalidPort, n)`: the
`%w` keeps `errors.Is(err, ErrInvalidPort)` true even after the error is buried
inside the joined error, while the trailing text gives the operator the specific
offending value. The caller branches on the sentinel; the human reads the message.
Splitting `parsePort` and `parseMode` into their own functions keeps each pure and
unit-testable and keeps `Load` a readable list of steps.

Create `config.go`:

```go
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	ErrMissingHost = errors.New("host is required")
	ErrInvalidPort = errors.New("port must be between 1 and 65535")
	ErrInvalidMode = errors.New("mode must be one of: dev, staging, prod")
)

// Mode is the deployment mode of the service.
type Mode string

const (
	ModeDev     Mode = "dev"
	ModeStaging Mode = "staging"
	ModeProd    Mode = "prod"
)

// Config is a validated service configuration. A Config that exists has already
// passed every check in Load; downstream code never re-validates it.
type Config struct {
	Host string
	Port int
	Mode Mode
}

// Load parses and validates the three raw inputs, returning either a valid
// Config or errors.Join of every problem found.
func Load(host, port, mode string) (Config, error) {
	var errs []error

	host = strings.TrimSpace(host)
	if host == "" {
		errs = append(errs, ErrMissingHost)
	}

	portInt, err := parsePort(port)
	if err != nil {
		errs = append(errs, err)
	}

	modeEnum, err := parseMode(mode)
	if err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}

	return Config{Host: host, Port: portInt, Mode: modeEnum}, nil
}

func parsePort(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("%w: empty", ErrInvalidPort)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a number", ErrInvalidPort, raw)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("%w: %d out of range", ErrInvalidPort, n)
	}
	return n, nil
}

func parseMode(raw string) (Mode, error) {
	raw = strings.TrimSpace(raw)
	switch Mode(raw) {
	case ModeDev, ModeStaging, ModeProd:
		return Mode(raw), nil
	case "":
		return "", fmt.Errorf("%w: empty", ErrInvalidMode)
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidMode, raw)
	}
}
```

### The runnable demo

The demo loads a valid config, then a config broken in all three fields, and
prints the joined error so you can see every problem reported at once. Because
`cmd/demo` is a separate `package main`, it reaches the config only through the
exported `Load` and the exported `Config` fields.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	config "example.com/config-load-errors-join"
)

func main() {
	c, err := config.Load("api.example.com", "8080", "prod")
	if err != nil {
		fmt.Println("unexpected:", err)
		return
	}
	fmt.Printf("loaded: %s:%d mode=%s\n", c.Host, c.Port, c.Mode)

	_, err = config.Load("", "70000", "weird")
	fmt.Println("errors:")
	fmt.Println(err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
loaded: api.example.com:8080 mode=prod
errors:
host is required
port must be between 1 and 65535: 70000 out of range
mode must be one of: dev, staging, prod: "weird"
```

### Tests

`TestLoadJoinsAllErrors` is the centerpiece: it feeds a triply-broken config and
asserts `errors.Is` finds all three sentinels inside the single joined error,
proving aggregation works. The rest pin each individual branch, the whitespace
trim, the human-readable message, and the full set of accepted modes.

Create `config_test.go`:

```go
package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestLoadAcceptsValidInput(t *testing.T) {
	t.Parallel()
	c, err := Load("api.example.com", "8080", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "api.example.com" || c.Port != 8080 || c.Mode != ModeDev {
		t.Fatalf("Config = %+v", c)
	}
}

func TestLoadRejectsEmptyHost(t *testing.T) {
	t.Parallel()
	_, err := Load("", "8080", "dev")
	if !errors.Is(err, ErrMissingHost) {
		t.Fatalf("err = %v, want ErrMissingHost", err)
	}
}

func TestLoadRejectsInvalidPort(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"empty":        "",
		"non-numeric":  "abc",
		"out of range": "70000",
	}
	for name, port := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load("host", port, "dev")
			if !errors.Is(err, ErrInvalidPort) {
				t.Fatalf("err = %v, want ErrInvalidPort", err)
			}
		})
	}
}

func TestLoadRejectsInvalidMode(t *testing.T) {
	t.Parallel()
	_, err := Load("host", "8080", "weird")
	if !errors.Is(err, ErrInvalidMode) {
		t.Fatalf("err = %v, want ErrInvalidMode", err)
	}
}

func TestLoadJoinsAllErrors(t *testing.T) {
	t.Parallel()
	_, err := Load("", "abc", "weird")
	for _, want := range []error{ErrMissingHost, ErrInvalidPort, ErrInvalidMode} {
		if !errors.Is(err, want) {
			t.Fatalf("joined err %v should include %v", err, want)
		}
	}
}

func TestLoadTrimsWhitespace(t *testing.T) {
	t.Parallel()
	c, err := Load("  host  ", "  8080  ", "  dev  ")
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "host" || c.Port != 8080 || c.Mode != ModeDev {
		t.Fatalf("Config = %+v", c)
	}
}

func TestErrorMessageMentionsAllFields(t *testing.T) {
	t.Parallel()
	_, err := Load("", "abc", "weird")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"host", "port", "mode"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("msg should mention %q: %s", want, msg)
		}
	}
}

func TestLoadAcceptsAllValidModes(t *testing.T) {
	t.Parallel()
	for _, m := range []Mode{ModeDev, ModeStaging, ModeProd} {
		c, err := Load("host", "8080", string(m))
		if err != nil {
			t.Fatalf("mode %q rejected: %v", m, err)
		}
		if c.Mode != m {
			t.Fatalf("mode = %q, want %q", c.Mode, m)
		}
	}
}

func ExampleLoad() {
	c, _ := Load("api.example.com", "8080", "prod")
	fmt.Printf("%s:%d %s\n", c.Host, c.Port, c.Mode)
	// Output: api.example.com:8080 prod
}
```

## Review

The constructor is correct when a valid triple builds the expected `Config` and
any broken field surfaces its sentinel through `errors.Is` even after joining.
The mistake this exercise exists to prevent is fail-fast validation: returning on
the first bad field hides the rest and costs the operator a redeploy per typo.
The second is the generic-string error — `errors.New("bad config")` — which forces
callers to string-match; wrapping a sentinel with `%w` keeps the identity stable
while the message stays human. Confirm `errors.Is` still finds each sentinel after
`errors.Join`, which is exactly what `Unwrap() []error` on the joined error
provides.

## Resources

- [errors package (Join, Is)](https://pkg.go.dev/errors) — `errors.Join` and its `Unwrap() []error` contract.
- [strconv package](https://pkg.go.dev/strconv) — `strconv.Atoi` for the port.
- [Effective Go: Errors](https://go.dev/doc/effective_go#errors) — sentinel-and-wrap conventions.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-env-config-loader-defaults.md](02-env-config-loader-defaults.md)
