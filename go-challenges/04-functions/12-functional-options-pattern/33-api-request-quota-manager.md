# Exercise 33: API Request Quota System With Per-User Limits and Reset Options

**Nivel: Intermedio** — validacion rapida (un test corto).

A quota manager that can source a user's request limit from three different
places — a hardcoded per-user override, an environment variable default, or
a plain constructor default — needs a precedence order a caller can rely
on, not just "whichever option happened to run last." This module builds
that manager with functional options, fixing the order to code beats env
beats option, and validating its one syntactic input: the enforcement mode.

## What you'll build

```text
quota/                            independent module: example.com/api-request-quota-manager
  go.mod                          go 1.24
  quota.go                        Manager, Option, New, WithDefaultLimit,
                                   WithEnvDefaultLimit, WithUserLimit, WithRefillInterval,
                                   WithEnforcementMode, WithClock, LimitFor, Allow
  cmd/
    demo/
      main.go                     precedence resolution, then a reject-mode denial
  quota_test.go                    option-validation table, precedence, mode, and window tests
```

- Files: `quota.go`, `cmd/demo/main.go`, `quota_test.go`.
- Implement: a `Manager` built by `New(opts ...Option) (*Manager, error)` whose `LimitFor` resolves a user's effective limit with code-set overrides beating an environment default beating the plain option default, and whose `Allow` enforces that limit per fixed window according to a validated enforcement mode.
- Test: every option-validation case including a missing (not an error) versus malformed (an error) environment variable, the full three-way precedence order, both `reject` and `throttle` enforcement behavior, and a window reset against an injected clock.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Three sources, one fixed order

`WithDefaultLimit`, `WithEnvDefaultLimit`, and `WithUserLimit` can each be
set independently, in any order, and none of their closures can see what
the others set. The precedence between them is therefore not decided by
call order at all — it is decided once, by `LimitFor`, which always checks
the per-user override map first, the environment-sourced default second,
and the plain option default only if neither of the first two applies.
That fixed lookup order is what "code-set quotas override environment"
actually means in this module: it is not a construction-time error to
configure both an environment default and a user override for the same
user, it is simply the case that the override is checked first and the
environment value is never consulted for that user at all.

### A missing environment variable is not a misconfiguration

`WithEnvDefaultLimit` treats a variable that is not set at all as "this
source does not apply" — it returns `nil`, and `LimitFor` falls through to
the option default exactly as if `WithEnvDefaultLimit` had never been
called. A variable that *is* set but is not a valid, positive integer is
different: that is a real misconfiguration (someone set `QUOTA_DEFAULT=oops`
in the deployment environment), and `WithEnvDefaultLimit` rejects it at
construction rather than silently falling back and hiding the mistake.
`os.LookupEnv` — as opposed to `os.Getenv` — is what makes that distinction
possible: it reports presence and value separately, so "unset" and "set to
an empty string" are not confused with each other.

### A fixed window, reset by an injected clock

`Allow` tracks each user's request count against a window that starts the
first time they are seen and resets once the configured refill interval has
elapsed, exactly like the health checker's and cache's interval logic
earlier in this chapter. Using an injected clock rather than `time.Now`
lets `TestAllowResetsAfterRefillInterval` advance past the window boundary
by simple assignment, with no real waiting and no flakiness.

Create `quota.go`:

