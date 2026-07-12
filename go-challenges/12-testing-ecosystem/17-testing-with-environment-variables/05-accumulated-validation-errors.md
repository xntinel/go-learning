# Exercise 5: Report Every Config Error At Startup, Not Just The First

A loader that bails on the first bad variable forces an operator into a
fix-restart-repeat loop. This exercise builds a `LoadAll` that validates every
field and returns a single joined error via `errors.Join`, so one boot log
surfaces every misconfiguration — while each underlying sentinel stays matchable
with `errors.Is`.

## What you'll build

```text
startupcfg/                independent module: example.com/startupcfg
  go.mod                   go directive supplied by the gate
  config.go                LoadAll() (errors.Join) and LoadFirst() (first-error-wins)
  cmd/
    demo/
      main.go              runnable demo: three bad vars, all reported at once
  config_test.go           assert Is for every sentinel simultaneously; contrast LoadFirst
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `LoadAll()` accumulating field errors into `errors.Join`, and a `LoadFirst()` that returns on the first error for contrast.
Test: set several invalid vars at once; assert the joined error satisfies `errors.Is` for each sentinel and lists every key; show `LoadFirst` reports only one.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/17-testing-with-environment-variables/05-accumulated-validation-errors/cmd/demo
cd go-solutions/12-testing-ecosystem/17-testing-with-environment-variables/05-accumulated-validation-errors
```

## Accumulate, then join

The pattern is mechanical but the payoff is large. Instead of returning at the
first problem, `LoadAll` appends each field's failure to a `[]error` and continues
validating the rest; at the end, if the slice is non-empty, it returns
`errors.Join(errs...)`. `errors.Join` builds a single error whose `Error()`
concatenates the messages one per line, and whose `Is`/`As` traversal visits
every joined element — so `errors.Is(joined, ErrMissingHost)` and
`errors.Is(joined, ErrInvalidPort)` can *both* be true of the same returned error.
The caller loses nothing: it can still branch on any specific sentinel, and it
gains a message that lists every offending key at once.

Contrast `LoadFirst`, the naive shape, which returns on the first failure. If a
deployment has a missing host, a bad port, and a bad timeout, `LoadFirst` reports
only the missing host; the operator fixes it, reboots, and only then learns about
the port. Three round-trips to surface three errors that were all knowable at the
first boot. `LoadAll` collapses that to one.

One subtlety: when a field fails to parse, `LoadAll` records the error and leaves
that field at its zero value rather than aborting, so validation of the *other*
fields still runs. The returned `Config` is only meaningful when the error is
`nil`; on failure the caller reads the error, not the struct.

Create `config.go`:

```go
package startupcfg

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// One sentinel per field failure.
var (
	ErrMissingHost     = errors.New("APP_HOST is missing")
	ErrInvalidPort     = errors.New("invalid port")
	ErrInvalidDuration = errors.New("invalid duration")
)

// Config is the resolved startup configuration.
type Config struct {
	Host    string
	Port    int
	Timeout time.Duration
}

// LoadAll validates every field and returns all failures joined into one error.
// The joined error stays matchable per sentinel via errors.Is.
func LoadAll() (Config, error) {
	var cfg Config
	var errs []error

	host := os.Getenv("APP_HOST")
	if host == "" {
		errs = append(errs, fmt.Errorf("APP_HOST: %w", ErrMissingHost))
	} else {
		cfg.Host = host
	}

	portStr := os.Getenv("APP_PORT")
	if port, err := strconv.Atoi(portStr); err != nil || port <= 0 {
		errs = append(errs, fmt.Errorf("APP_PORT=%q: %w", portStr, ErrInvalidPort))
	} else {
		cfg.Port = port
	}

	timeoutStr := os.Getenv("APP_TIMEOUT")
	if d, err := time.ParseDuration(timeoutStr); err != nil {
		errs = append(errs, fmt.Errorf("APP_TIMEOUT=%q: %w", timeoutStr, ErrInvalidDuration))
	} else {
		cfg.Timeout = d
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// LoadFirst is the first-error-wins variant kept for contrast: it returns as
// soon as one field is invalid, hiding the rest until the next boot.
func LoadFirst() (Config, error) {
	var cfg Config

	host := os.Getenv("APP_HOST")
	if host == "" {
		return Config{}, fmt.Errorf("APP_HOST: %w", ErrMissingHost)
	}
	cfg.Host = host

	portStr := os.Getenv("APP_PORT")
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return Config{}, fmt.Errorf("APP_PORT=%q: %w", portStr, ErrInvalidPort)
	}
	cfg.Port = port

	timeoutStr := os.Getenv("APP_TIMEOUT")
	d, err := time.ParseDuration(timeoutStr)
	if err != nil {
		return Config{}, fmt.Errorf("APP_TIMEOUT=%q: %w", timeoutStr, ErrInvalidDuration)
	}
	cfg.Timeout = d

	return cfg, nil
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/startupcfg"
)

func main() {
	// A deployment with three problems at once.
	os.Setenv("APP_HOST", "")
	os.Setenv("APP_PORT", "-1")
	os.Setenv("APP_TIMEOUT", "30") // missing unit

	_, err := startupcfg.LoadAll()
	fmt.Println("LoadAll reports every problem:")
	fmt.Println(err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
LoadAll reports every problem:
APP_HOST: APP_HOST is missing
APP_PORT="-1": invalid port
APP_TIMEOUT="30": invalid duration
```

