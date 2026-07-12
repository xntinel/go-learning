# Exercise 5: Interface Segregation — Depend On The Narrow Method Set You Actually Call

An interface should name the one or two methods a consumer actually uses, not a
provider's entire surface. Here a `UserService` needs only `UserByID` from a
`*sqlStore` that has a dozen methods. You define the narrow `userGetter` interface
in the consumer, let the fat concrete store satisfy it implicitly, and write a
one-method fake instead of a twelve-method mock.

This module is fully self-contained, with its own `go mod init`, code, demo, and
tests.

## What you'll build

```text
narrowiface/                independent module: example.com/narrowiface
  go.mod                    module example.com/narrowiface
  store.go                  the fat *sqlStore: UserByID plus many other methods
  service.go                UserService depending on a one-method userGetter interface
  cmd/
    demo/
      main.go               wires the real store into the service and greets a user
  service_test.go           a one-method fake drives the service; compile-time satisfaction proof
```

- Files: `store.go`, `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: a fat `*sqlStore` with `UserByID` and several unrelated methods; a `userGetter` interface (single method `UserByID(ctx, id) (User, error)`) defined in the consumer; a `UserService` that takes a `userGetter` and has a `Greeting(ctx, id)` method.
- Test: a `fakeGetter` implementing only `UserByID` drives the service; `var _ userGetter = (*sqlStore)(nil)` proves the real store still satisfies the narrowed interface.
- Verify: `go test -count=1 -race ./...`

### Who owns the interface

In many languages the interface is declared next to the implementation: the
data-access layer exports a `Store` interface and its concrete type. Go inverts
this. Because satisfaction is structural — a type satisfies an interface merely by
having the right methods, with no `implements` keyword — the *consumer* can declare
the interface it needs, naming only the methods it calls, and any provider with
those methods satisfies it automatically. The provider need not import the consumer
or even know the interface exists.

That is interface segregation with teeth. `*sqlStore` here is deliberately fat: it
has `UserByID`, but also `CreateUser`, `DeleteUser`, `ListUsers`, `UpdateEmail`,
`Ping`, and `Close`. A `UserService` that only reads a user by id has no business
depending on all of that. So `service.go` declares `userGetter` — one method,
`UserByID` — and `UserService` depends on that. The consequences are concrete.
First, the dependency is honest: the signature says exactly what the service
touches. Second, the fake is trivial: a test implements one method, not twelve, so
there is no ceremony and no methods stubbed out with `panic("unimplemented")`.
Third, the service is decoupled from the provider package: swap `*sqlStore` for a
cache, a gRPC client, or an in-memory map, and as long as it has `UserByID`, it
fits.

The compile-time assertion `var _ userGetter = (*sqlStore)(nil)` is documentation
that also fails the build if it ever stops being true: it states that the real
store satisfies the narrowed interface. It lives in the test file (or could live in
the composition root) rather than in the store's own package, precisely because the
store should not need to know about `userGetter`.

Create `store.go`:

```go
package narrowiface

import (
	"context"
	"errors"
)

// ErrNoUser is returned when a user id is not found.
var ErrNoUser = errors.New("user not found")

// User is the domain entity.
type User struct {
	ID   string
	Name string
}

// sqlStore is a deliberately fat data-access type: it has many methods, only
// one of which (UserByID) the UserService needs.
type sqlStore struct {
	users map[string]User
}

// NewSQLStore builds a store seeded with a couple of users (standing in for a
// real database).
func NewSQLStore() *sqlStore {
	return &sqlStore{users: map[string]User{
		"u1": {ID: "u1", Name: "Alice"},
		"u2": {ID: "u2", Name: "Bob"},
	}}
}

func (s *sqlStore) UserByID(_ context.Context, id string) (User, error) {
	u, ok := s.users[id]
	if !ok {
		return User{}, ErrNoUser
	}
	return u, nil
}

// The remaining methods make sqlStore "fat" — none is needed by UserService,
// which is exactly the point of depending on a narrow interface.
func (s *sqlStore) CreateUser(_ context.Context, u User) error {
	s.users[u.ID] = u
	return nil
}

func (s *sqlStore) DeleteUser(_ context.Context, id string) error {
	delete(s.users, id)
	return nil
}

