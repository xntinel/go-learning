# Exercise 1: A Request Service That Receives Its Clock, Repository, And Logger

The base pattern of the whole lesson: a service that receives every collaborator
it needs and constructs none of them. You build a `Service` holding three
interfaces — a `Clock`, a `Repository`, and a `Logger` — and a `NewService`
constructor that does nothing but store what it is handed. Every later exercise is
a variation on this seam.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
service/                    independent module: example.com/service
  go.mod                    module example.com/service
  service.go                Clock, Repository, Logger interfaces; RealClock; Service; NewService; Get
  cmd/
    demo/
      main.go               wires RealClock + an in-memory repo + a stdout logger and calls Get
  service_test.go           smoke test wiring the real clock and a trivial repo
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: `Clock` (`Now() time.Time`), `Repository` (`Get(ctx, id)`), `Logger` (`Info(msg, kv...)`); a `RealClock()` production helper wrapping `time.Now`; a `Service` struct holding the three interfaces; `NewService(clock, repo, logger)` that stores them and constructs nothing; `Get(ctx, id)` that logs via the injected logger and reads via the injected repo.
- Test: a compile-time assertion that the constructor takes interfaces, plus a smoke test wiring `RealClock()` and a trivial in-memory repo through `NewService` and calling `Get` for the happy path.
- Verify: `go test -count=1 -race ./...`

### Why the constructor constructs nothing

The single most important line in this file is the one that is not there:
`NewService` never calls `NewMemoryRepository()`, never captures `time.Now`, never
builds a logger. It receives three interface values and stores them. That is the
whole discipline. Because the constructor is a pure assembler, a test builds the
service in one line with three fakes and the service behaves identically to
production — there is nothing ambient left to intercept.

`Get` demonstrates why each dependency is an interface rather than a concrete type.
It reads the current time through `s.clock.Now()` (not `time.Now()`), so a test can
pin the instant; it reads the value through `s.repo.Get` (not a hardcoded map or a
real database), so a test can inject a canned result or a canned error; and it
records the access through `s.logger.Info` (not a package-level `log.Printf`), so a
test can assert that the access was logged. Three interfaces, three seams.

`RealClock()` is the production adapter for the `Clock` seam. It returns a
`realClock{}` whose `Now` simply calls `time.Now()`. This is the only place in the
package that touches the wall clock, and it is a leaf adapter, not business logic —
exactly where a concrete implementation belongs. Note it returns the `Clock`
interface here for convenience at the call site, while `NewService` itself returns
the concrete `*Service`: accept interfaces, return structs.

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

// Clock reports the current time. Injecting it lets tests pin the instant
// instead of reading the wall clock.
type Clock interface {
	Now() time.Time
}

// Repository reads a stored value by id.
type Repository interface {
	Get(ctx context.Context, id string) (string, error)
}

// Logger records structured events. Info takes a message and alternating
// key/value pairs, mirroring the shape of slog.
type Logger interface {
	Info(msg string, kv ...any)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// RealClock is the production Clock: it reads the wall clock. It is the only
// place in this package that calls time.Now.
func RealClock() Clock { return realClock{} }

// Service serves read requests. It owns no concrete dependency; every
// collaborator arrives through the constructor as an interface.
type Service struct {
	clock  Clock
	repo   Repository
	logger Logger
}

// NewService stores the injected collaborators and constructs none of them.
// This is constructor injection: the caller (the composition root) decides
// which concrete implementations to pass.
func NewService(clock Clock, repo Repository, logger Logger) *Service {
	return &Service{clock: clock, repo: repo, logger: logger}
}

// Get logs the access through the injected logger and reads the value through
// the injected repository. It reads the time through the injected clock, never
// time.Now directly.
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

The demo is a miniature composition root: it constructs the three concrete
implementations (the real clock, a map-backed repository, a logger that writes to
stdout) and injects them into `NewService`. Because `cmd/demo` is a separate
`package main`, it can only see the exported API, which is exactly the surface a
real caller would use.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/service"
)

