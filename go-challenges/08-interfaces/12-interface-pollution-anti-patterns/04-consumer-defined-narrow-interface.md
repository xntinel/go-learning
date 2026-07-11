# Exercise 4: Move the Interface to the Consumer

The Go Code Review Comments rule that prevents most interface pollution: the
package that USES a value declares the interface with only the methods it calls,
and the package that IMPLEMENTS the value returns a concrete type. This module
builds a store that returns concrete `*Store` and a separate service package that
declares its own one-method `userGetter` interface — so the service is tested with
a ten-line hand-written fake and no producer-side interface exists at all.

## What you'll build

```text
userlookup/                 independent module: example.com/userlookup
  go.mod                    go 1.26
  store/
    store.go                package store: User, *Store, NewStore, GetUser; ErrNotFound
  service.go                package userlookup: userGetter (consumer interface), Service
  cmd/
    demo/
      main.go               wires the real *Store into the Service
  service_test.go           fakeUserGetter satisfies userGetter; asserts error mapping
```

- Files: `store/store.go`, `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: `store.Store` returns a concrete `*Store` from `NewStore` and never an interface; `userlookup.Service` declares its own `userGetter interface { GetUser(ctx, id) (store.User, error) }` holding only the one method it calls, and is constructed with that narrow interface.
- Test: a hand-written `fakeUserGetter` implementing `userGetter`, injected into `Service`; assert the service maps `store.ErrNotFound` to its own domain error via `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/userlookup/store ~/go-exercises/userlookup/cmd/demo
cd ~/go-exercises/userlookup
go mod init example.com/userlookup
```

### Why the interface lives in the consumer

The store package has one job: hold users and return them. It exposes a concrete
`*Store` and a `GetUser` method, and it defines NO interface. This is deliberate.
Because Go satisfies interfaces implicitly, the store does not need to know that
anyone will treat it abstractly — it just returns a concrete type, which means it
can grow new methods later without breaking a single caller. There is no shared
interface enumerating its method set to keep in sync.

The consumer — the `Service` that looks up a user's display name — declares the
interface. It calls exactly one method on the store, `GetUser`, so it declares
exactly a one-method interface, `userGetter`, and takes that in its constructor.
This is the payoff of the rule. The interface is minimal because it lists what
the consumer uses and nothing else; if the store later grows a `DeleteUser`
method, `userGetter` does not, because the service does not call it. The service
is coupled to one method, not to the store's whole surface.

The interface is also unexported (`userGetter`, lowercase). That is fine and even
preferable: it is an implementation detail of how the service names its
dependency, not part of its public API. External callers still construct the
service by passing a concrete `*store.Store`, which is assignable to the
unexported interface parameter because it structurally satisfies it — you do not
need to name the interface to pass something that implements it.

The test exploits all of this. It writes a `fakeUserGetter` — a ten-line struct
with one method — injects it, and drives the service's error-mapping logic with
no mock framework and no producer-side interface. The seam exists exactly where
the test needs it: at the one method the service calls.

Create `store/store.go`:

```go
package store

import (
	"context"
	"errors"
)

// ErrNotFound is the sentinel the store returns when a user id is unknown.
var ErrNotFound = errors.New("store: user not found")

// User is the record the store holds.
type User struct {
	ID   string
	Name string
}

// Store is a concrete user store. It returns concrete types and defines no
// interface: any consumer that wants to abstract over it declares its own.
type Store struct {
	users map[string]User
}

// NewStore returns a concrete *Store, never an interface, so callers can use
// every method (including ones added later).
func NewStore() *Store {
	return &Store{users: make(map[string]User)}
}

// Add seeds a user; a real store would load from Postgres.
func (s *Store) Add(u User) {
	s.users[u.ID] = u
}

// GetUser returns the user for id, or ErrNotFound wrapped nowhere else.
func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	u, ok := s.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}
```

Create `service.go`:

```go
package userlookup

import (
	"context"
	"errors"
	"fmt"

	"example.com/userlookup/store"
)

// ErrUnknownUser is this package's domain error. The service maps the store's
// ErrNotFound onto it so callers depend on the service's vocabulary, not the
// store's.
var ErrUnknownUser = errors.New("userlookup: unknown user")

// userGetter is the consumer-side interface: it lists the ONLY method Service
// calls on its store. It is unexported because it is an implementation detail of
// how Service names its dependency. The producer (store.Store) defines no
// interface; this one lives here, in the using package.
type userGetter interface {
	GetUser(ctx context.Context, id string) (store.User, error)
}

// Service resolves a user's display name. It is constructed with the narrow
// userGetter interface, so it is coupled to one method, not the store's surface.
type Service struct {
	users userGetter
}

// NewService accepts the narrow interface (accept interfaces) and returns the
// concrete *Service (return structs).
func NewService(users userGetter) *Service {
	return &Service{users: users}
}

