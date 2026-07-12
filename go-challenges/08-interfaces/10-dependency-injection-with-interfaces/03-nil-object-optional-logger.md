# Exercise 3: Graceful Degradation — A Service That Tolerates A Nil Optional Dependency

Not every dependency is required. A repository is load-bearing; a logger is a
convenience. This exercise draws that line in the constructor: a nil logger is
substituted with a no-op null-object so `Get` never nil-checks and never panics,
while a nil repository is rejected at construction time so a missing
load-bearing dependency fails fast instead of being silently swallowed.

This module is fully self-contained, with its own `go mod init`, code, demo, and
tests.

## What you'll build

```text
service/                    independent module: example.com/service
  go.mod                    module example.com/service
  service.go                Service; NewService returns error; nopLogger null-object; nil-repo fail-fast
  cmd/
    demo/
      main.go               constructs with a nil logger, calls Get, then shows the nil-repo error
  service_test.go           nil-logger-no-panic and nil-repo-rejected tests
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: `NewService(clock, repo, logger) (*Service, error)` that returns an error when `repo` is nil (required, fail fast), substitutes a `nopLogger` when `logger` is nil (optional, null-object), and substitutes `RealClock()` when `clock` is nil (optional default). `Get` calls the logger unconditionally.
- Test: `TestServiceWithNilLoggerDoesNotPanic` (pass nil logger, call `Get`, assert no panic and a correct result); `TestNilRepoRejected` (pass nil repo, assert the constructor returns a non-nil error).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/10-dependency-injection-with-interfaces/03-nil-object-optional-logger/cmd/demo
cd go-solutions/08-interfaces/10-dependency-injection-with-interfaces/03-nil-object-optional-logger
```

### Required versus optional, encoded in the constructor

The design decision is which dependencies the service cannot function without.
Storage is one of them: a service with no repository has nothing to read, and a
`Get` that silently returned an empty string for a nil repo would hide a wiring bug
behind plausible-looking data. So the constructor treats `repo` as required and
returns `ErrNilRepository` when it is nil — the failure surfaces at startup, in the
composition root, where it is cheap to diagnose, not at request time under load.

The logger is the opposite. A service that cannot log is degraded but still
correct; crashing because no logger was supplied would be a worse failure than the
missing logs. So the constructor treats `logger` as optional and applies the
null-object pattern: when `logger` is nil it substitutes `nopLogger{}`, a type
whose `Info` does nothing. After that substitution `s.logger` is never nil, so
`Get` calls `s.logger.Info(...)` unconditionally with no guard — the nil-check
lives in one place, the constructor, instead of being scattered through every
method. The clock gets the same treatment with a real default (`RealClock()`),
because "no clock supplied" has an obvious safe meaning: use the wall clock.

The modern alternative to a hand-written `nopLogger` is `slog`: a service that
injects a `*slog.Logger` would default a nil to `slog.New(slog.DiscardHandler)`
(Go 1.24+), the standard library's own null-object logger. The pattern is
identical; `nopLogger` here just makes the mechanism explicit against the
one-method `Logger` interface.

A note on the typed-nil gotcha: the constructor compares `logger == nil` while it
still holds the interface value the caller passed. If the caller passed an
untyped `nil`, this is true and the substitution happens. If the caller passed a
typed nil (a `(*myLogger)(nil)` wrapped in the interface), `logger == nil` is
false and no substitution occurs — which is correct, because a typed-nil receiver
is the caller's own implementation to make nil-safe. The constructor only defends
against the untyped nil it is designed to accept.

Create `service.go`:

```go
package service

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by a Repository when the id has no stored value.
var ErrNotFound = errors.New("not found")

// ErrNilRepository is returned by NewService when the required repository is
// nil. A required dependency fails fast at construction time.
var ErrNilRepository = errors.New("service: repository is required")

type Clock interface {
	Now() time.Time
}

type Repository interface {
	Get(ctx context.Context, id string) (string, error)
}

type Logger interface {
	Info(msg string, kv ...any)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// RealClock is the production Clock.
func RealClock() Clock { return realClock{} }

// nopLogger is the null-object for the optional Logger: its Info does nothing,
// so a service with no logger degrades gracefully instead of panicking.
type nopLogger struct{}

func (nopLogger) Info(string, ...any) {}

type Service struct {
	clock  Clock
	repo   Repository
	logger Logger
}

// NewService distinguishes required from optional dependencies. A nil repo is
// rejected (fail fast). A nil logger is replaced by a no-op null-object, and a
// nil clock by the real clock, so both degrade gracefully.
func NewService(clock Clock, repo Repository, logger Logger) (*Service, error) {
	if repo == nil {
		return nil, ErrNilRepository
	}
	if logger == nil {
		logger = nopLogger{}
	}
	if clock == nil {
		clock = RealClock()
	}
	return &Service{clock: clock, repo: repo, logger: logger}, nil
}

// Get calls the logger unconditionally: after construction s.logger is never
// nil, so no per-call guard is needed.
func (s *Service) Get(ctx context.Context, id string) (string, error) {
	s.logger.Info("getting item", "id", id, "at", s.clock.Now())
	v, err := s.repo.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return v, nil
}
```

