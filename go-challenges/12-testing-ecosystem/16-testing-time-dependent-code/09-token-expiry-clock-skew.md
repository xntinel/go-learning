# Exercise 9: Validate token exp/nbf with an injected now and clock-skew leeway

Session and token validation is time-dependent security code: a token is valid
only between its not-before (`nbf`) and expiry (`exp`) instants, and because the
issuing and validating services run on different machines whose clocks drift, real
systems accept a small skew *leeway* on both edges. This is exactly the code that
must take `now` as an input — the "current time" is a security-relevant parameter,
not an ambient fact — so a test can pin the not-yet-valid, expired, and
within-leeway boundaries at exact instants.

## What you'll build

```text
tokenvalidate/                 independent module: example.com/tokenvalidate
  go.mod
  validate.go                  Claims; sentinel errors; Validator.Validate(claims, now)
  cmd/
    demo/
      main.go                  validate a token at several instants; print outcomes
  validate_test.go             table: valid, not-yet-valid, expired, within-leeway both edges
```

Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
Implement: a stateless `Validator` with a skew `Leeway` and `Validate(claims Claims, now time.Time) error`, returning `ErrTokenNotYetValid` before `nbf-leeway`, `ErrTokenExpired` after `exp+leeway`, and nil within.
Test: table-driven with an injected `now` — valid mid-window; before `nbf-leeway` rejects but within leeway accepts; after `exp+leeway` rejects but within leeway accepts; assert sentinels via `errors.Is`; `t.Parallel`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tokenvalidate/cmd/demo
cd ~/go-exercises/tokenvalidate
go mod init example.com/tokenvalidate
```

### Why now is a parameter, and why leeway exists

A validator that called `time.Now()` internally would be almost impossible to test
at the boundaries: you cannot reliably manufacture "exactly one nanosecond past
expiry" against the wall clock. More importantly, in real deployments the same
token is validated by many nodes, and "now" is genuinely different on each — so
"now" belongs in the signature as an input the caller supplies (from its own
clock, or from a `Clock` in larger systems). Passing `now time.Time` makes every
boundary a precise, reproducible assertion.

The leeway models real clock skew. Suppose the auth service issues a token whose
`exp` is `12:00:00`, but the validating service's clock runs three seconds fast:
at the validator's `12:00:02` the token looks expired, yet on the issuer's clock
it is still valid. Rejecting it would log a legitimate user out early. The standard
remedy is a small tolerance — commonly a few seconds — applied to both edges. A
token is *not yet valid* only when `now` is before `nbf - leeway`; it is *expired*
only when `now` is after `exp + leeway`. Within those widened bounds it is
accepted. The leeway is a deliberate widening of the valid window by `leeway` on
each side.

The two rejection paths return package-level sentinel errors wrapped with `%w` so
callers can branch with `errors.Is` — an API gateway returns `401` with a
distinct log line for "not yet valid" (usually a misconfigured client clock)
versus "expired" (refresh needed). The order matters only for a malformed token
whose `nbf` is after `exp`; validating `nbf` first means such a token reports
not-yet-valid, which is the conservative choice.

Create `validate.go`:

```go
package tokenvalidate

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors let callers branch on the failure kind with errors.Is.
var (
	ErrTokenNotYetValid = errors.New("token not yet valid")
	ErrTokenExpired     = errors.New("token expired")
)

// Claims are the time-bound fields of a token.
type Claims struct {
	NotBefore time.Time // nbf
	ExpiresAt time.Time // exp
}

// Validator validates claims against a caller-supplied now, tolerating clock
// skew up to Leeway on both edges.
type Validator struct {
	Leeway time.Duration
}

// Validate returns nil when now is within [nbf-leeway, exp+leeway]. It returns
// ErrTokenNotYetValid before that window and ErrTokenExpired after it.
func (v Validator) Validate(c Claims, now time.Time) error {
	if now.Before(c.NotBefore.Add(-v.Leeway)) {
		return fmt.Errorf("nbf=%s now=%s: %w", c.NotBefore.Format(time.RFC3339), now.Format(time.RFC3339), ErrTokenNotYetValid)
	}
	if now.After(c.ExpiresAt.Add(v.Leeway)) {
		return fmt.Errorf("exp=%s now=%s: %w", c.ExpiresAt.Format(time.RFC3339), now.Format(time.RFC3339), ErrTokenExpired)
	}
	return nil
}
```

### The runnable demo

The demo validates one token (valid 12:00-13:00, 30s leeway) at four instants: a
mid-window time, before `nbf`, within the pre-`nbf` leeway, and well after `exp`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/tokenvalidate"
)

func main() {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := tokenvalidate.Claims{NotBefore: base, ExpiresAt: base.Add(time.Hour)}
	v := tokenvalidate.Validator{Leeway: 30 * time.Second}

	cases := []struct {
		label string
		now   time.Time
	}{
		{"mid-window", base.Add(30 * time.Minute)},
		{"1min before nbf", base.Add(-time.Minute)},
		{"10s before nbf", base.Add(-10 * time.Second)},
		{"10min after exp", base.Add(70 * time.Minute)},
	}
	for _, tc := range cases {
		fmt.Printf("%-16s -> %v\n", tc.label, v.Validate(c, tc.now))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
mid-window       -> <nil>
1min before nbf  -> nbf=2026-01-01T12:00:00Z now=2026-01-01T11:59:00Z: token not yet valid
10s before nbf   -> <nil>
10min after exp  -> exp=2026-01-01T13:00:00Z now=2026-01-01T14:10:00Z: token expired
```

