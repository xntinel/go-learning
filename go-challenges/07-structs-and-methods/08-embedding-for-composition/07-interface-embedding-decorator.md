# Exercise 7: A Repository Decorator by Embedding the Interface

The cleanest way to add cross-cutting behavior — metrics, retry, logging, caching
— to an existing implementation is a decorator that wraps it. Go builds decorators
by embedding the *interface*: you override the one method you care about and let
promotion forward all the rest. This is how instrumentation wrappers are written in
real services.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
repo/                      independent module: example.com/repo
  go.mod                   go 1.26
  repo.go                  UserRepository interface; instrumentedRepo embeds it; overrides FindByID
  cmd/
    demo/
      main.go              runnable demo: wrap a fake repo, call FindByID and Save
  repo_test.go             override records a metric, Save forwarded, nil embedded interface panics
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: a `UserRepository` interface and an `instrumentedRepo` embedding it, overriding only `FindByID` to count/log while delegating everything else; a `NewInstrumented` constructor that rejects a nil delegate.
- Test: the override records a metric and returns the delegate's result; a non-overridden method (`Save`) is forwarded transparently; a nil embedded interface makes a promoted call panic.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/repo/cmd/demo
cd ~/go-exercises/repo
go mod init example.com/repo
```

### Override one method, promote the rest

A `UserRepository` might have a dozen methods. To instrument just `FindByID` you do
*not* want to write twelve forwarding methods by hand. Embedding the interface
solves it: `instrumentedRepo` has an embedded `UserRepository` field holding the
real implementation, so every method of the interface is promoted onto
`instrumentedRepo` and forwards to the wrapped value automatically. You then
declare `FindByID` on `instrumentedRepo`, which shadows the promoted one, and in it
you record a metric, time the call, log, and delegate to `r.UserRepository.FindByID`.
`Save` and any other method you did not override are promoted, so they pass through
untouched. Adding a thirteenth method to the interface requires no change to the
decorator.

This is the same shadow-and-delegate pattern as the earlier server exercise, but
over an interface instead of a concrete type, and it is the backbone of middleware
for non-HTTP layers: repository instrumentation, retrying transports, caching
clients. The decorator satisfies `UserRepository` itself, so it drops in wherever
the bare repository was used.

The hazard is the nil embedded interface. If `instrumentedRepo` is built with a nil
`UserRepository`, every promoted call — and every delegation inside an override —
dereferences a nil interface and panics. There is no compile-time protection; the
zero value of the struct has a nil embedded interface. So the decorator needs a
constructor that rejects nil, which `NewInstrumented` does. That is the same
constructor discipline pointer embedding needs, for the same reason.

Create `repo.go`:

```go
package repo

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// ErrNotFound is returned when a user does not exist.
var ErrNotFound = errors.New("user not found")

// User is the stored entity.
type User struct {
	ID   string
	Name string
}

// UserRepository is the port the rest of the app depends on.
type UserRepository interface {
	FindByID(ctx context.Context, id string) (User, error)
	Save(ctx context.Context, u User) error
}

// instrumentedRepo decorates a UserRepository. It embeds the interface so every
// method is promoted (forwarded) to the wrapped value, and overrides only
// FindByID to add metrics and logging.
type instrumentedRepo struct {
	UserRepository
	logger    *slog.Logger
	findCalls int
}

