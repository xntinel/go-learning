# Exercise 1: A User Service That Shares *User As Mutable State

The backing store of almost every service is a map of pointers to structs. This
exercise builds the canonical shape — a `User` with a value `Profile` field and a
pointer `Manager` field, a constructor that returns `*User`, and a concurrency-safe
`Service` backed by `map[string]*User` — and proves the property that makes it
both powerful and dangerous: a mutation through one `*User` is visible to every
holder of that pointer.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
userservice/               independent module: example.com/userservice
  go.mod
  user.go                  User{Profile, Manager *User}; New; Service over map[string]*User
  cmd/
    demo/
      main.go              add two users, wire a manager, mutate through the shared pointer
  user_test.go             constructor validation, pointer identity, duplicate/not-found, race
```

Files: `user.go`, `cmd/demo/main.go`, `user_test.go`.
Implement: a `User` struct, `New(id, email) (*User, error)` with validation, mutex-guarded `SetManager`/`GetManager`, and a `Service` with `Add`, `Get`, `FindByEmail` over `map[string]*User` guarded by a `sync.RWMutex`.
Test: `New` rejects empty fields and initializes `Profile`/`CreatedAt`; `Add`+`Get` return the SAME pointer; duplicate `Add` is `ErrAlreadyExists`; missing `Get` is `ErrNotFound`; a pointer mutation is visible to all holders; `FindByEmail` finds `u1`; concurrent `Add` is race-clean.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/05-pointers-to-structs/01-user-service-shared-mutable-state/cmd/demo
cd go-solutions/09-pointers/05-pointers-to-structs/01-user-service-shared-mutable-state
```

### Why the store holds pointers, and why identity is a contract

The `Service` keeps `map[string]*User`, not `map[string]User`. If it stored values,
`Get` would return a copy and a caller could never mutate the stored user — every
update would have to go back through the service with a write lock. Storing
pointers means the map entry, the pointer `Add` received, and the pointer `Get`
returns all name the *same* `User`. That is a real contract, not an implementation
detail, so the test asserts `got == u`: pointer equality proves the service did not
silently copy the struct. (The trade-off — that callers can now mutate stored state
through the shared pointer — is exactly the aliasing hazard Exercise 3 fixes with
defensive copies. Here we lean into sharing; there we guard against it.)

`New` returns `(*User, error)`. Returning `*User` is the standard constructor shape
for a type meant to be mutated after construction: the caller keeps one struct and
mutates it in place, and `New` owns validation and default initialization
(`Profile.DisplayName` defaults to the email, `CreatedAt` is stamped). Returning
`&User{...}` is safe — escape analysis moves the struct to the heap so the pointer
never dangles.

The `User` type mixes both field kinds on purpose. `Profile` is a value field: a
small, self-contained struct that is fine to copy and does not need sharing. `Manager`
is `*User`: a manager is a distinct entity that other users also point at, and
whose own fields change independently, so it must be a shared reference, not an
embedded copy. `SetManager`/`GetManager` are mutex-guarded because the manager link
can be read and written concurrently; the pointer itself provides no synchronization.

Create `user.go`:

```go
package user

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrNotFound      = errors.New("user not found")
	ErrAlreadyExists = errors.New("user already exists")
)

// Profile is a small, self-contained value. It is a value field on User because
// it is cheap to copy and does not need to be shared independently.
type Profile struct {
	DisplayName string
	Bio         string
}

// User is shared as *User so many holders name one struct. Profile is embedded
// by value; Manager is a *User because a manager is a distinct, shared entity.
type User struct {
	ID        string
	Email     string
	Profile   Profile
	Manager   *User
	CreatedAt time.Time

	mu sync.Mutex // guards Manager against concurrent Set/Get
}

// New validates its inputs and returns a *User ready to be mutated by callers.
func New(id, email string) (*User, error) {
	if id == "" {
		return nil, errors.New("id is required")
	}
	if email == "" {
		return nil, errors.New("email is required")
	}
	return &User{
		ID:        id,
		Email:     email,
		Profile:   Profile{DisplayName: email},
		CreatedAt: time.Now(),
	}, nil
}

func (u *User) SetManager(m *User) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.Manager = m
}

func (u *User) GetManager() *User {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.Manager
}

// Service is the in-memory store. It keeps *User so Get hands back the same
// struct that was added; the RWMutex guards the maps, not the users themselves.
type Service struct {
	mu     sync.RWMutex
	byID   map[string]*User
	byMail map[string]*User
}

func NewService() *Service {
	return &Service{
		byID:   make(map[string]*User),
		byMail: make(map[string]*User),
	}
}

func (s *Service) Add(u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[u.ID]; ok {
		return ErrAlreadyExists
	}
	s.byID[u.ID] = u
	s.byMail[u.Email] = u
	return nil
}

func (s *Service) Get(id string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	return u, nil
}

func (s *Service) FindByEmail(email string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.byMail[email]
	if !ok {
		return nil, ErrNotFound
	}
	return u, nil
}
```

### The runnable demo

The demo adds two users, wires one as the other's manager, then mutates a user's
display name through the pointer the service returned — and reads the change back
through a *different* pointer to the same struct, showing the shared-state property
in action.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/userservice"
)

