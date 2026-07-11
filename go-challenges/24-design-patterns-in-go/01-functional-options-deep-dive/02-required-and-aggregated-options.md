# Exercise 2: Required Fields and Aggregated Errors

Functional options model optional settings cleanly, but real constructors also carry a *required* input and a *cross-field* invariant that no single option can enforce. This exercise builds a database `Client` whose DSN is required, whose `maxIdleConns` must not exceed `maxOpenConns`, and whose constructor reports every problem at once with `errors.Join` instead of stopping at the first.

This module is fully self-contained. It starts with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
client.go              Client, Option, New (aggregating), With* options, sentinel errors, accessors
cmd/
  demo/
    main.go            build a valid client, then show a misconfigured call reporting every error
client_test.go         required enforcement, cross-field rule, aggregation, per-option validation
```

- Files: `client.go`, `cmd/demo/main.go`, `client_test.go`.
- Implement: `Option func(*Client) error`, `New(opts ...Option) (*Client, error)` that collects all errors, `WithDSN` (required) plus optional `WithMaxOpenConns`, `WithMaxIdleConns`, `WithConnMaxLifetime`, `WithAppName`, and read-only accessors.
- Test: `client_test.go` proves a missing DSN is rejected, the `idle <= open` invariant holds, a single bad call surfaces all three of its problems through `errors.Is`, and each per-option validator fires.
- Verify: `go test -race ./...` then `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p required-options/cmd/demo && cd required-options
go mod init example.com/required-options
```

### Why aggregate, and where required and cross-field checks belong

Exercise 1's constructor stops at the first failing option. That is the right default for a boot path, but it is the wrong experience for a configuration assembled from a file or a flag set, where the user wants to see *every* mistake in one pass rather than fix-one-rerun-repeat. This constructor takes the other strategy: it runs every option, appends each returned error to a slice, then returns `errors.Join(errs...)` — a single error whose `Error()` prints each cause on its own line and through which `errors.Is` still finds every joined sentinel.

Two checks deliberately live *outside* the options. The first is the required DSN. A required field cannot be validated by an option, because the failure case is the option never being passed at all. The clean enforcement is to give `dsn` no default and test it after the loop: if `c.dsn == ""` once every option has run, the field was never supplied, and a missing DSN and an explicitly-empty one collapse into the same `ErrMissingDSN`. That is why `WithDSN` itself does no validation — it simply assigns, and the final check owns the rule.

The second is the cross-field invariant `maxIdleConns <= maxOpenConns`. It cannot live inside `WithMaxIdleConns` or `WithMaxOpenConns`, because each option sees only its own argument and the other field is in whatever state the option ordering left it. A cross-field rule must read the fully-assembled value once, after the loop. Note the guard `c.maxOpenConns > 0`: a zero open limit means "unlimited", so the ceiling does not apply and any idle count is legal.

The per-option validators (`WithMaxOpenConns`, `WithMaxIdleConns`, `WithConnMaxLifetime`, `WithAppName`) still reject their own bad inputs immediately by returning a wrapped sentinel; those errors flow into the same `errs` slice. The result is that one `New` call can report a negative open count, a missing DSN, and an idle-exceeds-open violation together, and a caller can branch on any one of them with `errors.Is`.

Create `client.go`:

```go
package dbclient

import (
	"errors"
	"fmt"
	"time"
)

// Option configures a Client. Unlike a short-circuiting constructor, New here
// runs every option and collects their errors, so one bad call reports all of
// its problems at once instead of only the first.
type Option func(*Client) error

// Client holds a validated connection configuration. dsn has no default: it is
// required, and a Client cannot be built without it.
type Client struct {
	dsn             string
	maxOpenConns    int
	maxIdleConns    int
	connMaxLifetime time.Duration
	appName         string
}

var (
	ErrMissingDSN      = errors.New("dsn is required")
	ErrInvalidMaxOpen  = errors.New("max open conns must not be negative")
	ErrInvalidMaxIdle  = errors.New("max idle conns must not be negative")
	ErrIdleExceedsOpen = errors.New("max idle conns must not exceed max open conns")
	ErrBadLifetime     = errors.New("conn max lifetime must not be negative")
	ErrEmptyAppName    = errors.New("app name must not be empty")
)

