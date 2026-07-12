# Exercise 30: Circuit Breaker Error Thresholds Initialized at init from Config with Sensible Defaults

**Nivel: Intermedio** — validacion rapida (un test corto).

A circuit breaker needs three numbers to work: how bad a failure rate has
to get before it trips, how many samples it needs before it trusts that
rate, and how long it stays open before trying again. Reading those from
environment variables at init — with hardcoded defaults for whichever ones
an operator never bothered to set, and a hard failure for one they set to
something invalid — means the breaker's own configuration cannot become
the reason a service falls over.

## What you'll build

```text
breaker/                   independent module: example.com/breaker
  go.mod                    module example.com/breaker
  breaker.go                  Config, loadConfig(getenv), Breaker with Allow/RecordResult
  cmd/
    demo/
      main.go                 shows default config, then trips and resets a breaker
  breaker_test.go              loadConfig defaults/overrides/validation table + trip + stay-closed tests
```

Files: `breaker.go`, `cmd/demo/main.go`, `breaker_test.go`.
Implement: `loadConfig(getenv func(string) string) (Config, error)` applying defaults for unset variables and validating any variable that is set; `Breaker.RecordResult(now time.Time, success bool)` tracking a rolling total/failure count and tripping open once `MinRequests` samples are in and the failure rate reaches `FailureThreshold`; `Breaker.Allow(now time.Time) bool` resetting to closed once `OpenDuration` has elapsed since it tripped.
Test: `loadConfig` with no environment variables set returns the hardcoded defaults; valid overrides for all three variables are applied; an out-of-range threshold, a non-numeric threshold, a non-positive min-requests, and an unparsable duration each return a descriptive error naming the offending variable; a breaker crossing its threshold opens and denies requests, then allows a trial request once its open duration elapses; a breaker below threshold stays closed.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why defaults for the unset case, and a hard failure for the invalid one

These three thresholds are exactly the kind of configuration an operator
usually leaves alone: `BREAKER_FAILURE_THRESHOLD=0.5`,
`BREAKER_MIN_REQUESTS=10`, and `BREAKER_OPEN_DURATION=30s` are reasonable
defaults for most services, and most environments will never set any of
these variables at all. `loadConfig` treats "unset" (an empty string from
`getenv`) as "use the default" — never as an error. But "set to something
that parses to a nonsensical value" is a different situation entirely: a
failure threshold of `1.5` (150% of requests failing is not a rate), a
`BREAKER_MIN_REQUESTS` of `0` or a negative number, or an unparsable
duration string are all configuration mistakes, and `loadConfig` returns a
descriptive error for each rather than silently falling back to the
default or, worse, silently accepting a nonsensical value that would make
the breaker never trip (or always trip). `init()` panics on that error,
which is the fail-fast behavior this chapter has used throughout: a broken
static configuration should never let the binary start.

`getenv` is injected as a plain function rather than read via `os.Getenv`
directly inside `loadConfig`, for the same testability reason used
elsewhere in this chapter — tests exercise every combination of set,
unset, valid, and invalid values through a fake map-backed `getenv`, never
touching real process environment variables (which would be both slower
and a source of test pollution across parallel subtests).

`Breaker` itself is a small state machine: it counts outcomes while closed,
trips open the moment enough samples exist and the failure rate reaches
threshold, and treats the elapsed time since it opened as the signal to try
again. Passing `now` explicitly into both `Allow` and `RecordResult`, rather
than calling `time.Now()` internally, is what makes
`TestBreakerTripsAndResetsAfterOpenDuration` deterministic: advancing past
`OpenDuration` is an exact, reproducible fact about the `time.Time` values
the test supplies, never a race against how long the test itself took to
run.

Create `breaker.go`:

```go
// breaker.go
// Package breaker reads circuit breaker thresholds from environment
// variables at package initialization, falling back to hardcoded sensible
// defaults for any parameter left unspecified, and panicking on a value
// that is present but invalid -- so a misconfigured threshold fails at
// startup, never partway through a production incident.
package breaker

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Defaults used for any parameter not overridden by environment variables.
const (
	defaultFailureThreshold = 0.5 // trip once >=50% of a window's requests fail
	defaultMinRequests      = 10  // never trip on fewer than this many samples
	defaultOpenDuration     = 30 * time.Second
)

// Config holds the breaker's tunable thresholds.
type Config struct {
	FailureThreshold float64
	MinRequests      int
	OpenDuration     time.Duration
}

// cfg is the resolved configuration, built once at init.
var cfg Config

func init() {
	c, err := loadConfig(os.Getenv)
	if err != nil {
		panic("breaker: " + err.Error())
	}
	cfg = c
}

// loadConfig reads BREAKER_FAILURE_THRESHOLD, BREAKER_MIN_REQUESTS, and
// BREAKER_OPEN_DURATION through getenv, applying defaultFailureThreshold,
// defaultMinRequests, and defaultOpenDuration for any unset variable. It
// returns an error naming the offending variable if a value is present but
// invalid. Extracted from init, with getenv injected, so tests can exercise
// every path without mutating real process environment variables.
func loadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		FailureThreshold: defaultFailureThreshold,
		MinRequests:      defaultMinRequests,
		OpenDuration:     defaultOpenDuration,
	}

	if v := getenv("BREAKER_FAILURE_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f > 1 {
			return Config{}, fmt.Errorf("BREAKER_FAILURE_THRESHOLD=%q must be a number in (0,1]", v)
		}
		c.FailureThreshold = f
	}
	if v := getenv("BREAKER_MIN_REQUESTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("BREAKER_MIN_REQUESTS=%q must be a positive integer", v)
		}
		c.MinRequests = n
	}
	if v := getenv("BREAKER_OPEN_DURATION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("BREAKER_OPEN_DURATION=%q must be a positive duration", v)
		}
		c.OpenDuration = d
	}
	return c, nil
}

// Current returns the resolved circuit breaker configuration.
func Current() Config { return cfg }

// Breaker is a minimal request-outcome circuit breaker: it trips open once
// enough requests have been observed and its failure rate reaches the
// configured threshold, and it resets to closed once OpenDuration has
// elapsed since it tripped.
type Breaker struct {
	cfg      Config
	total    int
	failures int
	open     bool
	openedAt time.Time
}

// New returns a Breaker using cfg's thresholds.
func New(cfg Config) *Breaker {
	return &Breaker{cfg: cfg}
}

// Allow reports whether a request may proceed right now. If the breaker is
// open but OpenDuration has elapsed since it tripped, it resets to closed
// (and its counters clear) so the next request can act as a trial.
func (b *Breaker) Allow(now time.Time) bool {
	if b.open && now.Sub(b.openedAt) >= b.cfg.OpenDuration {
		b.open = false
		b.total = 0
		b.failures = 0
	}
	return !b.open
}

// RecordResult records the outcome of one request at time now, tripping the
// breaker open if enough requests have been observed and the failure rate
// has reached the configured threshold.
func (b *Breaker) RecordResult(now time.Time, success bool) {
	if b.open {
		return
	}
	b.total++
	if !success {
		b.failures++
	}
	if b.total >= b.cfg.MinRequests {
		rate := float64(b.failures) / float64(b.total)
		if rate >= b.cfg.FailureThreshold {
			b.open = true
			b.openedAt = now
		}
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"time"

	"example.com/breaker"
)

func main() {
	fmt.Printf("config: %+v\n", breaker.Current())

	cfg := breaker.Config{FailureThreshold: 0.5, MinRequests: 4, OpenDuration: 10 * time.Second}
	b := breaker.New(cfg)

	start := time.Unix(0, 0)
	outcomes := []bool{true, false, false, false} // 3/4 failures, over threshold
	for i, ok := range outcomes {
		b.RecordResult(start, ok)
		fmt.Printf("after request %d, Allow: %v\n", i+1, b.Allow(start))
	}

	later := start.Add(11 * time.Second) // past the 10s open duration
	fmt.Println("Allow after open duration elapses:", b.Allow(later))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
config: {FailureThreshold:0.5 MinRequests:10 OpenDuration:30s}
after request 1, Allow: true
after request 2, Allow: true
after request 3, Allow: true
after request 4, Allow: false
Allow after open duration elapses: true
```

### Tests

Create `breaker_test.go`:

```go
// breaker_test.go
package breaker

import (
	"strings"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	t.Parallel()

	getenv := func(string) string { return "" }
	c, err := loadConfig(getenv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{
		FailureThreshold: defaultFailureThreshold,
		MinRequests:      defaultMinRequests,
		OpenDuration:     defaultOpenDuration,
	}
	if c != want {
		t.Fatalf("loadConfig defaults = %+v, want %+v", c, want)
	}
}

func TestLoadConfigOverridesAndValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(t *testing.T, c Config)
	}{
		{
			name: "valid overrides",
			env: map[string]string{
				"BREAKER_FAILURE_THRESHOLD": "0.25",
				"BREAKER_MIN_REQUESTS":      "5",
				"BREAKER_OPEN_DURATION":     "1m",
			},
			check: func(t *testing.T, c Config) {
				if c.FailureThreshold != 0.25 || c.MinRequests != 5 || c.OpenDuration != time.Minute {
					t.Fatalf("got %+v", c)
				}
			},
		},
		{
			name:    "invalid threshold too high",
			env:     map[string]string{"BREAKER_FAILURE_THRESHOLD": "1.5"},
			wantErr: "BREAKER_FAILURE_THRESHOLD",
		},
		{
			name:    "invalid threshold not a number",
			env:     map[string]string{"BREAKER_FAILURE_THRESHOLD": "nope"},
			wantErr: "BREAKER_FAILURE_THRESHOLD",
		},
		{
			name:    "invalid min requests",
			env:     map[string]string{"BREAKER_MIN_REQUESTS": "0"},
			wantErr: "BREAKER_MIN_REQUESTS",
		},
		{
			name:    "invalid open duration",
			env:     map[string]string{"BREAKER_OPEN_DURATION": "not-a-duration"},
			wantErr: "BREAKER_OPEN_DURATION",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(key string) string { return tc.env[key] }
			c, err := loadConfig(getenv)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				tc.check(t, c)
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestBreakerTripsAndResetsAfterOpenDuration(t *testing.T) {
	t.Parallel()

	cfg := Config{FailureThreshold: 0.5, MinRequests: 4, OpenDuration: 10 * time.Second}
	b := New(cfg)
	start := time.Unix(0, 0)

	for _, ok := range []bool{true, false, false, false} { // 3/4 failures
		b.RecordResult(start, ok)
	}
	if b.Allow(start) {
		t.Fatal("breaker should be open after crossing the failure threshold")
	}

	if !b.Allow(start.Add(11 * time.Second)) {
		t.Fatal("breaker should allow a trial request once OpenDuration has elapsed")
	}
}

func TestBreakerStaysClosedBelowThreshold(t *testing.T) {
	t.Parallel()

	cfg := Config{FailureThreshold: 0.5, MinRequests: 4, OpenDuration: 10 * time.Second}
	b := New(cfg)
	start := time.Unix(0, 0)

	for _, ok := range []bool{true, true, true, false} { // 1/4 failures, below threshold
		b.RecordResult(start, ok)
	}
	if !b.Allow(start) {
		t.Fatal("breaker should stay closed when failure rate is below threshold")
	}
}
```

## Review

`loadConfig` is correct when every unset variable resolves to its
hardcoded default, every valid override is applied exactly, and every
invalid value — out of range, non-numeric, non-positive, unparsable — is
rejected with an error naming the specific variable, not a generic "bad
config" message. `Breaker` is correct when it only trips once both
conditions hold together (`total >= MinRequests` *and* `rate >=
FailureThreshold`) — tripping on rate alone would let a single early
failure out of one sample close the breaker — and when it only resets after
`OpenDuration` has genuinely elapsed, verified with injected `time.Time`
values rather than real sleeps.

The mistake to avoid is silently ignoring an invalid environment variable
value and falling back to the default: that turns a typo like
`BREAKER_FAILURE_THRESHOLD=05` (missing the decimal point) into a breaker
that quietly runs with a threshold nobody chose, discovered only when it
behaves unexpectedly under load. Fail loudly at startup instead — the
entire point of validating configuration in `init()`.

## Resources

- [os.Getenv](https://pkg.go.dev/os#Getenv) — the real environment reader `init()` passes to `loadConfig`.
- [strconv.ParseFloat](https://pkg.go.dev/strconv#ParseFloat) and [time.ParseDuration](https://pkg.go.dev/time#ParseDuration) — the parsers validating the threshold and duration variables.
- [Martin Fowler — CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html) — the pattern this package's `Breaker` implements a minimal version of.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-dns-cache-prewarming-with-resolver.md](29-dns-cache-prewarming-with-resolver.md) | Next: [31-webhook-event-schema-registration-table.md](31-webhook-event-schema-registration-table.md)
