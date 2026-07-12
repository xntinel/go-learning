# Exercise 3: Fake vs Mock — An In-Memory UserRepository as a Working Test Double

Some contracts are round-trips: create a user, then read it back and expect the
same fields; look up a missing id and expect a not-found error. A call-recording
spy cannot honestly model that — it has no state to read back. A *fake*, a real
but lightweight implementation, can. This module builds a `UserService` over a
`UserRepository` port and tests it against a map-backed in-memory fake.

Fully self-contained: its own module, package, demo, and test.

## What you'll build

```text
fakerepo/                    independent module: example.com/fakerepo
  go.mod                     go 1.26
  user.go                    User; UserRepository port; ErrNotFound; UserService
  cmd/
    demo/
      main.go                runnable demo: register then fetch, and a miss
  user_test.go              in-memory fake repo; round-trip and miss tests
```

- Files: `user.go`, `cmd/demo/main.go`, `user_test.go`.
- Implement: a `UserService` with `Register(ctx, name) (User, error)` and `Get(ctx, id) (User, error)`, depending on a `UserRepository` with `FindByID` and `Save`.
- Test: an in-memory fake repo (map + `sync.RWMutex`, returns `ErrNotFound`); drive create-then-read and read-missing; assert the returned `User` and that a miss surfaces via `errors.Is(err, ErrNotFound)`.
- Verify: `go test -count=1 -race ./...`

### Why a fake, not a mock, for a repository

A repository has *stateful* behavior: what you `Save` is what a later `FindByID`
returns; an id you never saved is not found. `UserService.Register` saves a new
user and `UserService.Get` reads one back, so a faithful test must exercise the
read-after-write round-trip end to end. If you doubled the repository with a mock
programmed to "return this user when `FindByID(7)` is called", you would be
hand-feeding the very answer the test is supposed to verify the service round-trips
correctly — the test would assert your stub configuration, not the service's
behavior. Worse, a mock cannot catch a service bug like "Register saves under the
wrong key" because the mock's `FindByID` answer is decoupled from what `Save`
received.

A *fake* closes that loop. The in-memory repository stores into a real map on
`Save` and reads from it on `FindByID`, returning `ErrNotFound` for an absent key —
genuine working behavior in a few dozen lines. Drive `Register` then `Get` through
the service and the fake carries the state between them, so the test verifies the
actual round-trip: the id the service generated, the record it persisted, and the
fields it read back. This is state-based verification through a working
implementation, and it is why "fake vs mock for a repository" is a real design
call seniors make in review.

### The port and the error sentinel

`UserRepository` is defined at the consumer with exactly two methods. `FindByID`
returns `ErrNotFound` (a package-level sentinel) when the id is absent, and the
service wraps it with `%w` so callers can classify the miss with
`errors.Is(err, ErrNotFound)` without string-matching. The fake and any real
implementation (Postgres, Redis) must honor the same sentinel — that shared error
contract is precisely the thing a mock-only suite tends to drift away from, and a
reason to keep at least one test against a working implementation.

The fake guards its map with a `sync.RWMutex` so concurrent reads do not block
each other while writes stay exclusive; it is correct under `-race`. `t.Cleanup`
is used to make the per-test fake's teardown explicit even though a fresh fake per
test already isolates state — the point is the habit: never share a double across
tests.

Create `user.go`:

```go
package fakerepo

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotFound is returned by a UserRepository when no user has the given id.
var ErrNotFound = errors.New("user not found")

// User is the domain record.
type User struct {
	ID   string
	Name string
}

// UserRepository is the persistence port, defined here at the consumer.
type UserRepository interface {
	FindByID(ctx context.Context, id string) (User, error)
	Save(ctx context.Context, u User) error
}

// IDGen produces unique user ids. Injected so tests are deterministic.
type IDGen func() string

// UserService is the SUT: it registers and fetches users through the repository.
type UserService struct {
	repo  UserRepository
	newID IDGen
}

func NewUserService(repo UserRepository, newID IDGen) *UserService {
	return &UserService{repo: repo, newID: newID}
}

// Register creates a user with a generated id and persists it.
func (s *UserService) Register(ctx context.Context, name string) (User, error) {
	if name == "" {
		return User{}, errors.New("name must not be empty")
	}
	u := User{ID: s.newID(), Name: name}
	if err := s.repo.Save(ctx, u); err != nil {
		return User{}, fmt.Errorf("register %q: %w", name, err)
	}
	return u, nil
}

// Get fetches a user by id, wrapping a miss so callers can classify it.
func (s *UserService) Get(ctx context.Context, id string) (User, error) {
	u, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return User{}, fmt.Errorf("get %q: %w", id, err)
	}
	return u, nil
}
```

### The runnable demo

The demo wires the service to the in-memory fake (defined in the demo's own
`main` package so it stays self-contained), registers a user, fetches it back, and
then asks for an id that was never saved to show the not-found path.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"example.com/fakerepo"
)

