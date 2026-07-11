# Exercise 5: Constructing an Entity with Functional Options and Safe Defaults

Real entities have a few required fields and several optional ones with sensible
defaults, and a positional constructor with eight arguments is unreadable and
un-extendable. The functional-options pattern keeps required fields positional and
takes the rest as `opts ...Option`, applies them onto a config, then validates
once — and it makes an injected clock turn `CreatedAt` from untestable into
deterministic.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
account/                    independent module: example.com/account
  go.mod                    go 1.26
  account.go                type Account; type Option; NewAccount(id, opts...); With* options
  cmd/
    demo/
      main.go               runnable demo: defaults, options, injected clock
  account_test.go           tests: defaults, WithInitialBalance, invalid option, deterministic clock, required id
```

- Files: `account.go`, `cmd/demo/main.go`, `account_test.go`.
- Implement: `NewAccount(id string, opts ...Option) (*Account, error)` with `WithInitialBalance`, `WithDisplayName`, `WithClock`; defaults for each; validation-after-apply aggregated with `errors.Join`.
- Test: zero options yields the documented defaults; `WithInitialBalance` sets and validates; an invalid option fails with a wrapped error; an injected clock makes `CreatedAt` deterministic; a missing id is rejected.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/account/cmd/demo
cd ~/go-exercises/account
go mod init example.com/account
```

### Options apply, then the constructor validates once

The functional-options pattern separates *what the caller wants to set* from *how
the type validates*. Each `Option` is a `func(*config)` that mutates a private
`config` struct holding the optional fields. `NewAccount` starts the config at its
defaults, applies every option in order, and only then validates the assembled
config as a whole. Two properties follow. First, option *ordering* is
last-write-wins: if the caller passes `WithInitialBalance(100)` then
`WithInitialBalance(200)`, the config ends at `200`, and that is fine because
validation runs after all options are applied, never in the middle. Second,
validation is centralized: every field is checked in one place, after assembly, so
there is no way for an option to smuggle in an invalid value — the option only
*sets*, the constructor *decides*.

Defaults are the point of the pattern. Zero options must yield a fully valid
`Account`: the balance defaults to `0`, the display name defaults to the id (a
sensible fallback), and the clock defaults to `time.Now`. Because the clock is a
field on the config, `WithClock` can inject a fixed function in a test so that
`CreatedAt` is a known constant instead of "whenever the test happened to run".
That is the standard trick for making time-stamped construction testable without a
global clock or a `//go:linkname` hack: inject the clock as an option, default it
to `time.Now`, and the production path is unchanged while the test path is
deterministic.

Validation aggregates with `errors.Join`. Rather than returning at the first bad
field, the constructor collects every violation — empty id, negative balance,
empty display name — and returns them joined, so a caller passing two bad options
sees both problems at once. Each individual error wraps a sentinel, so
`errors.Is` still matches against the joined result (`errors.Is` walks a joined
error's tree). The `id` is required and positional, so the "id is required" check
is the one invariant that cannot be defaulted away.

Create `account.go`:

```go
package account

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrEmptyID       = errors.New("account: id is required")
	ErrNegativeStart = errors.New("account: initial balance must be non-negative")
	ErrEmptyName     = errors.New("account: display name must not be empty")
)

// config holds the optional fields an Option may set, seeded with defaults.
type config struct {
	balance     int64
	displayName string
	now         func() time.Time
}

// Option configures a new Account. Options only set; the constructor validates.
type Option func(*config)

// WithInitialBalance sets the opening balance in minor units.
func WithInitialBalance(minor int64) Option {
	return func(c *config) { c.balance = minor }
}

// WithDisplayName sets a human-facing name; defaults to the id.
func WithDisplayName(name string) Option {
	return func(c *config) { c.displayName = name }
}

// WithClock injects the clock used for CreatedAt; defaults to time.Now.
func WithClock(now func() time.Time) Option {
	return func(c *config) { c.now = now }
}

// Account is an entity constructed via functional options with safe defaults.
type Account struct {
	id          string
	balance     int64
	displayName string
	createdAt   time.Time
}

// NewAccount builds an Account. The id is required and positional; everything
// else has a default. Options are applied, then the whole config is validated.
func NewAccount(id string, opts ...Option) (*Account, error) {
	cfg := config{
		balance:     0,
		displayName: id, // default: display name falls back to the id
		now:         time.Now,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	var errs []error
	if id == "" {
		errs = append(errs, ErrEmptyID)
	}
	if cfg.balance < 0 {
		errs = append(errs, fmt.Errorf("%w: got %d", ErrNegativeStart, cfg.balance))
	}
	if cfg.displayName == "" {
		errs = append(errs, ErrEmptyName)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return &Account{
		id:          id,
		balance:     cfg.balance,
		displayName: cfg.displayName,
		createdAt:   cfg.now(),
	}, nil
}

func (a *Account) ID() string           { return a.id }
func (a *Account) Balance() int64       { return a.balance }
func (a *Account) DisplayName() string  { return a.displayName }
func (a *Account) CreatedAt() time.Time { return a.createdAt }
```

### The runnable demo

The demo shows all three shapes: defaults only, options set, and an injected clock
that makes `CreatedAt` a fixed instant.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/account"
)

