# Exercise 4: Parse a LOG_LEVEL Env Value Into an slog.Level

A service's log verbosity is almost always driven by a `LOG_LEVEL` environment
variable, and the parser that turns that string into an `slog.Level` runs exactly
once, at startup. This module builds it as a normalized expression switch whose
`default` fails loudly — so a typo in a deploy config crashes the boot instead of
silently pinning the service at the wrong verbosity for weeks.

This module is fully self-contained: its own `go mod init`, code, demo, and
tests.

## What you'll build

```text
logcfg/                    independent module: example.com/log-level-config-loader
  go.mod                   go 1.24
  logcfg.go                ParseLevel(raw) (slog.Level, error); MustLevel(raw)
  cmd/
    demo/
      main.go              runnable demo parsing several spellings
  logcfg_test.go           table over spellings/whitespace/case + numeric-value checks
```

- Files: `logcfg.go`, `cmd/demo/main.go`, `logcfg_test.go`.
- Implement: `ParseLevel(raw string) (slog.Level, error)` (normalized expression switch, wrapped-error default) and `MustLevel(raw string) slog.Level` (falls back to `LevelInfo`).
- Test: a table asserting each accepted spelling maps to the right level, unknown values return a wrapped sentinel error, `MustLevel` falls back for bad input, and the numeric level values hold.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Normalize first, then fail loud

The subject here is a stringly value from the environment, so the two hazards are
the ones the concepts file warns about. First, matching is exact:
case-sensitive and whitespace-sensitive. `"INFO"`, `" info"`, and `"Info"` are
three distinct strings, and an operator will absolutely set one of them. The fix
is to normalize with `strings.ToLower(strings.TrimSpace(raw))` *before* the
switch, so the case list matches regardless of surrounding whitespace or casing.

Second, the `default` is a policy decision. A tempting but dangerous choice is to
default an unknown level to `LevelInfo` — that turns `LOG_LEVEL=debgu` (a typo)
into a silent production info-level service when the operator wanted debug output
to diagnose an incident. `ParseLevel` therefore fails closed with a wrapped
`ErrUnknownLevel`, so startup can refuse to boot on a misconfigured level. The
`MustLevel` wrapper exists for the genuinely-optional case where a missing or
malformed value should quietly fall back to `LevelInfo` — but that fallback is an
explicit, named choice, not the default buried inside the parser.

`slog`'s levels are ordered integers — `LevelDebug = -4`, `LevelInfo = 0`,
`LevelWarn = 4`, `LevelError = 8` — with gaps left for custom intermediate
levels. The test pins those numeric values because the ordering is what makes
`logger.Enabled(ctx, level)` comparisons work; if a parser returned the wrong
constant, comparisons would silently include or drop records.

Create `logcfg.go`:

```go
package logcfg

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// ErrUnknownLevel is returned for a LOG_LEVEL value that does not name a level.
var ErrUnknownLevel = errors.New("unknown log level")

// ParseLevel maps a raw LOG_LEVEL string to an slog.Level. It normalizes case
// and whitespace, then dispatches with an expression switch whose default fails
// closed so a typo surfaces at startup instead of silently defaulting.
func ParseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("%w: %q", ErrUnknownLevel, raw)
	}
}

// MustLevel is for optional config: it returns the parsed level, or LevelInfo
// when raw is empty or unrecognized. The fallback is an explicit, named choice,
// not a silent default hidden inside ParseLevel.
func MustLevel(raw string) slog.Level {
	level, err := ParseLevel(raw)
	if err != nil {
		return slog.LevelInfo
	}
	return level
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/log-level-config-loader"
)

func main() {
	for _, raw := range []string{"debug", " INFO ", "Warning", "error", "verbose"} {
		level, err := logcfg.ParseLevel(raw)
		if err != nil {
			fmt.Printf("%-10q -> error: %v\n", raw, err)
			continue
		}
		fmt.Printf("%-10q -> %s (%d)\n", raw, level, int(level))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"debug"    -> DEBUG (-4)
" INFO "   -> INFO (0)
"Warning"  -> WARN (4)
"error"    -> ERROR (8)
"verbose"  -> error: unknown log level: "verbose"
```

### Tests

`TestParseLevel` proves normalization by feeding whitespace and mixed-case
spellings and asserting the exact `slog.Level`, and proves the fail-closed
default with `errors.Is(err, ErrUnknownLevel)`. `TestNumericValues` pins the
integer values so ordering comparisons remain valid. `TestMustLevel` shows the
optional-config fallback returns `LevelInfo` for empty and invalid input.

Create `logcfg_test.go`:

```go
package logcfg

import (
	"errors"
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw       string
		want      slog.Level
		wantError bool
	}{
		{"debug", slog.LevelDebug, false},
		{"DEBUG", slog.LevelDebug, false},
		{" info ", slog.LevelInfo, false},
		{"Info", slog.LevelInfo, false},
		{"warn", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{"WARNING", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"", 0, true},
		{"verbose", 0, true},
		{"trace", 0, true},
	}

	for _, tc := range tests {
		got, err := ParseLevel(tc.raw)
		if tc.wantError {
			if !errors.Is(err, ErrUnknownLevel) {
				t.Errorf("ParseLevel(%q) err = %v, want errors.Is ErrUnknownLevel", tc.raw, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseLevel(%q) err = %v, want nil", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestNumericValues(t *testing.T) {
	t.Parallel()

	pairs := []struct {
		level slog.Level
		want  int
	}{
		{slog.LevelDebug, -4},
		{slog.LevelInfo, 0},
		{slog.LevelWarn, 4},
		{slog.LevelError, 8},
	}
	for _, p := range pairs {
		if int(p.level) != p.want {
			t.Errorf("level %v numeric = %d, want %d", p.level, int(p.level), p.want)
		}
	}
	if !(slog.LevelDebug < slog.LevelInfo && slog.LevelInfo < slog.LevelWarn && slog.LevelWarn < slog.LevelError) {
		t.Error("slog levels are not strictly ordered debug < info < warn < error")
	}
}

func TestMustLevel(t *testing.T) {
	t.Parallel()

	if got := MustLevel(""); got != slog.LevelInfo {
		t.Errorf("MustLevel(\"\") = %v, want LevelInfo", got)
	}
	if got := MustLevel("nonsense"); got != slog.LevelInfo {
		t.Errorf("MustLevel(nonsense) = %v, want LevelInfo", got)
	}
	if got := MustLevel("error"); got != slog.LevelError {
		t.Errorf("MustLevel(error) = %v, want LevelError", got)
	}
}
```

## Review

The parser is correct when a valid level survives any reasonable
casing/whitespace and an invalid one is refused, not swallowed. The normalization
in front of the switch is what makes `" INFO "` and `"debug"` both work; drop it
and the case list silently misses valid operator input. The fail-closed default
is what turns a `LOG_LEVEL` typo into a boot failure rather than weeks of
wrong-verbosity logging, and `TestParseLevel`'s `errors.Is` assertion is what
locks that behavior in. `MustLevel` shows the deliberate opposite choice for
optional config — a named fallback, not a hidden default.

## Resources

- [log/slog Level](https://pkg.go.dev/log/slog#Level) — the level constants and their integer values.
- [slog.Level.Level and comparison](https://pkg.go.dev/log/slog#Level) — how ordered levels drive `Handler.Enabled`.
- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — expression switch with comma-list cases.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-http-retry-classifier.md](03-http-retry-classifier.md) | Next: [05-method-router-405.md](05-method-router-405.md)