// memRepo is a minimal in-memory UserRepository for the demo.
type memRepo struct {
	mu    sync.RWMutex
	users map[string]fakerepo.User
}

func newMemRepo() *memRepo { return &memRepo{users: map[string]fakerepo.User{}} }

func (r *memRepo) FindByID(_ context.Context, id string) (fakerepo.User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.users[id]
	if !ok {
		return fakerepo.User{}, fakerepo.ErrNotFound
	}
	return u, nil
}

func (r *memRepo) Save(_ context.Context, u fakerepo.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[u.ID] = u
	return nil
}

func main() {
	ctx := context.Background()
	seq := 0
	svc := fakerepo.NewUserService(newMemRepo(), func() string {
		seq++
		return fmt.Sprintf("u%d", seq)
	})

	u, _ := svc.Register(ctx, "alice")
	fmt.Printf("registered: %s -> %s\n", u.ID, u.Name)

	got, _ := svc.Get(ctx, u.ID)
	fmt.Printf("fetched: %s -> %s\n", got.ID, got.Name)

	_, err := svc.Get(ctx, "missing")
	fmt.Printf("miss is ErrNotFound: %v\n", errors.Is(err, fakerepo.ErrNotFound))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
registered: u1 -> alice
fetched: u1 -> alice
miss is ErrNotFound: true
```

### Tests

The test defines a `fakeRepo` in the test package and drives the service through
it. `TestRegisterThenGetRoundTrips` proves a registered user is readable with the
same fields — the round-trip a mock cannot honestly model. `TestGetMissingReturns
NotFound` proves an unsaved id surfaces `ErrNotFound` through the service's
wrapping via `errors.Is`. `TestRegisterRejectsEmptyName` covers the validation
branch. The `Example` shows the round-trip's output.

Create `user_test.go`:

```go
package fakerepo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// fakeRepo is a working in-memory UserRepository: a real test double with state.
type fakeRepo struct {
	mu    sync.RWMutex
	users map[string]User
}

func newFakeRepo() *fakeRepo { return &fakeRepo{users: map[string]User{}} }

func (r *fakeRepo) FindByID(_ context.Context, id string) (User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

func (r *fakeRepo) Save(_ context.Context, u User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[u.ID] = u
	return nil
}

// fixedID returns a deterministic id generator for tests.
func fixedID(id string) IDGen { return func() string { return id } }

func TestRegisterThenGetRoundTrips(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	t.Cleanup(func() { repo.users = nil })
	svc := NewUserService(repo, fixedID("u1"))
	ctx := context.Background()

	created, err := svc.Register(ctx, "alice")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if created.ID != "u1" || created.Name != "alice" {
		t.Fatalf("Register returned %+v, want {u1 alice}", created)
	}

	got, err := svc.Get(ctx, "u1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != created {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, created)
	}
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	t.Parallel()

	svc := NewUserService(newFakeRepo(), fixedID("u1"))

	_, err := svc.Get(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}
}

func TestRegisterRejectsEmptyName(t *testing.T) {
	t.Parallel()

	svc := NewUserService(newFakeRepo(), fixedID("u1"))

	if _, err := svc.Register(context.Background(), ""); err == nil {
		t.Fatal("Register(\"\") returned nil error, want validation failure")
	}
}

func Example() {
	svc := NewUserService(newFakeRepo(), fixedID("u1"))
	ctx := context.Background()
	u, _ := svc.Register(ctx, "alice")
	got, _ := svc.Get(ctx, u.ID)
	fmt.Printf("%s %s\n", got.ID, got.Name)
	// Output: u1 alice
}
```

## Review

The fake earns its place by carrying state between the two service calls: because
`Save` really stores and `FindByID` really reads, `TestRegisterThenGetRoundTrips`
verifies the service round-trips the record — the id it minted, the name it
persisted, the fields it returned — rather than verifying a stub you pre-loaded
with the answer. That is the core "fake vs mock" lesson: when the contract is a
read-after-write, a working double tells the truth a canned answer cannot.

Two things keep it honest. Keep the `ErrNotFound` sentinel shared between the fake
and any real repository, and wrap it with `%w` so `errors.Is` classifies the miss
without string comparison — a mock-only suite tends to drift from that shared error
semantics, which is why a fake (or an integration test) is worth keeping. And make
a fresh fake per test: the `RWMutex` keeps it race-free, but isolation comes from
not sharing the double, which `t.Cleanup` here makes explicit.

## Resources

- [`errors.Is`](https://pkg.go.dev/errors#Is) — classifying a wrapped sentinel error like `ErrNotFound`.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — read/write locking for the in-memory fake.
- [Martin Fowler: Mocks Aren't Stubs](https://martinfowler.com/articles/mocksArentStubs.html) — when a fake beats a mock for stateful ports.

---

Back to [02-stub-error-injection-and-concurrency.md](02-stub-error-injection-and-concurrency.md) | Next: [04-mock-http-client-roundtripper.md](04-mock-http-client-roundtripper.md)