func main() {
	a, _ := account.NewAccount("acct-1")
	fmt.Printf("defaults: name=%q balance=%d\n", a.DisplayName(), a.Balance())

	fixed := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	b, _ := account.NewAccount("acct-2",
		account.WithInitialBalance(5000),
		account.WithDisplayName("Payments Float"),
		account.WithClock(func() time.Time { return fixed }),
	)
	fmt.Printf("configured: name=%q balance=%d created=%s\n",
		b.DisplayName(), b.Balance(), b.CreatedAt().Format(time.RFC3339))

	if _, err := account.NewAccount("acct-3", account.WithInitialBalance(-1)); err != nil {
		fmt.Printf("rejected: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
defaults: name="acct-1" balance=0
configured: name="Payments Float" balance=5000 created=2026-01-02T15:04:05Z
rejected: account: initial balance must be non-negative: got -1
```

### Tests

The tests cover each guarantee: defaults with zero options, `WithInitialBalance`
setting and validating, an invalid option producing a wrapped error matchable with
`errors.Is`, the injected clock giving a deterministic `CreatedAt`, and the missing
id still rejected. The `TestAggregatedErrors` case passes two bad options and
asserts both sentinels are present in the joined error.

Create `account_test.go`:

```go
package account

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	t.Parallel()
	a, err := NewAccount("acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if a.DisplayName() != "acct-1" {
		t.Fatalf("DisplayName = %q, want acct-1 (default = id)", a.DisplayName())
	}
	if a.Balance() != 0 {
		t.Fatalf("Balance = %d, want 0", a.Balance())
	}
}

func TestWithInitialBalance(t *testing.T) {
	t.Parallel()
	a, err := NewAccount("acct-1", WithInitialBalance(2500))
	if err != nil {
		t.Fatal(err)
	}
	if a.Balance() != 2500 {
		t.Fatalf("Balance = %d, want 2500", a.Balance())
	}
}

func TestInvalidOptionRejected(t *testing.T) {
	t.Parallel()
	if _, err := NewAccount("acct-1", WithInitialBalance(-1)); !errors.Is(err, ErrNegativeStart) {
		t.Fatalf("err = %v, want ErrNegativeStart", err)
	}
}

func TestInjectedClockIsDeterministic(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	a, err := NewAccount("acct-1", WithClock(func() time.Time { return fixed }))
	if err != nil {
		t.Fatal(err)
	}
	if !a.CreatedAt().Equal(fixed) {
		t.Fatalf("CreatedAt = %s, want %s", a.CreatedAt(), fixed)
	}
}

func TestRequiredIDEnforced(t *testing.T) {
	t.Parallel()
	if _, err := NewAccount(""); !errors.Is(err, ErrEmptyID) {
		t.Fatalf("err = %v, want ErrEmptyID", err)
	}
}

func TestAggregatedErrors(t *testing.T) {
	t.Parallel()
	_, err := NewAccount("", WithInitialBalance(-1), WithDisplayName(""))
	if !errors.Is(err, ErrEmptyID) || !errors.Is(err, ErrNegativeStart) || !errors.Is(err, ErrEmptyName) {
		t.Fatalf("joined err = %v, want all three sentinels", err)
	}
}

func ExampleNewAccount() {
	fixed := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	a, _ := NewAccount("acct-1",
		WithInitialBalance(5000),
		WithDisplayName("Float"),
		WithClock(func() time.Time { return fixed }),
	)
	fmt.Printf("%s %d %s\n", a.DisplayName(), a.Balance(), a.CreatedAt().Format(time.RFC3339))
	// Output: Float 5000 2026-01-02T15:04:05Z
}
```

## Review

`NewAccount` is correct when zero options yield a valid, fully-defaulted account
and every option only sets a value that the single post-apply validation pass then
checks. The injected clock is the testability payoff: `CreatedAt` is deterministic
because the option defaults to `time.Now` in production and to a fixed function in
the test. The mistakes to avoid: validating inside an option (which breaks the
last-write-wins ordering), and returning at the first error instead of joining
(which hides the second problem from the caller). Because `errors.Join` builds a
tree, `errors.Is` still matches each wrapped sentinel inside it.

## Resources

- [Functional options for friendly APIs (Dave Cheney)](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis) — the origin of the pattern.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — aggregating multiple validation errors.
- [`time` package](https://pkg.go.dev/time) — `time.Now`, `time.Date`, and `Time.Equal`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-money-currency-safe-arithmetic.md](04-money-currency-safe-arithmetic.md) | Next: [06-domain-dto-json-boundary.md](06-domain-dto-json-boundary.md)
