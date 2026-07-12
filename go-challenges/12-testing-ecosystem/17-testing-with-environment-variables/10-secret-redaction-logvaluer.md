# Exercise 10: Make Config Safe To Log By Redacting Secret Env Values

Dumping the config at startup is a useful diagnostic and a classic credential
leak: a DB password or API token read from the environment lands in log
aggregation the moment someone logs the struct. This exercise makes a config
struct safe to log by implementing `slog.LogValuer`, so secret fields render as
`"REDACTED"` while the rest stays visible.

## What you'll build

```text
safeconfig/                independent module: example.com/safeconfig
  go.mod                   go directive supplied by the gate
  config.go                Config; LogValue() slog.Value; String(); redact()
  cmd/
    demo/
      main.go              runnable demo: log the config, show secrets redacted
  config_test.go           log to a buffer; assert visible fields, REDACTED, no secret
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `Config` with `LogValue() slog.Value` returning a `slog.GroupValue` where secret fields are `"REDACTED"`, plus a matching `String()`.
Test: log the `Config` through a `slog.TextHandler` into a `bytes.Buffer`; assert it contains host/port and `REDACTED` but not the real secret substrings.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/17-testing-with-environment-variables/10-secret-redaction-logvaluer/cmd/demo
cd go-solutions/12-testing-ecosystem/17-testing-with-environment-variables/10-secret-redaction-logvaluer
```

## `slog.LogValuer` redacts at the source

`slog` defines the interface `LogValuer` with one method, `LogValue() slog.Value`.
When a value that implements it is logged — as an attribute value, e.g.
`logger.Info("startup", "config", cfg)` — `slog` calls `LogValue()` and logs the
returned `slog.Value` instead of the raw struct. That is the hook: return a
`slog.GroupValue` built from `slog.Attr`s where the visible fields (host, port,
timeout) carry their real values and the secret fields (DB password, API token)
carry `"REDACTED"`. The real secret never reaches the handler, so it never reaches
the log output — no matter which handler or format is configured.

`redact` returns `"REDACTED"` for any non-empty secret and `""` for an empty one,
so an unset secret does not print a misleading `REDACTED`. Implementing `String()`
with the same redaction closes the other common leak path: `fmt.Sprintf("%v",
cfg)` or `%s` uses `String()`, so a stray `fmt.Println(cfg)` is safe too. The two
methods together mean there is no ordinary way to accidentally print the secret.

A subtlety worth stating: redaction lives on the *type*, not the call site. You do
not have to remember to redact at each log statement; any code anywhere that logs
a `Config` gets the safe rendering for free. That is the whole point — security
that depends on every caller remembering is security that eventually fails.

Create `config.go`:

```go
package safeconfig

import (
	"fmt"
	"log/slog"
)

// Config carries both visible settings and secrets read from the environment.
type Config struct {
	Host       string
	Port       int
	DBPassword string // secret
	APIToken   string // secret
}

// LogValue implements slog.LogValuer so secrets never reach the log handler.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("host", c.Host),
		slog.Int("port", c.Port),
		slog.String("db_password", redact(c.DBPassword)),
		slog.String("api_token", redact(c.APIToken)),
	)
}

// String redacts too, so fmt verbs (%v, %s) cannot leak the secrets either.
func (c Config) String() string {
	return fmt.Sprintf("Config{host=%s port=%d db_password=%s api_token=%s}",
		c.Host, c.Port, redact(c.DBPassword), redact(c.APIToken))
}

// redact hides a non-empty secret; an empty secret stays empty (not "REDACTED").
func redact(s string) string {
	if s == "" {
		return ""
	}
	return "REDACTED"
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"log/slog"
	"os"

	"example.com/safeconfig"
)

func main() {
	cfg := safeconfig.Config{
		Host:       "db.internal",
		Port:       5432,
		DBPassword: os.Getenv("DB_PASSWORD"), // e.g. "s3cr3t-pw"
		APIToken:   "tok-abc123",
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		// Drop the timestamp so the demo output is stable.
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	logger.Info("startup config", "config", cfg)
}
```

Run it:

```bash
DB_PASSWORD=s3cr3t-pw go run ./cmd/demo
```

Expected output:

```
level=INFO msg="startup config" config.host=db.internal config.port=5432 config.db_password=REDACTED config.api_token=REDACTED
```

## Tests

The test logs a `Config` with real secret values through a `slog.TextHandler`
writing to a `bytes.Buffer`, then asserts on the captured output: the visible
fields (`db.internal`, `5432`) are present, `REDACTED` is present, and neither
real secret substring appears anywhere. A second test asserts `String()` is
equally safe, covering the `fmt` path.

Create `config_test.go`:

```go
package safeconfig

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestConfigRedactsInSlog(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := Config{
		Host:       "db.internal",
		Port:       5432,
		DBPassword: "s3cr3t-pw",
		APIToken:   "tok-abc123",
	}
	logger.Info("startup", "config", cfg)
	out := buf.String()

	if !strings.Contains(out, "db.internal") {
		t.Errorf("log is missing the visible host: %q", out)
	}
	if !strings.Contains(out, "5432") {
		t.Errorf("log is missing the visible port: %q", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("log does not contain REDACTED marker: %q", out)
	}
	for _, secret := range []string{"s3cr3t-pw", "tok-abc123"} {
		if strings.Contains(out, secret) {
			t.Errorf("log LEAKED secret %q: %q", secret, out)
		}
	}
}

func TestConfigStringRedacts(t *testing.T) {
	t.Parallel()

	cfg := Config{Host: "h", Port: 1, DBPassword: "pw-leak", APIToken: "tok-leak"}
	s := fmt.Sprintf("%v", cfg)

	if strings.Contains(s, "pw-leak") || strings.Contains(s, "tok-leak") {
		t.Fatalf("String() leaked a secret: %q", s)
	}
	if !strings.Contains(s, "REDACTED") {
		t.Fatalf("String() missing REDACTED: %q", s)
	}
}

func TestRedactEmptyStaysEmpty(t *testing.T) {
	t.Parallel()

	if got := redact(""); got != "" {
		t.Fatalf("redact(\"\") = %q, want empty string", got)
	}
	if got := redact("x"); got != "REDACTED" {
		t.Fatalf("redact(\"x\") = %q, want REDACTED", got)
	}
}

func ExampleConfig_String() {
	cfg := Config{Host: "db", Port: 1, DBPassword: "pw", APIToken: "tok"}
	fmt.Println(cfg)
	// Output: Config{host=db port=1 db_password=REDACTED api_token=REDACTED}
}
```

## Review

The config is safe to log when the captured output shows the visible fields and
`REDACTED` but never the real secret substrings — the assertions check both
halves, because "contains REDACTED" alone would pass even if the secret also
leaked elsewhere. The design point is that redaction is a property of the type via
`slog.LogValuer` (and `String()` for `fmt`), so it holds at every call site
without discipline at any of them. Note this test is `t.Parallel()`: it never
touches the environment, only the `Config` value — the payoff of keeping the
secret in a field rather than reading it globally at log time.

## Resources

- [slog.LogValuer](https://pkg.go.dev/log/slog#LogValuer) — the `LogValue() Value` hook slog calls before logging.
- [slog.GroupValue](https://pkg.go.dev/log/slog#GroupValue) — build a grouped value from attributes.
- [slog.Value and attributes](https://pkg.go.dev/log/slog#Value) — the value types redaction returns.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-init-time-capture-pitfall.md](09-init-time-capture-pitfall.md) | Next: [../18-integration-tests-with-build-tags/00-concepts.md](../18-integration-tests-with-build-tags/00-concepts.md)
