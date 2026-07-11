# Exercise 8: Timeout and Backoff Config — Parsing and Bounding time.Duration

Operational settings arrive as strings like `"250ms"`, `"2s"`, `"1m"`. This module
parses them into `time.Duration`, rejects a zero or negative value where a positive
timeout is required, clamps to a maximum, and computes exponential backoff steps
without overflowing `int64` — the shift-based bug where a large attempt count wraps a
duration negative.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports another exercise.

## What you'll build

```text
backoff/                   independent module: example.com/backoff
  go.mod                   go 1.26
  backoff.go               ParsePositiveDuration, ParseTimeoutOrDefault, Clamp, Backoff
  cmd/
    demo/
      main.go              parses timeouts, prints a capped backoff sequence
  backoff_test.go          valid parses, zero rejected, default for empty, overflow-safe backoff
```

- Files: `backoff.go`, `cmd/demo/main.go`, `backoff_test.go`.
- Implement: `ParsePositiveDuration` (requires > 0), `ParseTimeoutOrDefault` (empty maps to a default), `Clamp`, and `Backoff` (base * 2^attempt capped at max, overflow-safe).
- Test: valid strings parse; `"0s"` where positive required is rejected; empty maps to the default; the backoff sequence is capped at max and never goes negative even for a huge attempt count.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/backoff/cmd/demo
cd ~/go-exercises/backoff
go mod init example.com/backoff
```

### Why zero is dangerous and how backoff overflows

`time.Duration` is an `int64` count of nanoseconds, so `time.ParseDuration` handles the
string forms and the arithmetic is plain integer math. Two boundary concerns make this
more than a one-liner. First, a zero `Duration` is ambiguous and often actively
dangerous: for many APIs — `http.Server`'s `ReadTimeout`/`WriteTimeout`, a client with
no deadline — zero means "no timeout, wait forever". So an operator who leaves
`REQUEST_TIMEOUT` unset must not silently get an infinite timeout. `ParsePositiveDuration`
rejects zero and negatives outright, and `ParseTimeoutOrDefault` maps the *unset* case
(empty string) to a documented positive default rather than to zero. The distinction
between "unset" and "explicitly 0" is exactly the kind of thing a boundary must decide
deliberately.

Second, exponential backoff computed as `base << attempt` or `base * 2^attempt`
overflows `int64` once the shift is large enough, and integer overflow *wraps* — a
duration that should keep growing suddenly goes negative, and a negative backoff means
an immediate retry storm. The guard is to never double past the cap: only double while
the current value is at most `max/2`, so the result can never exceed `max` and therefore
can never overflow. This is cleaner than computing the shift and checking `math.MaxInt64`
afterward, and it makes the cap the natural stopping condition.

Create `backoff.go`:

```go
package backoff

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors for the duration boundary.
var (
	ErrParse       = errors.New("invalid duration")
	ErrNonPositive = errors.New("duration must be positive")
)

// ParsePositiveDuration parses s and requires a strictly positive duration, so
// a zero (which many APIs read as "no timeout") is rejected.
func ParsePositiveDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrParse, s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%w: %s", ErrNonPositive, d)
	}
	return d, nil
}

// ParseTimeoutOrDefault maps an unset (empty) value to def, and otherwise
// requires a positive duration. This keeps "unset" distinct from "0".
func ParseTimeoutOrDefault(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return ParsePositiveDuration(s)
}

// Clamp bounds d to at most max (and at least 0).
func Clamp(d, max time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > max {
		return max
	}
	return d
}