## Tests

The key assertion is that one returned error satisfies `errors.Is` for all three
sentinels *simultaneously* — the property `errors.Join` provides and a
first-error-wins loader cannot. A second assertion checks the `Error()` text names
every offending key, so the operator-facing message is complete. The contrast
test runs `LoadFirst` against the same broken environment and shows it matches
only the first sentinel.

Create `config_test.go`:

```go
package startupcfg

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func setBroken(t *testing.T) {
	t.Helper()
	t.Setenv("APP_HOST", "")
	t.Setenv("APP_PORT", "-1")
	t.Setenv("APP_TIMEOUT", "30")
}

func TestLoadAllReportsEverything(t *testing.T) {
	setBroken(t)

	_, err := LoadAll()
	if err == nil {
		t.Fatal("LoadAll() = nil error, want joined error")
	}

	for _, want := range []error{ErrMissingHost, ErrInvalidPort, ErrInvalidDuration} {
		if !errors.Is(err, want) {
			t.Errorf("joined error does not match %v", want)
		}
	}
	for _, key := range []string{"APP_HOST", "APP_PORT", "APP_TIMEOUT"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error text %q does not mention %s", err.Error(), key)
		}
	}
}

func TestLoadAllSuccess(t *testing.T) {
	t.Setenv("APP_HOST", "api.internal")
	t.Setenv("APP_PORT", "8080")
	t.Setenv("APP_TIMEOUT", "3s")

	cfg, err := LoadAll()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{Host: "api.internal", Port: 8080, Timeout: 3_000_000_000}
	if cfg != want {
		t.Fatalf("cfg = %+v, want %+v", cfg, want)
	}
}

func TestLoadFirstHidesLaterErrors(t *testing.T) {
	setBroken(t)

	_, err := LoadFirst()
	if !errors.Is(err, ErrMissingHost) {
		t.Fatalf("LoadFirst() err = %v, want ErrMissingHost first", err)
	}
	// First-error-wins: the port and timeout failures are invisible here.
	if errors.Is(err, ErrInvalidPort) || errors.Is(err, ErrInvalidDuration) {
		t.Fatalf("LoadFirst() unexpectedly reported more than the first error: %v", err)
	}
}

func ExampleLoadAll() {
	os.Setenv("APP_HOST", "api.internal")
	os.Setenv("APP_PORT", "8080")
	os.Setenv("APP_TIMEOUT", "3s")

	cfg, err := LoadAll()
	fmt.Println(cfg.Host, cfg.Port, cfg.Timeout, err)
	// Output: api.internal 8080 3s <nil>
}
```

## Review

`LoadAll` is correct when its single returned error matches every field sentinel
through `errors.Join`'s traversal and its message lists every key — that is the
whole difference from `LoadFirst`, which the contrast test shows reports only the
first problem. The design rule to carry forward: at startup, validation is
cheap and reboots are expensive, so accumulate all failures and join them.
`errors.Join` keeps the result both human-readable (one line per failure) and
machine-matchable (`errors.Is` per sentinel), so you give up nothing by batching.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — combines multiple errors into one that `Is`/`As` can traverse.
- [errors.Is](https://pkg.go.dev/errors#Is) — matches any sentinel inside a joined error.
- [The Twelve-Factor App: Config](https://12factor.net/config) — config validation as a startup concern.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-typed-env-parsing.md](04-typed-env-parsing.md) | Next: [06-getenv-injection-parallel.md](06-getenv-injection-parallel.md)