// DisplayName returns the user's name, mapping a not-found from the store to the
// package's own ErrUnknownUser and wrapping any other error with context.
func (s *Service) DisplayName(ctx context.Context, id string) (string, error) {
	u, err := s.users.GetUser(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("display name for %q: %w", id, ErrUnknownUser)
		}
		return "", fmt.Errorf("display name for %q: %w", id, err)
	}
	return u.Name, nil
}
```

### The runnable demo

The demo wires the real concrete `*store.Store` into the service, proving the
concrete type satisfies the consumer's interface with no adapter.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/userlookup"
	"example.com/userlookup/store"
)

func main() {
	ctx := context.Background()

	st := store.NewStore()
	st.Add(store.User{ID: "u1", Name: "Alice"})

	// The concrete *store.Store satisfies the service's userGetter implicitly.
	svc := userlookup.NewService(st)

	name, err := svc.DisplayName(ctx, "u1")
	if err != nil {
		panic(err)
	}
	fmt.Printf("u1 -> %s\n", name)

	_, err = svc.DisplayName(ctx, "ghost")
	fmt.Printf("ghost -> unknown user: %v\n", errors.Is(err, userlookup.ErrUnknownUser))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
u1 -> Alice
ghost -> unknown user: true
```

### Tests

The test defines `fakeUserGetter`, a tiny struct that satisfies `userGetter` by
returning whatever the test programs. No mock framework, no producer-side
interface. It drives the mapping from `store.ErrNotFound` to `ErrUnknownUser`
and the pass-through of an arbitrary error, asserting both with `errors.Is`.
`ExampleService_DisplayName` pins the happy-path output so `go test` verifies the
snippet too.

Create `service_test.go`:

```go
package userlookup

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"example.com/userlookup/store"
)

// fakeUserGetter is a hand-written fake satisfying the consumer-side interface.
// It is the whole point: the seam is narrow enough to fake in ten lines.
type fakeUserGetter struct {
	user store.User
	err  error
}

func (f fakeUserGetter) GetUser(ctx context.Context, id string) (store.User, error) {
	return f.user, f.err
}

func TestDisplayName(t *testing.T) {
	t.Parallel()

	otherErr := errors.New("connection refused")

	cases := []struct {
		name    string
		getter  fakeUserGetter
		want    string
		wantErr error // sentinel expected via errors.Is, or nil
	}{
		{
			name:   "found",
			getter: fakeUserGetter{user: store.User{ID: "u1", Name: "Alice"}},
			want:   "Alice",
		},
		{
			name:    "not found maps to domain error",
			getter:  fakeUserGetter{err: store.ErrNotFound},
			wantErr: ErrUnknownUser,
		},
		{
			name:    "other error passes through",
			getter:  fakeUserGetter{err: otherErr},
			wantErr: otherErr,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := NewService(tc.getter)

			got, err := svc.DisplayName(context.Background(), "u1")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want errors.Is(_, %v)", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("DisplayName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDoesNotLeakStoreSentinelOnNotFound(t *testing.T) {
	t.Parallel()
	svc := NewService(fakeUserGetter{err: store.ErrNotFound})

	_, err := svc.DisplayName(context.Background(), "u1")
	// The store's not-found is mapped; but wrapping with %w keeps the chain, so
	// errors.Is still sees the underlying store error too. Both must hold.
	if !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("want ErrUnknownUser in chain, got %v", err)
	}
}

// ExampleService_DisplayName wires the real concrete *store.Store into the
// consumer-side interface; the // Output line is auto-verified by `go test`.
func ExampleService_DisplayName() {
	st := store.NewStore()
	st.Add(store.User{ID: "u1", Name: "Alice"})
	svc := NewService(st)
	name, _ := svc.DisplayName(context.Background(), "u1")
	fmt.Println(name)
	// Output: Alice
}
```

## Review

The structural win is that no interface lives next to `store.Store`: the store
returns concrete types and can grow methods freely, while the service declares
the one-method `userGetter` it actually uses. That is "interfaces belong in the
consumer" made concrete, and it is why the service is trivially fakeable — the
seam is exactly one method wide. One subtlety worth internalizing from the last
test: mapping with `fmt.Errorf("...: %w", ErrUnknownUser)` puts `ErrUnknownUser`
in the chain, and the earlier not-found case wraps `ErrUnknownUser` rather than
the raw `store.ErrNotFound`, so callers depend on the service's vocabulary. If
you instead wrapped `store.ErrNotFound` directly, you would leak the store's
error type across the package boundary and couple every caller to the store.

## Resources

- [Go Code Review Comments — Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — "The interface belongs in the package that uses it, not the package that implements it."
- [Go blog — Working with Errors (Is/As/%w)](https://go.dev/blog/go1.13-errors) — `errors.Is` and wrapping with `%w`.
- [Dave Cheney — SOLID Go Design](https://dave.cheney.net/2016/08/20/solid-go-design) — accept interfaces, return structs.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-behavior-preserving-refactor-test.md](03-behavior-preserving-refactor-test.md) | Next: [05-interface-segregation-role-split.md](05-interface-segregation-role-split.md)