// Backoff returns base doubled attempt times, capped at max. It never overflows:
// it only doubles while the value is at most max/2, so the result stays <= max.
func Backoff(base, max time.Duration, attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := base
	for range attempt {
		if d > max/2 {
			return max
		}
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}
```

### The runnable demo

The demo parses a timeout, shows an empty value falling back to a default, prints a
backoff sequence that grows and then caps, and shows a zero timeout being rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"log"
	"time"

	"example.com/backoff"
)

func main() {
	timeout, err := backoff.ParsePositiveDuration("2s")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("timeout:", timeout)

	def, _ := backoff.ParseTimeoutOrDefault("", 30*time.Second)
	fmt.Println("empty -> default:", def)

	fmt.Print("backoff:")
	for i := range 7 {
		fmt.Printf(" %s", backoff.Backoff(100*time.Millisecond, 2*time.Second, i))
	}
	fmt.Println()

	_, err = backoff.ParsePositiveDuration("0s")
	fmt.Println("zero rejected:", errors.Is(err, backoff.ErrNonPositive))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
timeout: 2s
empty -> default: 30s
backoff: 100ms 200ms 400ms 800ms 1.6s 2s 2s
zero rejected: true
```

### Tests

`TestParsePositiveDuration` covers valid strings and the rejection of `"0s"` and a
negative. `TestParseTimeoutOrDefault` shows empty maps to the default while a bad
non-empty value still errors. `TestBackoffSequence` pins the capped doubling sequence.
`TestBackoffNeverNegative` is the overflow guard: even with an enormous attempt count
the result equals `max` and stays strictly positive, where a shift-based implementation
would wrap negative.

Create `backoff_test.go`:

```go
package backoff

import (
	"errors"
	"testing"
	"time"
)

func TestParsePositiveDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    time.Duration
		wantErr error
	}{
		{in: "250ms", want: 250 * time.Millisecond},
		{in: "2s", want: 2 * time.Second},
		{in: "1m", want: time.Minute},
		{in: "0s", wantErr: ErrNonPositive},
		{in: "-5s", wantErr: ErrNonPositive},
		{in: "banana", wantErr: ErrParse},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			got, err := ParsePositiveDuration(tt.in)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Parse(%q) error = %v, want %v", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Parse(%q) = %s,%v; want %s", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestParseTimeoutOrDefault(t *testing.T) {
	t.Parallel()

	got, err := ParseTimeoutOrDefault("", 30*time.Second)
	if err != nil || got != 30*time.Second {
		t.Fatalf("empty = %s,%v; want 30s", got, err)
	}
	if _, err := ParseTimeoutOrDefault("0s", 30*time.Second); !errors.Is(err, ErrNonPositive) {
		t.Fatalf("explicit 0s should still be rejected, got %v", err)
	}
}

func TestBackoffSequence(t *testing.T) {
	t.Parallel()

	base, max := 100*time.Millisecond, 2*time.Second
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
		2 * time.Second,
		2 * time.Second,
	}
	for i, w := range want {
		if got := Backoff(base, max, i); got != w {
			t.Fatalf("Backoff attempt %d = %s, want %s", i, got, w)
		}
	}
}

func TestBackoffNeverNegative(t *testing.T) {
	t.Parallel()

	max := 30 * time.Second
	got := Backoff(time.Second, max, 1000)
	if got != max {
		t.Fatalf("Backoff huge attempt = %s, want %s", got, max)
	}
	if got <= 0 {
		t.Fatalf("Backoff went non-positive: %s (overflow)", got)
	}
}
```

## Review

The parser is correct when "unset", "explicitly zero", and "positive" are three
distinct outcomes: `ParsePositiveDuration` rejects zero and negatives, and
`ParseTimeoutOrDefault` routes only the empty case to the default — so an operator who
forgets a timeout gets the documented default, not an accidental infinite wait. The
backoff guard is the subtle part: `TestBackoffNeverNegative` proves an attempt count that
would overflow a `base << attempt` shift instead saturates at `max` and stays positive,
because the loop never doubles past `max/2`. Prefer this saturating form over computing
the shift and checking `math.MaxInt64` afterward.

## Resources

- [time.ParseDuration](https://pkg.go.dev/time#ParseDuration) — the accepted string forms.
- [time.Duration](https://pkg.go.dev/time#Duration) — the int64-nanosecond representation.
- [net/http.Server](https://pkg.go.dev/net/http#Server) — where a zero timeout means "no timeout".

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-feature-flag-tri-state.md](09-feature-flag-tri-state.md)