// memRepo is a trivial in-memory Repository for the demo.
type memRepo struct{ data map[string]string }

func (r memRepo) Get(_ context.Context, id string) (string, error) {
	v, ok := r.data[id]
	if !ok {
		return "", service.ErrNotFound
	}
	return v, nil
}

// stdoutLogger prints each event so you can watch the injected logger fire.
type stdoutLogger struct{}

func (stdoutLogger) Info(msg string, kv ...any) {
	fmt.Printf("log: %s %v\n", msg, kv[:2])
}

func main() {
	repo := memRepo{data: map[string]string{"u1": "alice"}}
	s := service.NewService(service.RealClock(), repo, stdoutLogger{})

	v, err := s.Get(context.Background(), "u1")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("got:", v)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
log: getting item [id u1]
got: alice
```

### Tests

The test file proves two things. First, a compile-time assertion (`var _ Clock =
...`, and the fact that `NewService` accepts our fakes at all) pins that the
constructor's parameters are interfaces, not concrete types — if someone changed a
parameter to a concrete `*sqlRepo`, this file would stop compiling. Second, a smoke
test wires `RealClock()` and a trivial in-memory repo through `NewService` and
calls `Get`, asserting the happy path assembles and returns the stored value. It
also asserts the logger was invoked, confirming `Get` logs through the injected
seam.

Create `service_test.go`:

```go
package service

import (
	"context"
	"testing"
	"time"
)

// smokeRepo is a minimal in-memory Repository used by the smoke test.
type smokeRepo struct{ data map[string]string }

func (r smokeRepo) Get(_ context.Context, id string) (string, error) {
	v, ok := r.data[id]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

// recordingLogger records that Info was called.
type recordingLogger struct{ calls int }

func (l *recordingLogger) Info(string, ...any) { l.calls++ }

// Compile-time proof the constructor takes interfaces: these concrete types
// are assignable to the interface parameters only because they satisfy them.
var (
	_ Clock      = RealClock()
	_ Repository = smokeRepo{}
	_ Logger     = (*recordingLogger)(nil)
)

func TestServiceHappyPathAssembles(t *testing.T) {
	t.Parallel()

	repo := smokeRepo{data: map[string]string{"u1": "alice"}}
	logger := &recordingLogger{}
	s := NewService(RealClock(), repo, logger)

	v, err := s.Get(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if v != "alice" {
		t.Fatalf("Get = %q, want alice", v)
	}
	if logger.calls != 1 {
		t.Fatalf("logger.calls = %d, want 1", logger.calls)
	}
}

func TestRealClockIsRecent(t *testing.T) {
	t.Parallel()

	before := time.Now()
	got := RealClock().Now()
	if got.Before(before) {
		t.Fatalf("RealClock().Now() = %v, before %v", got, before)
	}
}
```

## Review

The service is correct when it owns no concrete dependency: `NewService` stores
what it is handed and builds nothing, and `Get` routes every ambient concern —
time, storage, logging — through an injected interface. The compile-time
assertions in the test are the structural proof that the seams are interfaces; the
smoke test is the behavioral proof that the happy path assembles from real and
fake parts alike. The mistake to avoid is the convenience default: had `NewService`
called `NewMemoryRepository()` internally, the smoke test could still pass, but the
next exercise's fake-driven tests would be impossible. Keep the constructor a pure
assembler and the seams stay open. Run `go test -race` to confirm nothing here
races, then move on to driving the same service entirely from fakes.

## Resources

- [Go Specification: Interface types](https://go.dev/ref/spec#Interface_types) — how a concrete type satisfies an interface implicitly.
- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces) — the idiom of small interfaces at boundaries.
- [log/slog](https://pkg.go.dev/log/slog) — the standard structured logger whose `Info(msg, args...)` shape the `Logger` interface mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-fake-driven-service-tests.md](02-fake-driven-service-tests.md)