func main() {
	svc := user.NewService()

	boss, _ := user.New("u2", "boss@example.com")
	alice, _ := user.New("u1", "alice@example.com")
	_ = svc.Add(boss)
	_ = svc.Add(alice)

	alice.SetManager(boss)

	// Fetch alice again: same underlying struct as the local `alice`.
	got, _ := svc.Get("u1")
	got.Profile.DisplayName = "Alice A."

	fmt.Printf("display name via original pointer: %s\n", alice.Profile.DisplayName)
	fmt.Printf("manager email: %s\n", alice.GetManager().Email)

	if err := svc.Add(alice); err != nil {
		fmt.Printf("duplicate add: %v\n", err)
	}
}
```

Note the package name is `user` even though the module is `example.com/userservice`;
the import path is the module path and the referenced identifier is the package name.

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
display name via original pointer: Alice A.
manager email: boss@example.com
duplicate add: user already exists
```

### Tests

The tests pin every part of the contract. `TestServiceAddAndGet` asserts pointer
identity with `got == u`. `TestPointerMutationVisibleToAllHolders` is the heart of
the lesson: two variables name one `*User`, a mutation through one is seen through
the other. Sentinel errors are asserted with `errors.Is`. `TestServiceFindByEmail`
pins that email lookup returns the user with ID `u1`. The concurrent test exists to
run the maps' locking under `-race`.

Create `user_test.go`:

```go
package user

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestNewRejectsEmptyFields(t *testing.T) {
	t.Parallel()
	if _, err := New("", "x@y.z"); err == nil {
		t.Fatal("expected error for empty id")
	}
	if _, err := New("u1", ""); err == nil {
		t.Fatal("expected error for empty email")
	}
}

func TestNewInitializesProfileAndCreatedAt(t *testing.T) {
	t.Parallel()
	u, err := New("u1", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "u1" {
		t.Fatalf("ID = %q, want u1", u.ID)
	}
	if u.Profile.DisplayName != "alice@example.com" {
		t.Fatalf("DisplayName = %q, want email default", u.Profile.DisplayName)
	}
	if u.CreatedAt.IsZero() {
		t.Fatal("CreatedAt is zero; New must stamp it")
	}
}

func TestServiceAddAndGet(t *testing.T) {
	t.Parallel()
	s := NewService()
	u, _ := New("u1", "alice@example.com")
	if err := s.Add(u); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("u1")
	if err != nil {
		t.Fatal(err)
	}
	if got != u {
		t.Fatal("Get must return the SAME pointer that was Added (identity)")
	}
}

func TestServiceAddRejectsDuplicate(t *testing.T) {
	t.Parallel()
	s := NewService()
	u, _ := New("u1", "alice@example.com")
	if err := s.Add(u); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(u); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
}

func TestServiceGetReturnsNotFound(t *testing.T) {
	t.Parallel()
	s := NewService()
	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestServiceFindByEmail(t *testing.T) {
	t.Parallel()
	s := NewService()
	u, _ := New("u1", "alice@example.com")
	if err := s.Add(u); err != nil {
		t.Fatal(err)
	}
	got, err := s.FindByEmail("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "u1" {
		t.Fatalf("FindByEmail ID = %q, want u1", got.ID)
	}
	if _, err := s.FindByEmail("nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPointerMutationVisibleToAllHolders(t *testing.T) {
	t.Parallel()
	u, _ := New("u1", "alice@example.com")
	holder := u // same *User, not a copy
	u.Profile.DisplayName = "Alice"
	if holder.Profile.DisplayName != "Alice" {
		t.Fatal("mutation must be visible to all holders of the pointer")
	}
}

func TestSetGetManagerRoundTrips(t *testing.T) {
	t.Parallel()
	alice, _ := New("u1", "alice@example.com")
	boss, _ := New("u2", "boss@example.com")
	alice.SetManager(boss)
	if alice.GetManager() != boss {
		t.Fatal("GetManager must return the pointer set by SetManager")
	}
}

func TestServiceIsSafeUnderConcurrentAdd(t *testing.T) {
	t.Parallel()
	s := NewService()
	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u, _ := New(fmt.Sprintf("u%d", i), fmt.Sprintf("u%d@x.z", i))
			_ = s.Add(u)
		}(i)
	}
	wg.Wait()
	if _, err := s.Get("u0"); err != nil {
		t.Fatalf("u0 missing after concurrent adds: %v", err)
	}
}

func Example() {
	s := NewService()
	u, _ := New("u1", "alice@example.com")
	_ = s.Add(u)
	got, _ := s.Get("u1")
	fmt.Println(got.Email, got == u)
	// Output: alice@example.com true
}
```

## Review

The service is correct when `Get` returns the exact pointer that was `Add`ed
(`got == u`), when a mutation through any holder is seen through every holder, and
when duplicate and missing lookups return the sentinel errors matched by
`errors.Is`. The pointer-identity test is the one that distinguishes a real shared
store from one that accidentally copies structs; if it fails, some layer is
returning `*u` or a fresh `&User{...}` instead of the stored pointer.

The mistakes to avoid: do not give the store value semantics (`map[string]User`),
which would force every update back through the service and copy on every `Get`;
do not forget the mutex on `Manager` — the pointer does not synchronize concurrent
reads and writes; and remember that this shared-pointer design is exactly what
lets an unrelated caller mutate stored state, which Exercise 3 addresses with
defensive copies. Run `go test -race` to confirm the maps' locking holds under
concurrent producers.

## Resources

- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — when a method or constructor should hand back a pointer.
- [Go Specification: Pointer types](https://go.dev/ref/spec#Pointer_types) — the address-of operator and pointer semantics.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read/write lock guarding the store's maps.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-patch-optional-pointer-fields.md](02-patch-optional-pointer-fields.md)
