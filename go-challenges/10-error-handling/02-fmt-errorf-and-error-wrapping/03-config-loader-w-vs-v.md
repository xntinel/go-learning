# Exercise 3: Decide %w versus %v deliberately in a config loader that must not leak secrets

A config loader is a security boundary as much as a parsing one. When an ordinary
field fails to parse you want the error fully inspectable — a caller should be
able to `errors.Is` it against a `ErrMalformedConfig` sentinel and read the
strconv cause. But when a *secret-bearing* field is bad, the raw value must never
enter the error string, because that string ends up in logs. This exercise builds
a loader that wraps ordinary failures with `%w` and constructs secret-field
failures with `%v` plus a redacted placeholder — the `%w`-inspectable versus
`%v`-severed decision made deliberately at a security boundary.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
config/                        independent module: example.com/config
  go.mod                       go 1.24
  config.go                    ErrMalformedConfig; LoadConfig(io.Reader) with redaction on secret fields
  config_test.go               malformed numeric wraps sentinel+cause; bad secret redacted; valid parses
  cmd/
    demo/
      main.go                  loads a valid config and two malformed ones
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `LoadConfig(r io.Reader)` parsing `KEY=VALUE` lines; a malformed numeric field wraps `ErrMalformedConfig` and the `strconv` cause with multiple `%w`; a malformed secret field wraps `ErrMalformedConfig` with `%w` but renders the value as a redacted placeholder via `%v`.
- Test: malformed numeric is `errors.Is(err, ErrMalformedConfig)` and names the key; bad secret is `errors.Is(err, ErrMalformedConfig)` but `err.Error()` does not contain the secret and does contain `redacted`; valid config parses to the right struct with `nil`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/config/cmd/demo
cd ~/go-exercises/config
go mod init example.com/config
```

### Two wrap decisions, one loader

The loader parses `KEY=VALUE` lines with `strings.Cut`, trimming whitespace and
skipping blanks and `#` comments. For `MAX_CONNS` it calls `strconv.Atoi`; the
value there is not sensitive, so on failure it wraps *both* the
`ErrMalformedConfig` sentinel and the `strconv` cause using multiple `%w`:
`fmt.Errorf("config key %s=%q: %w: %w", key, val, ErrMalformedConfig, err)`. That
gives a caller two inspection points — `errors.Is(err, ErrMalformedConfig)` for
the category and `errors.As` on a `*strconv.NumError` for the parse detail — and
puts the offending value in the message because it is safe to log.

For `DB_PASSWORD` the decision inverts. The value is a credential, so it must not
appear in the error string under any circumstance. On a bad secret (here: shorter
than the minimum length) the loader constructs `fmt.Errorf("config key %s=%s (too
short): %w", key, redacted, ErrMalformedConfig)`. The `%s` renders the constant
`[redacted]` placeholder, never the value; the `%w` still wraps
`ErrMalformedConfig`, so a caller's `errors.Is` branch works exactly as it does
for the numeric field. The category stays inspectable; the secret stays out of the
logs. That is the deliberate `%w`-versus-`%v` decision the concept file describes,
made at the point where getting it wrong leaks credentials.

Create `config.go`:

```go
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrMalformedConfig is the category sentinel callers branch on regardless of
// which field failed.
var ErrMalformedConfig = errors.New("malformed config")

// redacted stands in for any secret value in an error message.
const redacted = "[redacted]"

// minSecretLen is the smallest acceptable DB_PASSWORD length.
const minSecretLen = 8

type Config struct {
	MaxConns   int
	DBPassword string
}

// LoadConfig parses KEY=VALUE lines. Ordinary parse failures are wrapped so the
// cause stays inspectable; secret-field failures are redacted so the value never
// enters the error string.
func LoadConfig(r io.Reader) (Config, error) {
	var cfg Config
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return Config{}, fmt.Errorf("config line %q: %w", line, ErrMalformedConfig)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		switch key {
		case "MAX_CONNS":
			n, err := strconv.Atoi(val)
			if err != nil {
				// Non-secret: wrap the sentinel AND the strconv cause with two %w
				// so callers can inspect both. The value is safe to log.
				return Config{}, fmt.Errorf("config key %s=%q: %w: %w", key, val, ErrMalformedConfig, err)
			}
			cfg.MaxConns = n
		case "DB_PASSWORD":
			if len(val) < minSecretLen {
				// Secret: never put the value in the message. Redact it, but still
				// wrap the sentinel so errors.Is works like any other field.
				return Config{}, fmt.Errorf("config key %s=%s (too short): %w", key, redacted, ErrMalformedConfig)
			}
			cfg.DBPassword = val
		default:
			return Config{}, fmt.Errorf("config key %s: unknown: %w", key, ErrMalformedConfig)
		}
	}
	if err := sc.Err(); err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return cfg, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"strings"

	"example.com/config"
)

func main() {
	valid := "MAX_CONNS=10\nDB_PASSWORD=supersecretpw\n"
	cfg, err := config.LoadConfig(strings.NewReader(valid))
	fmt.Printf("valid: MaxConns=%d err=%v\n", cfg.MaxConns, err)

	_, err = config.LoadConfig(strings.NewReader("MAX_CONNS=abc\n"))
	fmt.Printf("bad numeric: %v\n", err)
	fmt.Printf("  is ErrMalformedConfig=%v\n", errors.Is(err, config.ErrMalformedConfig))

	_, err = config.LoadConfig(strings.NewReader("DB_PASSWORD=abc\n"))
	fmt.Printf("bad secret: %v\n", err)
	fmt.Printf("  leaks value=%v  is ErrMalformedConfig=%v\n",
		strings.Contains(err.Error(), "abc"), errors.Is(err, config.ErrMalformedConfig))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid: MaxConns=10 err=<nil>
bad numeric: config key MAX_CONNS="abc": malformed config: strconv.Atoi: parsing "abc": invalid syntax
  is ErrMalformedConfig=true
bad secret: config key DB_PASSWORD=[redacted] (too short): malformed config
  leaks value=false  is ErrMalformedConfig=true
```

