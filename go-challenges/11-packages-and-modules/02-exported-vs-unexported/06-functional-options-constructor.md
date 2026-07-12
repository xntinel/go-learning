# Exercise 6: A Constructor That Stays Backward-Compatible: Functional Options

A constructor that takes a giant exported `Config` struct turns every new field into a
potential breaking change and forces callers to know each field's zero-value meaning.
The functional-options pattern avoids both: an unexported `options` struct, an exported
`Option` type, and exported `With*` constructors let you add configuration knobs over
time without ever changing `New`'s signature. This exercise builds an HTTP-client
config that way and proves defaults, overrides, and later-wins ordering.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
httpclient/                independent module: example.com/httpclient
  go.mod                   go 1.26
  httpclient.go            unexported options; Option type; WithTimeout/WithRetries/WithLogger; New
  cmd/
    demo/
      main.go              builds clients with zero, one, and several options
  httpclient_test.go       package httpclient_test: defaults, single option, later-wins override
```

- Files: `httpclient.go`, `cmd/demo/main.go`, `httpclient_test.go`.
- Implement: an unexported `options` struct, an exported `Option` type (`func(*options)`), exported `WithTimeout`/`WithRetries`/`WithLogger`, and `New(opts ...Option)` that applies defaults then overrides; exported getters so behavior is observable.
- Test: build with zero options (defaults), one option, and several including a later option overriding an earlier one; assert observable behavior through the exported getters and the injected logger.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/02-exported-vs-unexported/06-functional-options-constructor/cmd/demo
cd go-solutions/11-packages-and-modules/02-exported-vs-unexported/06-functional-options-constructor
go mod edit -go=1.26
```

### Why the options struct stays unexported

The whole point of the pattern is that adding a configuration knob next year is not a
breaking change. `options` holds the raw configuration, and it is unexported: callers
never name it, never construct it, and never depend on its shape, so you can add a field
to it whenever a new `WithX` is needed without touching a single call site or changing
`New`'s signature. Contrast the alternatives. A single exported `Config` struct means
every new field is visible API, callers must understand the zero-value meaning of each,
and adding a required field is a breaking change. Telescoping constructors
(`New`, `NewWithTimeout`, `NewWithTimeoutAndRetries`) explode combinatorially. Functional
options give you one variadic constructor that is stable forever.

The mechanics: `Option` is `func(*options)`, an exported named type so it can appear in
`New`'s signature and in the return type of the `With*` funcs. Each `WithX` returns a
closure that captures its argument and sets the corresponding field when applied. `New`
builds an `options` seeded with defaults, then applies each option in order, so a later
option overrides an earlier one; finally it copies the resolved values into the `Client`.
The zero-argument call `New()` is valid and yields the defaults, which is a property
worth preserving: options are additive refinements, never mandatory ceremony.

The `Client`'s own fields are unexported; observation goes through exported getters
(`Timeout`, `Retries`) and through observable behavior (`Describe` calls the injected
logger). That keeps the internal representation free to change while giving callers and
tests a stable way to read the effective configuration.

Create `httpclient.go`:

```go
package httpclient

import (
	"fmt"
	"time"
)

// options is unexported: it can gain fields over time without breaking callers,
// because callers never name it.
type options struct {
	timeout time.Duration
	retries int
	logger  func(string)
}

// Option is the exported knob type. It appears in New's signature and as the
// return type of the With* constructors.
type Option func(*options)

// WithTimeout sets the request timeout.
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithRetries sets the retry count.
func WithRetries(n int) Option {
	return func(o *options) { o.retries = n }
}

// WithLogger installs a logging sink used by Describe.
func WithLogger(fn func(string)) Option {
	return func(o *options) { o.logger = fn }
}

// Client holds the resolved configuration in unexported fields.
type Client struct {
	timeout time.Duration
	retries int
	logger  func(string)
}

// New applies defaults, then the options in order (so a later option overrides an
// earlier one), then copies the resolved values into the Client. New() with no
// arguments is valid and returns the defaults.
func New(opts ...Option) *Client {
	o := options{
		timeout: 30 * time.Second,
		retries: 3,
		logger:  func(string) {}, // no-op default, so Describe never nil-panics
	}
	for _, opt := range opts {
		opt(&o)
	}
	return &Client{
		timeout: o.timeout,
		retries: o.retries,
		logger:  o.logger,
	}
}

// Timeout exposes the resolved timeout without exporting the field.
func (c *Client) Timeout() time.Duration { return c.timeout }

// Retries exposes the resolved retry count.
func (c *Client) Retries() int { return c.retries }

// Describe returns a human-readable summary and also emits it to the injected
// logger, giving tests an observable behavior for the logger option.
func (c *Client) Describe() string {
	s := fmt.Sprintf("timeout=%s retries=%d", c.timeout, c.retries)
	c.logger(s)
	return s
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/httpclient"
)

func main() {
	def := httpclient.New()
	fmt.Printf("defaults: %s\n", def.Describe())

	tuned := httpclient.New(
		httpclient.WithTimeout(5*time.Second),
		httpclient.WithRetries(1),
	)
	fmt.Printf("tuned: %s\n", tuned.Describe())

	// Later option wins: the second WithTimeout overrides the first.
	overridden := httpclient.New(
		httpclient.WithTimeout(1*time.Second),
		httpclient.WithTimeout(2*time.Second),
	)
	fmt.Printf("override: %s\n", overridden.Describe())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
defaults: timeout=30s retries=3
tuned: timeout=5s retries=1
override: timeout=2s retries=3
```

