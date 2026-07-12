# Exercise 4: A Constructor With Sensible Defaults Via Functional Options

Once a service has more than one tunable knob, a positional parameter list stops
scaling: every new option either lengthens the signature or forces callers to pass
zero values they do not care about. Functional options fix this. You reshape the
constructor to `NewService(repo, opts ...Option)` — required dependency positional,
everything else an additive, defaulted option — so callers and tests set only what
matters.

This module is fully self-contained, with its own `go mod init`, code, demo, and
tests.

## What you'll build

```text
diopts/                     independent module: example.com/diopts
  go.mod                    module example.com/diopts
  service.go                Service; Option = func(*Service); WithClock/WithLogger/WithTimeout; defaults
  cmd/
    demo/
      main.go               constructs with defaults, then with a couple of options
  service_test.go           defaults-applied, single-option, combined, last-writer-wins tests
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: `type Option func(*Service)`; `NewService(repo Repository, opts ...Option) *Service` that installs defaults (`RealClock()`, `slog.Default()`, a fixed timeout) then applies each option; `WithClock(Clock)`, `WithLogger(*slog.Logger)`, `WithTimeout(time.Duration)`; exported accessors so `cmd/demo` and tests can read the resulting configuration.
- Test: construct with zero options (defaults present), with `WithClock` alone (only the clock changed), with several options combined, and two `WithTimeout` in sequence (last writer wins).
- Verify: `go test -count=1 -race ./...`

### Why options, and how the pattern works

The required dependency — the repository — stays positional: a service with no
storage is meaningless, so the type system should demand it at every call site. The
optional, tunable collaborators become options. An `Option` is just a
`func(*Service)` that mutates a partially-built service. `NewService` builds the
service with all defaults first, then loops over the options applying each in turn.
Because the options run in order and later ones overwrite earlier ones, the rule is
last-writer-wins, which is defined and testable — passing `WithTimeout(1*time.Second),
WithTimeout(2*time.Second)` leaves the timeout at two seconds.

The defaults are the contract. With zero options, `NewService(repo)` must produce a
fully working service: `RealClock()` for the clock, `slog.Default()` for the logger,
and a fixed default timeout (five seconds here). A caller that only cares about the
clock writes `NewService(repo, WithClock(fake))` and inherits the other two
defaults untouched. This is the property the tests pin: an option you do not pass
leaves its documented default in place.

Compare the alternatives. A growing positional list —
`NewService(repo, clock, logger, timeout, retries, ...)` — breaks every call site
each time a knob is added and forces callers to spell out values they do not care
about. A mutable `Config` struct passed by value or pointer invites half-filled
configs where a zero `time.Duration` silently means "no timeout" rather than "use
the default". Functional options make the default explicit (it is set before any
option runs) and make each override self-describing at the call site.

Create `service.go`:

```go
package diopts

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// ErrNotFound is returned by a Repository when the id has no stored value.
var ErrNotFound = errors.New("not found")

// DefaultTimeout is the per-request timeout applied when WithTimeout is not set.
const DefaultTimeout = 5 * time.Second

type Clock interface {
	Now() time.Time
}

type Repository interface {
	Get(ctx context.Context, id string) (string, error)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// RealClock is the production Clock, and the default when WithClock is not set.
func RealClock() Clock { return realClock{} }

// Service serves read requests with a configurable clock, logger, and timeout.
type Service struct {
	repo    Repository
	clock   Clock
	logger  *slog.Logger
	timeout time.Duration
}

// Option configures a Service during construction. It is applied after the
// defaults, so an option overrides the corresponding default.
type Option func(*Service)

// WithClock overrides the default RealClock.
func WithClock(c Clock) Option {
	return func(s *Service) { s.clock = c }
}

// WithLogger overrides the default slog.Default logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) { s.logger = l }
}

// WithTimeout overrides the default per-request timeout.
func WithTimeout(d time.Duration) Option {
	return func(s *Service) { s.timeout = d }
}