### Tests

The table drives one token through every boundary at exact instants: mid-window
(valid), the far side of both leeway bounds (rejected with the right sentinel), and
just inside both leeway bounds (accepted). `errors.Is` matches the wrapped
sentinels. Because `now` is an input, each row states its instant precisely — there
is no wall clock to race.

Create `validate_test.go`:

```go
package tokenvalidate

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	exp := base.Add(time.Hour)
	c := Claims{NotBefore: base, ExpiresAt: exp}
	v := Validator{Leeway: 30 * time.Second}

	tests := []struct {
		name    string
		now     time.Time
		wantErr error
	}{
		{"mid-window", base.Add(30 * time.Minute), nil},
		{"just after nbf", base.Add(time.Nanosecond), nil},
		{"just before exp", exp.Add(-time.Nanosecond), nil},
		{"within pre-nbf leeway", base.Add(-20 * time.Second), nil},
		{"within post-exp leeway", exp.Add(20 * time.Second), nil},
		{"at nbf-leeway edge", base.Add(-30 * time.Second), nil},
		{"at exp+leeway edge", exp.Add(30 * time.Second), nil},
		{"before nbf-leeway", base.Add(-31 * time.Second), ErrTokenNotYetValid},
		{"far before nbf", base.Add(-time.Hour), ErrTokenNotYetValid},
		{"after exp+leeway", exp.Add(31 * time.Second), ErrTokenExpired},
		{"far after exp", exp.Add(time.Hour), ErrTokenExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := v.Validate(c, tt.now)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestZeroLeewayIsInclusiveAtEdges(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := Claims{NotBefore: base, ExpiresAt: base.Add(time.Hour)}
	v := Validator{} // no leeway

	if err := v.Validate(c, base); err != nil {
		t.Fatalf("exactly at nbf = %v, want nil (inclusive)", err)
	}
	if err := v.Validate(c, base.Add(time.Hour)); err != nil {
		t.Fatalf("exactly at exp = %v, want nil (inclusive)", err)
	}
	if err := v.Validate(c, base.Add(-time.Nanosecond)); !errors.Is(err, ErrTokenNotYetValid) {
		t.Fatalf("1ns before nbf = %v, want ErrTokenNotYetValid", err)
	}
	if err := v.Validate(c, base.Add(time.Hour+time.Nanosecond)); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("1ns after exp = %v, want ErrTokenExpired", err)
	}
}

func ExampleValidator_Validate() {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := Claims{NotBefore: base, ExpiresAt: base.Add(time.Hour)}
	v := Validator{Leeway: 30 * time.Second}

	fmt.Println(v.Validate(c, base.Add(30*time.Minute)) == nil)
	fmt.Println(errors.Is(v.Validate(c, base.Add(2*time.Hour)), ErrTokenExpired))
	// Output:
	// true
	// true
}
```

## Review

The validator is correct when `now` outside `[nbf-leeway, exp+leeway]` reports the
matching sentinel and inside it returns nil, with the edges inclusive
(`Before`/`After` are strict, so exactly at an edge passes). Taking `now` as a
parameter is the whole design point — it makes the boundary tests exact and models
the reality that different nodes have different clocks. The bug this guards against
is forgetting leeway entirely (rejecting a token one second past `exp` and logging
users out on clock drift) or applying it to only one edge. Asserting the sentinels
with `errors.Is` (not string matching) is what lets a gateway branch on the failure
kind. There is no goroutine here, so `-race` is trivially clean, but keep it in the
command for uniformity.

## Resources

- [`time.Time.Before`](https://pkg.go.dev/time#Time.Before) and [`time.Time.After`](https://pkg.go.dev/time#Time.After) — the strict comparisons that make the edges inclusive.
- [RFC 7519 §4.1.4/4.1.5](https://datatracker.ietf.org/doc/html/rfc7519#section-4.1.4) — JWT `exp` and `nbf`, and the "some small leeway" clock-skew guidance.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching the wrapped sentinel errors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-circuit-breaker-half-open.md](08-circuit-breaker-half-open.md) | Next: [10-lease-renewal-heartbeat.md](10-lease-renewal-heartbeat.md)