### Tests

The black-box test cannot see `options`, so it asserts through the exported getters and
the injected logger, exactly as a real caller would. `TestDefaults` pins the
zero-argument contract. `TestSingleOption` checks one knob leaves the others at their
defaults. `TestLaterOptionWins` proves ordering. `TestLoggerOption` proves the injected
logger is actually called by `Describe`.

Create `httpclient_test.go`:

```go
package httpclient_test

import (
	"fmt"
	"testing"
	"time"

	"example.com/httpclient"
)

func TestDefaults(t *testing.T) {
	t.Parallel()

	c := httpclient.New()
	if c.Timeout() != 30*time.Second {
		t.Fatalf("timeout = %s, want 30s", c.Timeout())
	}
	if c.Retries() != 3 {
		t.Fatalf("retries = %d, want 3", c.Retries())
	}
}

func TestSingleOption(t *testing.T) {
	t.Parallel()

	c := httpclient.New(httpclient.WithRetries(7))
	if c.Retries() != 7 {
		t.Fatalf("retries = %d, want 7", c.Retries())
	}
	if c.Timeout() != 30*time.Second {
		t.Fatalf("timeout = %s, want default 30s", c.Timeout())
	}
}

func TestLaterOptionWins(t *testing.T) {
	t.Parallel()

	c := httpclient.New(
		httpclient.WithTimeout(1*time.Second),
		httpclient.WithTimeout(2*time.Second),
	)
	if c.Timeout() != 2*time.Second {
		t.Fatalf("timeout = %s, want 2s (later option wins)", c.Timeout())
	}
}

func TestLoggerOption(t *testing.T) {
	t.Parallel()

	var logged []string
	c := httpclient.New(httpclient.WithLogger(func(s string) {
		logged = append(logged, s)
	}))
	_ = c.Describe()
	if len(logged) != 1 {
		t.Fatalf("logger called %d times, want 1", len(logged))
	}
}

func ExampleNew() {
	c := httpclient.New(httpclient.WithTimeout(5*time.Second), httpclient.WithRetries(1))
	fmt.Println(c.Describe())
	// Output: timeout=5s retries=1
}
```

## Review

The pattern is correct when `New()` with no arguments returns the documented defaults,
when a single option leaves the other fields at their defaults, and when two options
setting the same field resolve to the last one applied, all of which the tests assert
through the exported getters because `options` is unreachable. The design payoff is
future-proofing: adding a `WithProxy` next year means one new `With*` func and one new
field on the unexported `options`, with no change to `New`'s signature and no broken
callers, which is exactly what a giant exported `Config` cannot give you. The no-op
default logger matters too: it ensures `Describe` never nil-panics when the caller
supplies no logger, a small invariant the pattern lets you guarantee inside `New`.

## Resources

- [Rob Pike: Self-referential functions and the design of options](https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html) — the original articulation of functional options.
- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis) — the pattern applied to a real constructor.
- [Google Go Style Decisions: option structs and variadic options](https://google.github.io/styleguide/go/decisions#option-structure) — when to prefer options and how to name them.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-repository-interface-unexported-impl.md](05-repository-interface-unexported-impl.md) | Next: [07-sentinel-errors-and-error-type.md](07-sentinel-errors-and-error-type.md)
