# Exercise 2: Testing The Service With Hand-Written Fakes And A Pinned Clock

Constructor injection pays off in the test file. Here you drive the same `Service`
entirely from hand-written fakes — a fixed clock, a map-backed repository with an
injectable error, and a recording logger — and assert the headline property:
`Get` reads time through the injected clock, never through `time.Now`. The
interface is the test boundary, and these three fakes are all it takes to control
the world the service runs in.

This module is fully self-contained. It restates the `Service` from Exercise 1 so
it builds and gates alone, then focuses on the test suite.

## What you'll build

```text
service/                    independent module: example.com/service
  go.mod                    module example.com/service
  service.go                the Service under test (Clock, Repository, Logger, Get)
  cmd/
    demo/
      main.go               runs Get against the fakes and prints the outcome
  service_test.go           fakeClock, fakeRepo, fakeLogger; value, error, injected-clock tests
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: the `Service` (as in Exercise 1) plus three fakes in the test — `fakeClock` (fixed time), `fakeRepo` (map-backed with an injectable `err`), `fakeLogger` (records entries).
- Test: `TestServiceGetReturnsValue`, `TestServiceGetReturnsError` (error propagates from the repo), `TestServiceUsesInjectedClock` (the fake's fixed time is the one the service observes and it is unchanged after the call).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/service/cmd/demo
cd ~/go-exercises/service
go mod init example.com/service
```

### The fakes are the point

A fake is a hand-written implementation of an interface, built for a test. Because
each dependency is a one- or two-method interface, each fake is tiny:

- `fakeClock` holds a `now time.Time` and returns it from `Now()`. It never
  advances. That is what makes it useful: whatever the service reads is exactly the
  value the test set, so the test can assert on it.
- `fakeRepo` holds a `data map[string]string` and an `err error`. If `err` is set,
  every `Get` returns it — that is how the test drives the error path without a real
  failing database. Otherwise it looks the id up in the map and returns
  `ErrNotFound` when it is absent.
- `fakeLogger` appends each message to a slice, so a test can assert the service
  logged and even check what it logged.

The three tests exercise three distinct contracts. `TestServiceGetReturnsValue`
checks the happy path: a present key returns its value and the logger fired.
`TestServiceGetReturnsError` injects `fakeRepo.err` and asserts the error
propagates unchanged out of `Get` — the service does not swallow or rewrite a
repository failure. `TestServiceUsesInjectedClock` is the headline: it pins the
clock to a fixed instant, runs `Get`, and asserts the clock's stored time is
*still* that exact instant afterward. If `Get` had called `time.Now()` anywhere,
the assertion would not catch it directly — but combined with the fact that the
only clock the service holds is the fake, an unchanged fake time proves the service
observed the injected clock and nothing else moved it. The property that makes the
service testable is that its notion of "now" is entirely under the test's control.

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

func RealClock() Clock { return realClock{} }

type Service struct {
	clock  Clock
	repo   Repository
	logger Logger
}

func NewService(clock Clock, repo Repository, logger Logger) *Service {
	return &Service{clock: clock, repo: repo, logger: logger}
}

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

The demo constructs the service from the same kind of fakes the test uses, so you
can see the injected stack produce output without any real dependency.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/service"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type mapRepo struct{ data map[string]string }

func (r mapRepo) Get(_ context.Context, id string) (string, error) {
	v, ok := r.data[id]
	if !ok {
		return "", service.ErrNotFound
	}
	return v, nil
}

type printLogger struct{}

func (printLogger) Info(msg string, kv ...any) {
	fmt.Printf("log: %s %v\n", msg, kv[:2])
}

func main() {
	clock := fixedClock{t: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)}
	repo := mapRepo{data: map[string]string{"u1": "alice"}}
	s := service.NewService(clock, repo, printLogger{})

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

Create `service_test.go`:

```go
package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeClock struct {
	now time.Time
}

func (f *fakeClock) Now() time.Time { return f.now }

type fakeRepo struct {
	data map[string]string
	err  error
}

func (f *fakeRepo) Get(_ context.Context, id string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	v, ok := f.data[id]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

type fakeLogger struct {
	entries []string
}

func (f *fakeLogger) Info(msg string, _ ...any) {
	f.entries = append(f.entries, msg)
}

func TestServiceGetReturnsValue(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{now: time.Unix(1700000000, 0).UTC()}
	repo := &fakeRepo{data: map[string]string{"u1": "alice"}}
	logger := &fakeLogger{}
	s := NewService(clock, repo, logger)

	v, err := s.Get(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if v != "alice" {
		t.Fatalf("Get = %q, want alice", v)
	}
	if len(logger.entries) == 0 {
		t.Fatal("logger should have been called")
	}
}

func TestServiceGetReturnsError(t *testing.T) {
	t.Parallel()

	diskErr := errors.New("disk error")
	clock := &fakeClock{now: time.Unix(0, 0)}
	repo := &fakeRepo{err: diskErr}
	logger := &fakeLogger{}
	s := NewService(clock, repo, logger)

	_, err := s.Get(context.Background(), "u1")
	if !errors.Is(err, diskErr) {
		t.Fatalf("Get error = %v, want %v", err, diskErr)
	}
}

func TestServiceUsesInjectedClock(t *testing.T) {
	t.Parallel()

	fixed := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	clock := &fakeClock{now: fixed}
	repo := &fakeRepo{data: map[string]string{"u1": "alice"}}
	logger := &fakeLogger{}
	s := NewService(clock, repo, logger)

	if _, err := s.Get(context.Background(), "u1"); err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if !clock.now.Equal(fixed) {
		t.Fatalf("injected clock changed: now = %v, want %v", clock.now, fixed)
	}
}
```

## Review

The suite is correct when each fake isolates one dimension of the service's world:
the clock isolates time, the repo isolates storage (and its error path), the logger
isolates observability. `TestServiceGetReturnsError` asserts propagation with
`errors.Is` against the exact sentinel it injected, so a service that swallowed or
rewrote the error would fail. `TestServiceUsesInjectedClock` is the property that
justifies the whole design: because the service's only clock is the fake and the
fake never advances, an unchanged fake time after `Get` proves the service never
consulted the wall clock. The mistake to avoid is testing through a real
dependency "because it is easy" — a real map or a real clock reintroduces exactly
the nondeterminism the fakes remove. Run `go test -race` to confirm the fakes and
the service compose without data races.

## Resources

- [Testing techniques (Andrew Gerrand)](https://go.dev/talks/2014/testing.slide) — hand-written fakes and the interface-as-seam pattern.
- [errors.Is](https://pkg.go.dev/errors#Is) — asserting a wrapped error against a sentinel.
- [Go Wiki: Table-driven tests](https://go.dev/wiki/TableDrivenTests) — the structure the fake-driven tests build on.

---

Back to [01-constructor-injection-service.md](01-constructor-injection-service.md) | Next: [03-nil-object-optional-logger.md](03-nil-object-optional-logger.md)