func (s *sqlStore) ListUsers(_ context.Context) ([]User, error) {
	out := make([]User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	return out, nil
}

func (s *sqlStore) UpdateEmail(_ context.Context, id, email string) error {
	_ = email
	if _, ok := s.users[id]; !ok {
		return ErrNoUser
	}
	return nil
}

func (s *sqlStore) Ping(_ context.Context) error { return nil }

func (s *sqlStore) Close() error { return nil }
```

Create `service.go`:

```go
package narrowiface

import (
	"context"
	"fmt"
)

// userGetter is the narrow, consumer-defined interface: it names only the one
// method UserService calls. The fat *sqlStore satisfies it implicitly.
type userGetter interface {
	UserByID(ctx context.Context, id string) (User, error)
}

// UserService depends on the narrow userGetter, not the fat store.
type UserService struct {
	users userGetter
}

// NewUserService accepts any userGetter (the real store, a cache, a fake).
func NewUserService(users userGetter) *UserService {
	return &UserService{users: users}
}

// Greeting reads a user through the narrow seam and formats a greeting.
func (s *UserService) Greeting(ctx context.Context, id string) (string, error) {
	u, err := s.users.UserByID(ctx, id)
	if err != nil {
		return "", fmt.Errorf("greeting %s: %w", id, err)
	}
	return "Hello, " + u.Name, nil
}
```

### The runnable demo

The demo wires the real fat store into the service through the narrow interface and
prints a greeting — the service never sees the eleven methods it does not use.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/narrowiface"
)

func main() {
	store := narrowiface.NewSQLStore()
	svc := narrowiface.NewUserService(store) // *sqlStore satisfies userGetter

	greeting, err := svc.Greeting(context.Background(), "u1")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(greeting)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Hello, Alice
```

### Tests

The fake implements exactly one method. That is the payoff of the narrow interface:
no need to stub `CreateUser`, `ListUsers`, `Ping`, or `Close`. The compile-time
`var _ userGetter = (*sqlStore)(nil)` assertion proves the real store still
satisfies the narrowed interface, so the demo's wiring is type-checked here too.

Create `service_test.go`:

```go
package narrowiface

import (
	"context"
	"errors"
	"testing"
)

// fakeGetter implements ONLY the one method userGetter requires.
type fakeGetter struct {
	user User
	err  error
}

func (f fakeGetter) UserByID(_ context.Context, _ string) (User, error) {
	return f.user, f.err
}

// Compile-time proof that the real fat store satisfies the narrow interface.
var _ userGetter = (*sqlStore)(nil)

// And the one-method fake does too.
var _ userGetter = fakeGetter{}

func TestGreetingReturnsGreeting(t *testing.T) {
	t.Parallel()

	svc := NewUserService(fakeGetter{user: User{ID: "u1", Name: "Alice"}})
	got, err := svc.Greeting(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Greeting: unexpected error: %v", err)
	}
	if got != "Hello, Alice" {
		t.Fatalf("Greeting = %q, want %q", got, "Hello, Alice")
	}
}

func TestGreetingWrapsError(t *testing.T) {
	t.Parallel()

	svc := NewUserService(fakeGetter{err: ErrNoUser})
	_, err := svc.Greeting(context.Background(), "missing")
	if !errors.Is(err, ErrNoUser) {
		t.Fatalf("Greeting error = %v, want wrapped %v", err, ErrNoUser)
	}
}
```

## Review

The design is correct when the interface is as narrow as the consumer's actual
usage: `UserService` calls only `UserByID`, so `userGetter` has only that method,
and the fat `*sqlStore` satisfies it for free. The two failure modes to avoid: a
provider-side fat interface that forces every fake to implement methods it never
calls, and returning the concrete `*sqlStore` from the constructor instead of
accepting the interface, which welds the service to that one store. The
compile-time assertions catch a regression in either direction. Run `go test -race`
to confirm the narrow seam holds.

## Resources

- [Go Proverbs: "The bigger the interface, the weaker the abstraction"](https://go-proverbs.github.io/) — the one-line case for narrow interfaces.
- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — define interfaces in the consuming package, not the implementing one.
- [Effective Go: Interfaces and methods](https://go.dev/doc/effective_go#interfaces_and_types) — structural satisfaction in practice.

---

Back to [04-functional-options-constructor.md](04-functional-options-constructor.md) | Next: [06-composition-root-wiring-main.md](06-composition-root-wiring-main.md)