// New applies defaults, runs all options collecting their errors, then performs
// the validations that cannot belong to any single option: the required-field
// check (dsn) and the cross-field invariant (idle <= open). Every problem is
// reported together via errors.Join, and errors.Is still finds each sentinel
// inside the joined result.
func New(opts ...Option) (*Client, error) {
	c := &Client{
		maxOpenConns: 10,
		maxIdleConns: 2,
		appName:      "app",
	}
	var errs []error
	for _, opt := range opts {
		if err := opt(c); err != nil {
			errs = append(errs, err)
		}
	}
	if c.dsn == "" {
		errs = append(errs, ErrMissingDSN)
	}
	if c.maxOpenConns > 0 && c.maxIdleConns > c.maxOpenConns {
		errs = append(errs, fmt.Errorf("%w: idle=%d open=%d", ErrIdleExceedsOpen, c.maxIdleConns, c.maxOpenConns))
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("dbclient: %w", errors.Join(errs...))
	}
	return c, nil
}

// WithDSN supplies the required data source name. An empty value is left for the
// required-field check in New to reject, so the error reads "dsn is required"
// rather than a separate "empty dsn" message.
func WithDSN(dsn string) Option {
	return func(c *Client) error {
		c.dsn = dsn
		return nil
	}
}

func WithMaxOpenConns(n int) Option {
	return func(c *Client) error {
		if n < 0 {
			return fmt.Errorf("%w: got %d", ErrInvalidMaxOpen, n)
		}
		c.maxOpenConns = n
		return nil
	}
}

func WithMaxIdleConns(n int) Option {
	return func(c *Client) error {
		if n < 0 {
			return fmt.Errorf("%w: got %d", ErrInvalidMaxIdle, n)
		}
		c.maxIdleConns = n
		return nil
	}
}

func WithConnMaxLifetime(d time.Duration) Option {
	return func(c *Client) error {
		if d < 0 {
			return fmt.Errorf("%w: got %s", ErrBadLifetime, d)
		}
		c.connMaxLifetime = d
		return nil
	}
}

func WithAppName(name string) Option {
	return func(c *Client) error {
		if name == "" {
			return ErrEmptyAppName
		}
		c.appName = name
		return nil
	}
}

func (c *Client) DSN() string                    { return c.dsn }
func (c *Client) MaxOpenConns() int              { return c.maxOpenConns }
func (c *Client) MaxIdleConns() int              { return c.maxIdleConns }
func (c *Client) ConnMaxLifetime() time.Duration { return c.connMaxLifetime }
func (c *Client) AppName() string                { return c.appName }
```

### The runnable demo

The demo first builds a fully valid client and prints its state, then makes a deliberately broken call — no DSN, a negative open count, and an idle count that exceeds the (defaulted) open limit — and prints the joined error plus two `errors.Is` probes. Because the negative `WithMaxOpenConns(-1)` is rejected by its own option, `maxOpenConns` stays at the default 10, which is what the cross-field check compares the idle count of 50 against.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/required-options"
)

func main() {
	c, err := dbclient.New(
		dbclient.WithDSN("postgres://localhost:5432/app"),
		dbclient.WithMaxOpenConns(20),
		dbclient.WithMaxIdleConns(5),
		dbclient.WithConnMaxLifetime(30*time.Minute),
		dbclient.WithAppName("billing"),
	)
	if err != nil {
		fmt.Println("unexpected error:", err)
		return
	}
	fmt.Printf("dsn=%s open=%d idle=%d lifetime=%s app=%s\n",
		c.DSN(), c.MaxOpenConns(), c.MaxIdleConns(), c.ConnMaxLifetime(), c.AppName())

	// A misconfigured call: no DSN, a negative open count, and idle > open.
	_, err = dbclient.New(
		dbclient.WithMaxOpenConns(-1),
		dbclient.WithMaxIdleConns(50),
	)
	fmt.Println("aggregated error:")
	fmt.Println(err)
	fmt.Println("missing dsn? ", errors.Is(err, dbclient.ErrMissingDSN))
	fmt.Println("bad max open?", errors.Is(err, dbclient.ErrInvalidMaxOpen))
}
```

The import path is the module path `example.com/required-options`; the package it names is `dbclient`. Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
dsn=postgres://localhost:5432/app open=20 idle=5 lifetime=30m0s app=billing
aggregated error:
dbclient: max open conns must not be negative: got -1
dsn is required
max idle conns must not exceed max open conns: idle=50 open=10
missing dsn?  true
bad max open? true
```

### Tests

The tests pin the four properties that make this constructor different from Exercise 1's: required enforcement, the cross-field rule, the all-at-once aggregation, and that the unlimited (`open == 0`) case skips the ceiling. `TestNewAggregatesAllErrors` is the central one — it asserts that a single call's joined error contains all three sentinels at once.

Create `client_test.go`:

```go
package dbclient

import (
	"errors"
	"testing"
	"time"
)

func TestNewRequiresDSN(t *testing.T) {
	t.Parallel()

	_, err := New(WithMaxOpenConns(5))
	if !errors.Is(err, ErrMissingDSN) {
		t.Fatalf("err = %v, want ErrMissingDSN", err)
	}
}