### Tests

The security-critical assertions are the two on the secret path:
`!strings.Contains(err.Error(), secret)` proves the value did not leak, and
`strings.Contains(err.Error(), "redacted")` proves the placeholder is there. The
numeric path asserts the sentinel is reachable and, because it used multiple `%w`,
that the strconv cause is reachable too.

Create `config_test.go`:

```go
package config

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestLoadConfigValid(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(strings.NewReader("MAX_CONNS=25\nDB_PASSWORD=longenough\n"))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if cfg.MaxConns != 25 {
		t.Fatalf("MaxConns = %d, want 25", cfg.MaxConns)
	}
	if cfg.DBPassword != "longenough" {
		t.Fatalf("DBPassword = %q, want longenough", cfg.DBPassword)
	}
}

func TestLoadConfigMalformedNumeric(t *testing.T) {
	t.Parallel()

	_, err := LoadConfig(strings.NewReader("MAX_CONNS=abc\n"))

	if !errors.Is(err, ErrMalformedConfig) {
		t.Fatalf("err = %v, want errors.Is ErrMalformedConfig", err)
	}
	if !strings.Contains(err.Error(), "MAX_CONNS") {
		t.Fatalf("err.Error() = %q, want it to name the key", err.Error())
	}

	// The strconv cause is reachable because we used multiple %w.
	var numErr *strconv.NumError
	if !errors.As(err, &numErr) {
		t.Fatal("err should also carry the *strconv.NumError cause")
	}
}

func TestLoadConfigRedactsSecret(t *testing.T) {
	t.Parallel()

	const secret = "hunter2"
	_, err := LoadConfig(strings.NewReader("DB_PASSWORD=" + secret + "\n"))

	if !errors.Is(err, ErrMalformedConfig) {
		t.Fatalf("err = %v, want errors.Is ErrMalformedConfig", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("err.Error() = %q leaked the secret value", err.Error())
	}
	if !strings.Contains(err.Error(), "redacted") {
		t.Fatalf("err.Error() = %q, want the redacted placeholder", err.Error())
	}
}
```

## Review

The loader is correct when both categories of failure are `errors.Is(err,
ErrMalformedConfig)` yet only the non-secret category exposes its value. The trap
this exercise guards against is treating redaction as a formatting nicety you can
bolt on later: the natural first draft writes `fmt.Errorf("bad DB_PASSWORD=%s:
%w", val, ErrMalformedConfig)`, which compiles, passes a naive test, and quietly
prints the credential into every log line for the rest of the service's life. The
`!strings.Contains(err.Error(), secret)` assertion is the regression guard that
makes the leak impossible to reintroduce unnoticed. The multiple-`%w` on the
numeric path is the contrast: there, exposing the value and the strconv cause is
correct, because a config parse error a human must fix should carry as much detail
as possible.

## Resources

- [fmt.Errorf](https://pkg.go.dev/fmt#Errorf) — `%w` versus `%v`, multiple `%w`.
- [strings.Cut](https://pkg.go.dev/strings#Cut) — splitting `KEY=VALUE` once.
- [strconv#NumError](https://pkg.go.dev/strconv#NumError) — the typed parse error `errors.As` recovers.
- [errors package](https://pkg.go.dev/errors) — `Is` and `As` traversal.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-repo-translate-not-leak.md](02-repo-translate-not-leak.md) | Next: [04-fanout-multi-w-partial-failure.md](04-fanout-multi-w-partial-failure.md)