// NewInstrumented wraps inner. It rejects a nil delegate because a nil embedded
// interface makes every promoted call panic.
func NewInstrumented(inner UserRepository, logger *slog.Logger) (*instrumentedRepo, error) {
	if inner == nil {
		return nil, errors.New("repo: inner UserRepository must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &instrumentedRepo{UserRepository: inner, logger: logger}, nil
}

// FindByID overrides the promoted method: count, time, log, then delegate.
func (r *instrumentedRepo) FindByID(ctx context.Context, id string) (User, error) {
	r.findCalls++
	start := time.Now()
	u, err := r.UserRepository.FindByID(ctx, id)
	r.logger.Info("FindByID",
		"id", id,
		"found", err == nil,
		"duration", time.Since(start),
	)
	return u, err
}

// FindCalls exposes the metric for tests and callers.
func (r *instrumentedRepo) FindCalls() int {
	return r.findCalls
}

// Save is intentionally NOT overridden: it is promoted from the embedded
// interface and forwards straight to the wrapped repository.
```

### The runnable demo

The demo defines a tiny in-memory repository, wraps it, and exercises both the
overridden `FindByID` and the promoted `Save`. The logger is discarded so the
output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"example.com/repo"
)

type memRepo struct {
	users map[string]repo.User
}

func (m *memRepo) FindByID(ctx context.Context, id string) (repo.User, error) {
	u, ok := m.users[id]
	if !ok {
		return repo.User{}, repo.ErrNotFound
	}
	return u, nil
}

func (m *memRepo) Save(ctx context.Context, u repo.User) error {
	m.users[u.ID] = u
	return nil
}

func main() {
	base := &memRepo{users: map[string]repo.User{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := repo.NewInstrumented(base, logger)
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	_ = r.Save(ctx, repo.User{ID: "u1", Name: "Alice"}) // promoted, forwarded

	u, _ := r.FindByID(ctx, "u1") // overridden, instrumented
	fmt.Println("found:", u.Name)

	_, err = r.FindByID(ctx, "missing")
	fmt.Println("missing is ErrNotFound:", errors.Is(err, repo.ErrNotFound))
	fmt.Println("find calls:", r.FindCalls())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
found: Alice
missing is ErrNotFound: true
find calls: 2
```

### Tests

`TestFindByIDInstrumented` wraps a fake, calls `FindByID`, and asserts the metric
incremented and the delegate's result (including its wrapped `ErrNotFound`, matched
with `errors.Is`) is returned unchanged. `TestSaveForwarded` calls the
non-overridden `Save` and asserts the fake actually recorded it — proving promotion
forwards it. `TestNilEmbeddedPanics` constructs an `instrumentedRepo` with a nil
embedded interface directly (bypassing the constructor) and asserts a promoted call
panics, which is why `NewInstrumented` rejects nil.

Create `repo_test.go`:

```go
package repo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

type fakeRepo struct {
	users     map[string]User
	saveCalls int
}

func newFake() *fakeRepo {
	return &fakeRepo{users: map[string]User{}}
}

func (f *fakeRepo) FindByID(ctx context.Context, id string) (User, error) {
	u, ok := f.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

func (f *fakeRepo) Save(ctx context.Context, u User) error {
	f.saveCalls++
	f.users[u.ID] = u
	return nil
}

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestFindByIDInstrumented(t *testing.T) {
	t.Parallel()
	fake := newFake()
	fake.users["u1"] = User{ID: "u1", Name: "Alice"}
	r, err := NewInstrumented(fake, discard())
	if err != nil {
		t.Fatal(err)
	}

	got, err := r.FindByID(context.Background(), "u1")
	if err != nil {
		t.Fatalf("FindByID = %v", err)
	}
	if got.Name != "Alice" {
		t.Errorf("Name = %q, want Alice", got.Name)
	}
	if r.FindCalls() != 1 {
		t.Errorf("FindCalls = %d, want 1", r.FindCalls())
	}

	_, err = r.FindByID(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing lookup err = %v, want ErrNotFound", err)
	}
	if r.FindCalls() != 2 {
		t.Errorf("FindCalls = %d, want 2", r.FindCalls())
	}
}

func TestSaveForwarded(t *testing.T) {
	t.Parallel()
	fake := newFake()
	r, err := NewInstrumented(fake, discard())
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Save(context.Background(), User{ID: "u2", Name: "Bob"}); err != nil {
		t.Fatal(err)
	}
	if fake.saveCalls != 1 {
		t.Fatalf("promoted Save did not reach the fake: saveCalls = %d", fake.saveCalls)
	}
}

func TestNilEmbeddedPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("promoted call on a nil embedded interface should panic")
		}
	}()
	var bad instrumentedRepo // embedded UserRepository is nil
	_ = bad.Save(context.Background(), User{ID: "x"})
}

func TestNewInstrumentedRejectsNil(t *testing.T) {
	t.Parallel()
	if _, err := NewInstrumented(nil, discard()); err == nil {
		t.Fatal("NewInstrumented(nil, ...) should return an error")
	}
}
```

## Review

The decorator is correct when the overridden method adds behavior and returns the
delegate's exact result, and every other method forwards untouched.
`TestFindByIDInstrumented` proves the metric and the pass-through result (including
the wrapped sentinel matched by `errors.Is`); `TestSaveForwarded` proves promotion
carries `Save` to the wrapped fake with no code written for it. The nil-interface
hazard is real and silent until it panics in production, which is why
`NewInstrumented` rejects nil and `TestNilEmbeddedPanics`/`TestNewInstrumentedRejectsNil`
pin both the failure and the guard. This is the pattern to reach for whenever you
need to instrument an existing implementation without reimplementing its whole
surface.

## Resources

- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — embedding an interface value and promoting its method set.
- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — interface embedding and forwarding by promotion.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching the delegate's wrapped sentinel through the decorator.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-method-shadowing-base-validator.md](08-method-shadowing-base-validator.md)