func TestNewAcceptsMinimalValid(t *testing.T) {
	t.Parallel()

	c, err := New(WithDSN("postgres://x"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if c.DSN() != "postgres://x" {
		t.Fatalf("dsn = %q", c.DSN())
	}
	if c.MaxOpenConns() != 10 || c.MaxIdleConns() != 2 || c.AppName() != "app" {
		t.Fatalf("defaults wrong: open=%d idle=%d app=%q", c.MaxOpenConns(), c.MaxIdleConns(), c.AppName())
	}
}

func TestEmptyDSNCountsAsMissing(t *testing.T) {
	t.Parallel()

	_, err := New(WithDSN(""))
	if !errors.Is(err, ErrMissingDSN) {
		t.Fatalf("err = %v, want ErrMissingDSN", err)
	}
}

func TestCrossFieldIdleExceedsOpen(t *testing.T) {
	t.Parallel()

	_, err := New(WithDSN("x"), WithMaxOpenConns(4), WithMaxIdleConns(9))
	if !errors.Is(err, ErrIdleExceedsOpen) {
		t.Fatalf("err = %v, want ErrIdleExceedsOpen", err)
	}
}

func TestNewAggregatesAllErrors(t *testing.T) {
	t.Parallel()

	// Three independent problems: bad open count, missing dsn, idle > open.
	_, err := New(WithMaxOpenConns(-1), WithMaxIdleConns(50))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	for _, want := range []error{ErrInvalidMaxOpen, ErrMissingDSN, ErrIdleExceedsOpen} {
		if !errors.Is(err, want) {
			t.Errorf("joined error missing %v; got %v", want, err)
		}
	}
}

func TestPerOptionValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opt  Option
		want error
	}{
		{"negative open", WithMaxOpenConns(-1), ErrInvalidMaxOpen},
		{"negative idle", WithMaxIdleConns(-1), ErrInvalidMaxIdle},
		{"negative lifetime", WithConnMaxLifetime(-time.Second), ErrBadLifetime},
		{"empty app name", WithAppName(""), ErrEmptyAppName},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(WithDSN("x"), tc.opt)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAllSettersTakeEffect(t *testing.T) {
	t.Parallel()

	c, err := New(
		WithDSN("dsn"),
		WithMaxOpenConns(30),
		WithMaxIdleConns(8),
		WithConnMaxLifetime(time.Hour),
		WithAppName("svc"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxOpenConns() != 30 || c.MaxIdleConns() != 8 {
		t.Fatalf("conns wrong: open=%d idle=%d", c.MaxOpenConns(), c.MaxIdleConns())
	}
	if c.ConnMaxLifetime() != time.Hour || c.AppName() != "svc" {
		t.Fatalf("lifetime/app wrong: %s %q", c.ConnMaxLifetime(), c.AppName())
	}
}

func TestZeroOpenMeansUnlimitedSkipsCrossField(t *testing.T) {
	t.Parallel()

	// open == 0 means unlimited, so the idle <= open invariant does not apply.
	c, err := New(WithDSN("x"), WithMaxOpenConns(0), WithMaxIdleConns(100))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.MaxOpenConns() != 0 || c.MaxIdleConns() != 100 {
		t.Fatalf("got open=%d idle=%d", c.MaxOpenConns(), c.MaxIdleConns())
	}
}
```

## Review

The constructor is correct when the two checks that no option can own live after the loop. The required-DSN check must read `c.dsn == ""` post-loop rather than inside `WithDSN`, or a caller who omits `WithDSN` entirely sails through; the test `TestNewRequiresDSN` (which passes only `WithMaxOpenConns`) is what catches that mistake. The cross-field check must read both fields once after the loop and must guard the unlimited case with `maxOpenConns > 0`, or `TestZeroOpenMeansUnlimitedSkipsCrossField` fails. The aggregation is correct only if `New` keeps collecting after the first failure instead of returning early — `TestNewAggregatesAllErrors` asserts three sentinels in one returned error, which a short-circuiting loop can never satisfy. Confirm `errors.Join` is used (not string concatenation), because that is what keeps every sentinel matchable through `errors.Is`. With those holding under `go test -race ./...`, the required/aggregate contract is sound.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — combines multiple errors into one whose `Is`/`As` traverse every wrapped cause.
- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `%w`, `errors.Is`, and the wrapping model the sentinels rely on.
- [`database/sql.DB.SetMaxIdleConns`](https://pkg.go.dev/database/sql#DB.SetMaxIdleConns) — the real-world pool knobs this Client mirrors, including the idle-vs-open relationship.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-server-options.md](01-server-options.md) | Next: [03-generic-options.md](03-generic-options.md)