```go
package quota

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

var validModes = map[string]bool{
	"reject":   true,
	"throttle": true,
	"log-only": true,
}

type usageEntry struct {
	count       int
	windowStart time.Time
}

// Manager enforces a per-user request quota over a fixed refill window,
// resolving each user's limit from three possible sources in a fixed order
// of precedence: a code-set per-user override first, an environment
// variable default second, and a plain option default last.
type Manager struct {
	defaultLimit   int
	envLimit       *int
	userLimits     map[string]int
	refillInterval time.Duration
	mode           string
	now            func() time.Time

	mu    sync.Mutex
	usage map[string]usageEntry
}

// Option configures a Manager and may reject invalid input.
type Option func(*Manager) error

// New builds a Manager, seeding a default limit of 100, a one-minute refill
// interval, and "reject" enforcement, then applies opts.
func New(opts ...Option) (*Manager, error) {
	m := &Manager{
		defaultLimit:   100,
		userLimits:     make(map[string]int),
		refillInterval: time.Minute,
		mode:           "reject",
		now:            time.Now,
		usage:          make(map[string]usageEntry),
	}
	for _, opt := range opts {
		if err := opt(m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// WithDefaultLimit sets the plain option-level default limit (>= 1), used
// when a user has no code-set override and no usable environment default.
func WithDefaultLimit(n int) Option {
	return func(m *Manager) error {
		if n < 1 {
			return fmt.Errorf("default limit must be >= 1, got %d", n)
		}
		m.defaultLimit = n
		return nil
	}
}

// WithEnvDefaultLimit reads envVar as the environment-sourced default
// limit. A missing environment variable is not an error (the option
// default still applies); a present but unparsable or non-positive value
// is.
func WithEnvDefaultLimit(envVar string) Option {
	return func(m *Manager) error {
		raw, ok := os.LookupEnv(envVar)
		if !ok {
			return nil
		}
		v, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("environment variable %s=%q is not a valid integer: %w", envVar, raw, err)
		}
		if v < 1 {
			return fmt.Errorf("environment variable %s must be >= 1, got %d", envVar, v)
		}
		m.envLimit = &v
		return nil
	}
}

// WithUserLimit sets a code-level per-user override, which always takes
// precedence over both the environment default and the option default.
func WithUserLimit(user string, limit int) Option {
	return func(m *Manager) error {
		if user == "" {
			return fmt.Errorf("user must not be empty")
		}
		if limit < 1 {
			return fmt.Errorf("user limit must be >= 1, got %d", limit)
		}
		m.userLimits[user] = limit
		return nil
	}
}

// WithRefillInterval sets how often a user's usage window resets (> 0).
func WithRefillInterval(d time.Duration) Option {
	return func(m *Manager) error {
		if d <= 0 {
			return fmt.Errorf("refill interval must be positive, got %s", d)
		}
		m.refillInterval = d
		return nil
	}
}

// WithEnforcementMode sets how Allow reacts to a user exceeding their
// limit: "reject" denies the request, "throttle" and "log-only" both allow
// it while still reporting the quota was exceeded.
func WithEnforcementMode(mode string) Option {
	return func(m *Manager) error {
		if !validModes[mode] {
			return fmt.Errorf("unsupported enforcement mode: %q", mode)
		}
		m.mode = mode
		return nil
	}
}

// WithClock injects the clock used to time refill windows.
func WithClock(now func() time.Time) Option {
	return func(m *Manager) error {
		if now == nil {
			return fmt.Errorf("clock is nil")
		}
		m.now = now
		return nil
	}
}

// LimitFor reports the effective limit for user: a code-set override wins
// first, the environment default second, and the option default last.
func (m *Manager) LimitFor(user string) int {
	if limit, ok := m.userLimits[user]; ok {
		return limit
	}
	if m.envLimit != nil {
		return *m.envLimit
	}
	return m.defaultLimit
}

// Allow records one request from user against their current window and
// reports whether it should proceed. overQuota reports whether the user's
// usage this window has exceeded their limit, regardless of enforcement
// mode; allowed is false only when overQuota is true and the mode is
// "reject" — "throttle" and "log-only" always allow the request through.
func (m *Manager) Allow(user string) (allowed, overQuota bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	limit := m.LimitFor(user)
	now := m.now()

	entry, ok := m.usage[user]
	if !ok || now.Sub(entry.windowStart) >= m.refillInterval {
		entry = usageEntry{windowStart: now}
	}
	entry.count++
	m.usage[user] = entry

	overQuota = entry.count > limit
	if m.mode == "reject" {
		return !overQuota, overQuota
	}
	return true, overQuota
}
```

