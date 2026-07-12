# Exercise 7: Token-Bucket Capacity And Refill From Constant Expressions

A token-bucket rate limiter is configured by requests-per-second, burst capacity,
and a refill interval. The refill interval is not a free parameter — it is
`time.Second / requestsPerSecond`, and computing it as a constant `Duration`
expression keeps it exactly consistent with the rate. This module derives those
values, showing untyped scalars flowing into a typed `Duration` and into `int`
fields, and validates the config.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
bucketconfig/                 independent module: example.com/bucketconfig
  go.mod                      go 1.26
  config.go                   RefillInterval = time.Second/rps (typed Duration); Config, Validate
  cmd/
    demo/
      main.go                 prints the default config; validates a bad one
  config_test.go              constant refill identity; Validate table; int/Duration landing
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `RequestsPerSecond`, `Burst`, `Capacity` (untyped ints) and
`RefillInterval` (a typed `Duration` constant `= time.Second / RequestsPerSecond`);
a `Config` struct with a `Validate` method requiring `rps > 0` and `burst >= 1`.
Test: `RefillInterval == time.Second / RequestsPerSecond` as a constant; `Validate`
rejects `rps = 0` and `burst = 0`; capacity and refill land in `int` fields.
Verify: `go test -count=1 -race ./...`

### The refill interval is a derived constant, not a guess

If the bucket admits `RequestsPerSecond` tokens per second, one token arrives every
`time.Second / RequestsPerSecond`. Writing that as a constant expression means the
interval is always exactly consistent with the rate: change `RequestsPerSecond` and
`RefillInterval` follows automatically at compile time, with no chance of the two
drifting. `time.Second` is a typed `Duration`, `RequestsPerSecond` is an untyped
integer constant, and the division folds to a typed `Duration` (10ms for 100 rps).
This is the untyped-scalar-into-typed-unit adaptation from the concepts, applied to
a real config knob.

The `Config` struct carries the runtime-tunable values as ordinary fields —
`RPS int`, `Burst int`, `Capacity int`, `Refill time.Duration`. `NewDefault`
populates them from the constants, so the untyped `Capacity` constant lands in an
`int` field and `RefillInterval` lands in a `Duration` field without any cast.
`Validate` enforces the invariants a token bucket needs: a positive rate and a
burst of at least one (a burst of zero would admit nothing). Validation errors are
sentinel-wrapped so callers can match them.

Create `config.go`:

```go
package bucketconfig

import (
	"errors"
	"fmt"
	"time"
)

const (
	// RequestsPerSecond is the steady-state admission rate.
	RequestsPerSecond = 100
	// Burst is the number of tokens available for a spike.
	Burst = 20
	// Capacity is the bucket's maximum token count.
	Capacity = RequestsPerSecond + Burst
	// RefillInterval is the time between single-token refills, derived from the
	// rate as a compile-time typed Duration.
	RefillInterval = time.Second / RequestsPerSecond
)

var (
	ErrRate  = errors.New("requests per second must be positive")
	ErrBurst = errors.New("burst must be at least 1")
)

// Config is a token-bucket configuration. RPS/Burst/Capacity are ints; Refill is
// a Duration. The default constants land in each field's type without a cast.
type Config struct {
	RPS      int
	Burst    int
	Capacity int
	Refill   time.Duration
}

// NewDefault returns the configuration built from the package constants.
func NewDefault() Config {
	return Config{
		RPS:      RequestsPerSecond,
		Burst:    Burst,
		Capacity: Capacity,
		Refill:   RefillInterval,
	}
}

// Validate reports whether the configuration is usable.
func (c Config) Validate() error {
	if c.RPS <= 0 {
		return fmt.Errorf("%w: got %d", ErrRate, c.RPS)
	}
	if c.Burst < 1 {
		return fmt.Errorf("%w: got %d", ErrBurst, c.Burst)
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bucketconfig"
)

func main() {
	cfg := bucketconfig.NewDefault()
	fmt.Printf("rps=%d burst=%d capacity=%d refill=%s\n", cfg.RPS, cfg.Burst, cfg.Capacity, cfg.Refill)
	fmt.Printf("valid: %v\n", cfg.Validate() == nil)

	bad := bucketconfig.Config{RPS: 0, Burst: 5}
	fmt.Printf("bad config error: %v\n", bad.Validate())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
rps=100 burst=20 capacity=120 refill=10ms
valid: true
bad config error: requests per second must be positive: got 0
```

### Tests

`TestRefillIsDerivedConstant` proves `RefillInterval` equals `time.Second /
RequestsPerSecond` as a constant and resolves to 10ms. `TestValidate` is
table-driven over the zero-rate, zero-burst, and valid cases, asserting the
sentinels with `errors.Is`. `TestFieldTypes` confirms the constants land in the
`int` and `Duration` fields.

Create `config_test.go`:

```go
package bucketconfig

import (
	"errors"
	"testing"
	"time"
)

func TestRefillIsDerivedConstant(t *testing.T) {
	t.Parallel()

	const want = time.Second / RequestsPerSecond
	if RefillInterval != want {
		t.Fatalf("RefillInterval = %s, want %s", RefillInterval, want)
	}
	if RefillInterval != 10*time.Millisecond {
		t.Fatalf("RefillInterval = %s, want 10ms", RefillInterval)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{"valid default", NewDefault(), nil},
		{"zero rps", Config{RPS: 0, Burst: 5}, ErrRate},
		{"negative rps", Config{RPS: -1, Burst: 5}, ErrRate},
		{"zero burst", Config{RPS: 10, Burst: 0}, ErrBurst},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cfg.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestFieldTypes(t *testing.T) {
	t.Parallel()

	cfg := NewDefault()
	var capacity int = cfg.Capacity
	var refill time.Duration = cfg.Refill

	if capacity != 120 {
		t.Fatalf("Capacity = %d, want 120", capacity)
	}
	if refill != 10*time.Millisecond {
		t.Fatalf("Refill = %s, want 10ms", refill)
	}
}
```

## Review

The config is correct when `RefillInterval` is derived from the rate as a constant
(so the two can never disagree) and when `Validate` rejects a non-positive rate and
a sub-one burst — the two invariants without which a token bucket admits nothing or
divides by zero. The lesson is the boundary adaptation: `time.Second /
RequestsPerSecond` folds an untyped scalar into a typed `Duration`, while
`Capacity` stays an untyped int that lands in an `int` field. No float leaks in
here; the refill is exact integer-nanosecond `Duration` arithmetic.

## Resources

- [time.Duration](https://pkg.go.dev/time#Duration) — Duration arithmetic and the division used for refill.
- [golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate) — the real token-bucket limiter this config mirrors.
- [Go Language Specification: Constant expressions](https://go.dev/ref/spec#Constant_expressions) — folding typed Duration division at compile time.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-sampler-rate-float-precision.md](06-sampler-rate-float-precision.md) | Next: [08-job-status-typed-enum.md](08-job-status-typed-enum.md)