### The runnable demo

The demo constructs a service with a nil logger and a nil clock — both allowed —
and calls `Get`, then attempts a construction with a nil repo to show the fail-fast
error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/service"
)

type memRepo struct{ data map[string]string }

func (r memRepo) Get(_ context.Context, id string) (string, error) {
	v, ok := r.data[id]
	if !ok {
		return "", service.ErrNotFound
	}
	return v, nil
}

func main() {
	// Optional dependencies omitted: nil logger and nil clock are accepted.
	repo := memRepo{data: map[string]string{"u1": "alice"}}
	s, err := service.NewService(nil, repo, nil)
	if err != nil {
		fmt.Println("unexpected:", err)
		return
	}
	v, _ := s.Get(context.Background(), "u1")
	fmt.Println("got with nil logger:", v)

	// Required dependency omitted: nil repo fails fast.
	if _, err := service.NewService(nil, nil, nil); errors.Is(err, service.ErrNilRepository) {
		fmt.Println("nil repo rejected:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
got with nil logger: alice
nil repo rejected: service: repository is required
```

### Tests

`TestServiceWithNilLoggerDoesNotPanic` is the promoted contract: it passes a nil
logger, calls `Get`, and asserts both that no panic occurred and that the result is
correct — proving the null-object substitution took effect. `TestNilRepoRejected`
asserts the inverse policy: a nil repo returns `ErrNilRepository` from the
constructor rather than a nil-and-no-error that a caller might use unknowingly.

Create `service_test.go`:

```go
package service

import (
	"context"
	"errors"
	"testing"
)

type stubRepo struct{ data map[string]string }

func (r stubRepo) Get(_ context.Context, id string) (string, error) {
	v, ok := r.data[id]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func TestServiceWithNilLoggerDoesNotPanic(t *testing.T) {
	t.Parallel()

	repo := stubRepo{data: map[string]string{"u1": "alice"}}
	s, err := NewService(RealClock(), repo, nil) // nil logger
	if err != nil {
		t.Fatalf("NewService with nil logger: unexpected error: %v", err)
	}

	// If Get dereferenced a nil logger this would panic and fail the test.
	v, err := s.Get(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if v != "alice" {
		t.Fatalf("Get = %q, want alice", v)
	}
}

func TestNilRepoRejected(t *testing.T) {
	t.Parallel()

	s, err := NewService(RealClock(), nil, nil) // nil repo: required
	if !errors.Is(err, ErrNilRepository) {
		t.Fatalf("NewService with nil repo: error = %v, want %v", err, ErrNilRepository)
	}
	if s != nil {
		t.Fatalf("NewService returned non-nil service alongside error: %v", s)
	}
}

func TestNilClockDefaultsToReal(t *testing.T) {
	t.Parallel()

	repo := stubRepo{data: map[string]string{"u1": "alice"}}
	s, err := NewService(nil, repo, nil) // nil clock defaults to RealClock
	if err != nil {
		t.Fatalf("NewService with nil clock: unexpected error: %v", err)
	}
	if _, err := s.Get(context.Background(), "u1"); err != nil {
		t.Fatalf("Get with defaulted clock: unexpected error: %v", err)
	}
}
```

## Review

The service is correct when the constructor encodes the required-versus-optional
policy so the methods do not have to. A nil repo returns `ErrNilRepository` and no
service; a nil logger becomes `nopLogger{}` so `Get` calls it without a guard; a
nil clock becomes `RealClock()`. The two failure modes to avoid are symmetric and
both real: dereferencing a nil optional dependency and panicking in production, or
silently no-oping a nil required dependency so a wiring bug ships as empty data.
`TestServiceWithNilLoggerDoesNotPanic` pins the first; `TestNilRepoRejected` pins
the second. Run `go test -race` to confirm.

## Resources

- [log/slog: DiscardHandler](https://pkg.go.dev/log/slog#DiscardHandler) — the standard library's own null-object logger, added in Go 1.24.
- [Null Object pattern (Refactoring Guru)](https://refactoring.guru/introduce-null-object) — the general shape of substituting a no-op for a nil dependency.
- [Go FAQ: nil interface values](https://go.dev/doc/faq#nil_error) — why a typed nil wrapped in an interface is not equal to nil.

---

Back to [02-fake-driven-service-tests.md](02-fake-driven-service-tests.md) | Next: [04-functional-options-constructor.md](04-functional-options-constructor.md)