### The runnable demo

The demo sets an environment variable default of 50, an option default of
3, and a code override of 10 for `"vip"`. It shows `alice` (no override)
getting the environment default rather than the option default, and `vip`
getting its code override rather than either. It then drives a low-limit
`"guest"` user past their quota under `reject` mode and shows the third
request denied.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/api-request-quota-manager"
)

func main() {
	os.Setenv("QUOTA_DEFAULT", "50")

	mgr, err := quota.New(
		quota.WithDefaultLimit(3),
		quota.WithEnvDefaultLimit("QUOTA_DEFAULT"),
		quota.WithUserLimit("vip", 10),
		quota.WithUserLimit("guest", 2),
		quota.WithEnforcementMode("reject"),
	)
	if err != nil {
		panic(err)
	}

	fmt.Printf("alice limit (env default overrides option default): %d\n", mgr.LimitFor("alice"))
	fmt.Printf("vip limit (code override wins over env): %d\n", mgr.LimitFor("vip"))

	for i := 1; i <= 3; i++ {
		allowed, overQuota := mgr.Allow("guest")
		fmt.Printf("guest request %d: allowed=%t overQuota=%t\n", i, allowed, overQuota)
	}

	_, err = quota.New(quota.WithEnforcementMode("silent-drop"))
	fmt.Printf("unsupported enforcement mode rejected: %t\n", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alice limit (env default overrides option default): 50
vip limit (code override wins over env): 10
guest request 1: allowed=true overQuota=false
guest request 2: allowed=true overQuota=false
guest request 3: allowed=false overQuota=true
unsupported enforcement mode rejected: true
```

### Tests

`TestNewValidation` tables construction failures, distinguishing a missing
environment variable (not an error) from a malformed or non-positive one
(both errors) using `t.Setenv`. `TestLimitForPrecedence` proves the full
three-way order in one manager: environment beats option, code beats
environment. `TestLimitForFallsBackToOptionDefault` covers the plain case.
`TestAllowRejectModeDeniesOverQuota` and `TestAllowThrottleModeAllowsOverQuota`
prove the two enforcement modes diverge only in `allowed`, never in
`overQuota`. `TestAllowResetsAfterRefillInterval` proves the window resets
against an injected clock.

Create `quota_test.go`:

```go
package quota

import (
	"testing"
	"time"
)

func TestNewValidation(t *testing.T) {
	// Not t.Parallel(): several cases use t.Setenv, which forbids it.

	tests := []struct {
		name    string
		env     map[string]string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only"},
		{name: "invalid default limit", opts: []Option{WithDefaultLimit(0)}, wantErr: true},
		{name: "invalid user limit", opts: []Option{WithUserLimit("u1", 0)}, wantErr: true},
		{name: "empty user", opts: []Option{WithUserLimit("", 5)}, wantErr: true},
		{name: "invalid refill interval", opts: []Option{WithRefillInterval(0)}, wantErr: true},
		{name: "unsupported mode", opts: []Option{WithEnforcementMode("silent-drop")}, wantErr: true},
		{name: "nil clock", opts: []Option{WithClock(nil)}, wantErr: true},
		{
			name:    "env var not an integer",
			env:     map[string]string{"Q1": "not-a-number"},
			opts:    []Option{WithEnvDefaultLimit("Q1")},
			wantErr: true,
		},
		{
			name:    "env var non-positive",
			env:     map[string]string{"Q2": "0"},
			opts:    []Option{WithEnvDefaultLimit("Q2")},
			wantErr: true,
		},
		{
			name: "env var missing is not an error",
			opts: []Option{WithEnvDefaultLimit("Q_DOES_NOT_EXIST")},
		},
		{
			name: "env var valid",
			env:  map[string]string{"Q3": "25"},
			opts: []Option{WithEnvDefaultLimit("Q3")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLimitForPrecedence(t *testing.T) {
	t.Setenv("Q_PRECEDENCE", "50")

	m, err := New(
		WithDefaultLimit(3),
		WithEnvDefaultLimit("Q_PRECEDENCE"),
		WithUserLimit("vip", 10),
	)
	if err != nil {
		t.Fatal(err)
	}

	if got := m.LimitFor("alice"); got != 50 {
		t.Fatalf("LimitFor(alice) = %d, want 50 (env default should beat the option default)", got)
	}
	if got := m.LimitFor("vip"); got != 10 {
		t.Fatalf("LimitFor(vip) = %d, want 10 (a code-set override should beat the env default)", got)
	}
}

func TestLimitForFallsBackToOptionDefault(t *testing.T) {
	t.Parallel()

	m, err := New(WithDefaultLimit(7))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.LimitFor("nobody"); got != 7 {
		t.Fatalf("LimitFor(nobody) = %d, want 7", got)
	}
}

func TestAllowRejectModeDeniesOverQuota(t *testing.T) {
	t.Parallel()

	m, err := New(WithUserLimit("u1", 2), WithEnforcementMode("reject"))
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		allowed, overQuota := m.Allow("u1")
		if !allowed || overQuota {
			t.Fatalf("request %d: allowed=%t overQuota=%t, want (true, false)", i+1, allowed, overQuota)
		}
	}
	allowed, overQuota := m.Allow("u1")
	if allowed || !overQuota {
		t.Fatalf("3rd request: allowed=%t overQuota=%t, want (false, true)", allowed, overQuota)
	}
}

func TestAllowThrottleModeAllowsOverQuota(t *testing.T) {
	t.Parallel()

	m, err := New(WithUserLimit("u1", 1), WithEnforcementMode("throttle"))
	if err != nil {
		t.Fatal(err)
	}

	if allowed, overQuota := m.Allow("u1"); !allowed || overQuota {
		t.Fatalf("1st request: allowed=%t overQuota=%t, want (true, false)", allowed, overQuota)
	}
	allowed, overQuota := m.Allow("u1")
	if !allowed || !overQuota {
		t.Fatalf("2nd request: allowed=%t overQuota=%t, want (true, true): throttle must still allow", allowed, overQuota)
	}
}

func TestAllowResetsAfterRefillInterval(t *testing.T) {
	t.Parallel()

	current := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	m, err := New(
		WithUserLimit("u1", 1),
		WithRefillInterval(time.Minute),
		WithClock(func() time.Time { return current }),
	)
	if err != nil {
		t.Fatal(err)
	}

	if allowed, _ := m.Allow("u1"); !allowed {
		t.Fatal("first request within the window should be allowed")
	}
	if allowed, overQuota := m.Allow("u1"); allowed || !overQuota {
		t.Fatal("second request within the same window should be rejected and over quota")
	}

	current = current.Add(2 * time.Minute)
	if allowed, overQuota := m.Allow("u1"); !allowed || overQuota {
		t.Fatalf("after the window resets: allowed=%t overQuota=%t, want (true, false)", allowed, overQuota)
	}
}
```

## Review

The quota manager is correct when `LimitFor` is the *only* place precedence
between code, environment, and option defaults is decided — none of the
three configuring options need to know about, or defer to, the others.
That single-lookup-order design is what makes "code overrides environment"
true unconditionally, for every user, rather than true only when the
options happened to run in a particular sequence. Treating a missing
environment variable as "this source doesn't apply" rather than an error,
while treating a malformed one as a real misconfiguration, is the same
distinction most configuration-loading code needs to make and often gets
wrong by conflating "unset" with "empty" or "invalid."

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [os.LookupEnv](https://pkg.go.dev/os#LookupEnv)
- [Stripe API rate limits](https://stripe.com/docs/rate-limits)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-transaction-isolation-levels.md](32-transaction-isolation-levels.md) | Next: [34-webhook-batch-delivery-service.md](34-webhook-batch-delivery-service.md)