// NewService takes the required repository positionally and installs defaults
// for every optional knob, then applies the options in order (last writer
// wins). The required dependency cannot be omitted; the optional ones need not
// be supplied.
func NewService(repo Repository, opts ...Option) *Service {
	s := &Service{
		repo:    repo,
		clock:   RealClock(),
		logger:  slog.Default(),
		timeout: DefaultTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Timeout reports the configured per-request timeout.
func (s *Service) Timeout() time.Duration { return s.timeout }

// Now reports the current time through the configured clock, so callers and
// tests can observe which clock is in effect.
func (s *Service) Now() time.Time { return s.clock.Now() }

// Get reads through the repository under the configured timeout and logs the
// access through the configured logger.
func (s *Service) Get(ctx context.Context, id string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	s.logger.Debug("getting item", "id", id)
	return s.repo.Get(ctx, id)
}
```

### The runnable demo

The demo constructs a service with defaults and prints the default timeout, then
constructs another with `WithClock` and `WithTimeout` to show the overrides take
effect while unset knobs keep their defaults.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/diopts"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type memRepo struct{}

func (memRepo) Get(_ context.Context, _ string) (string, error) { return "v", nil }

func main() {
	def := diopts.NewService(memRepo{})
	fmt.Println("default timeout:", def.Timeout())

	custom := diopts.NewService(memRepo{},
		diopts.WithClock(fixedClock{t: time.Unix(0, 0).UTC()}),
		diopts.WithTimeout(2*time.Second),
	)
	fmt.Println("custom timeout:", custom.Timeout())
	fmt.Println("custom clock now:", custom.Now().UTC().Format(time.RFC3339))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
default timeout: 5s
custom timeout: 2s
custom clock now: 1970-01-01T00:00:00Z
```

### Tests

The tests pin the four properties that make options trustworthy: defaults are
applied when no option is passed, a single option changes only its own field,
several options compose, and two options touching the same field resolve
last-writer-wins.

Create `service_test.go`:

```go
package diopts

import (
	"context"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c fakeClock) Now() time.Time { return c.t }

type memRepo struct{}

func (memRepo) Get(_ context.Context, _ string) (string, error) { return "v", nil }

func TestDefaultsApplied(t *testing.T) {
	t.Parallel()

	s := NewService(memRepo{})
	if s.Timeout() != DefaultTimeout {
		t.Fatalf("timeout = %v, want default %v", s.Timeout(), DefaultTimeout)
	}
	if s.clock == nil || s.logger == nil {
		t.Fatal("defaults for clock and logger must be installed")
	}
}

func TestWithClockAloneLeavesOtherDefaults(t *testing.T) {
	t.Parallel()

	fixed := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	s := NewService(memRepo{}, WithClock(fakeClock{t: fixed}))

	if !s.Now().Equal(fixed) {
		t.Fatalf("Now() = %v, want injected %v", s.Now(), fixed)
	}
	if s.Timeout() != DefaultTimeout {
		t.Fatalf("timeout = %v, want untouched default %v", s.Timeout(), DefaultTimeout)
	}
}

func TestCombinedOptions(t *testing.T) {
	t.Parallel()

	fixed := time.Unix(0, 0).UTC()
	s := NewService(memRepo{},
		WithClock(fakeClock{t: fixed}),
		WithTimeout(2*time.Second),
	)
	if !s.Now().Equal(fixed) {
		t.Fatalf("Now() = %v, want %v", s.Now(), fixed)
	}
	if s.Timeout() != 2*time.Second {
		t.Fatalf("timeout = %v, want 2s", s.Timeout())
	}
}

func TestLastWriterWins(t *testing.T) {
	t.Parallel()

	s := NewService(memRepo{},
		WithTimeout(1*time.Second),
		WithTimeout(3*time.Second),
	)
	if s.Timeout() != 3*time.Second {
		t.Fatalf("timeout = %v, want last-writer 3s", s.Timeout())
	}
}
```

The test's `memRepo` satisfies `Repository`, so its `Get` signature must name
`context.Context`; that is why the test file imports `context`.

## Review

The constructor is correct when the required repository is positional and every
optional knob is a defaulted option. `TestDefaultsApplied` proves zero options
yields a working service; `TestWithClockAloneLeavesOtherDefaults` proves an option
changes only its field; `TestLastWriterWins` proves ordering is defined. The
mistake to avoid is smuggling a required dependency into an option — if `WithRepo`
existed, a caller could forget it and construct a service with a nil repo, losing
exactly the compile-time guarantee the positional parameter provides. Keep required
positional, optional as options. Run `go test -race` to confirm the options compose
without surprises.

## Resources

- [Functional options for friendly APIs (Dave Cheney)](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis) — the canonical write-up of this pattern.
- [Self-referential functions and the design of options (Rob Pike)](https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html) — the original idea.
- [log/slog: Default](https://pkg.go.dev/log/slog#Default) — the default logger used when `WithLogger` is not supplied.

---

Back to [03-nil-object-optional-logger.md](03-nil-object-optional-logger.md) | Next: [05-consumer-defined-narrow-interfaces.md](05-consumer-defined-narrow-interfaces.md)
