# Exercise 1: A Sentinel Error Hierarchy for a User Domain

The foundation of the whole lesson: a user domain whose errors form a tree of
plain sentinel values connected by `%w`, so a caller can branch on a category
without naming a concrete type. This is the cheapest hierarchy there is, and for a
pure category with no data to carry it is also the best.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
sentinel-error-hierarchy/          module example.com/sentinel-error-hierarchy
  go.mod
  internal/domain/domain.go        ErrDomain -> ErrUser -> {NotFound,Exists,Invalid}; Service; IsUser/IsDomain
  cmd/demo/main.go                 duplicate-add and missing-get, printing category checks
  internal/domain/domain_test.go   errors.Is walks the chain; message carries the category
```

- Files: `internal/domain/domain.go`, `cmd/demo/main.go`, `internal/domain/domain_test.go`.
- Implement: a base `ErrDomain`, a mid base `ErrUser`, three leaf sentinels wrapped with `%w`, a `Service` with `Add`/`Get`/`Delete`, and `IsDomain`/`IsUser` helpers.
- Test: table-driven assertions that a not-found error `errors.Is` the leaf, the mid base, and the root; that `IsUser` rejects an unrelated error; and that the rendered message carries the category words.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/13-designing-an-error-hierarchy/01-sentinel-error-hierarchy/internal/domain
mkdir -p go-solutions/10-error-handling/13-designing-an-error-hierarchy/01-sentinel-error-hierarchy/cmd/demo
cd go-solutions/10-error-handling/13-designing-an-error-hierarchy/01-sentinel-error-hierarchy
```

### Why sentinels, and how the tree matches

The domain has exactly three failure categories — a user is missing, a user
already exists, a user is invalid — and none of them needs to carry data beyond its
identity. That is the signature case for *sentinel* errors: a single comparable
`error` value per category, declared once at package scope. The hierarchy comes
from wrapping. `ErrDomain` is the root; `ErrUser` wraps it with `%w`; each leaf
wraps `ErrUser` with `%w`. Because `%w` makes `Unwrap` return the parent, a single
`errors.Is(err, ErrUser)` is true for `ErrUserNotFound`, `ErrUserExists`, and
`ErrUserInvalid` alike, and `errors.Is(err, ErrDomain)` is true for all of them.

That is the payoff a caller wants. A generic handler that only cares "is this a
user-domain failure I should turn into a 4xx?" checks `ErrUser`. A specific handler
that renders a 404 checks `ErrUserNotFound`. Neither depends on the other's
granularity, and neither imports a concrete error type — the sentinels *are* the
interface. `IsDomain` and `IsUser` are just named conveniences over the two most
common `errors.Is` checks, so callers in other packages read `domain.IsUser(err)`
instead of spelling the base out.

The `Service` is an in-memory stand-in for a repository so the exercise stays
focused on the error contract rather than storage. `Add` returns `ErrUserExists` on
a duplicate id, `Get` and `Delete` return `ErrUserNotFound` on a miss, and
`NewUser` rejects empty fields with `ErrUserInvalid`. Every method returns a
sentinel, so every caller can branch on a category.

Create `internal/domain/domain.go`:

```go
package domain

import (
	"errors"
	"fmt"
)

var ErrDomain = errors.New("domain error")

var (
	ErrUser         = fmt.Errorf("user: %w", ErrDomain)
	ErrUserNotFound = fmt.Errorf("user: not found: %w", ErrUser)
	ErrUserExists   = fmt.Errorf("user: already exists: %w", ErrUser)
	ErrUserInvalid  = fmt.Errorf("user: invalid: %w", ErrUser)
)

type User struct {
	ID    string
	Email string
}

func NewUser(id, email string) (*User, error) {
	if id == "" || email == "" {
		return nil, ErrUserInvalid
	}
	return &User{ID: id, Email: email}, nil
}

type Service struct {
	byID map[string]*User
}

func NewService() *Service {
	return &Service{byID: make(map[string]*User)}
}

func (s *Service) Add(u *User) error {
	if _, ok := s.byID[u.ID]; ok {
		return ErrUserExists
	}
	s.byID[u.ID] = u
	return nil
}

func (s *Service) Get(id string) (*User, error) {
	u, ok := s.byID[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	return u, nil
}

func (s *Service) Delete(id string) error {
	if _, ok := s.byID[id]; !ok {
		return ErrUserNotFound
	}
	delete(s.byID, id)
	return nil
}

func IsDomain(err error) bool { return errors.Is(err, ErrDomain) }

func IsUser(err error) bool { return errors.Is(err, ErrUser) }
```

### The runnable demo

The demo drives the two most interesting paths — a duplicate add and a missing get
— and prints the category checks so you can watch a single leaf error satisfy the
leaf, the mid base, and the root at once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/sentinel-error-hierarchy/internal/domain"
)

func main() {
	s := domain.NewService()
	u, _ := domain.NewUser("u1", "alice@example.com")
	_ = s.Add(u)

	if err := s.Add(u); err != nil {
		fmt.Printf("add duplicate: %v\n", err)
		fmt.Printf("  IsUser=%v IsDomain=%v exists=%v\n",
			domain.IsUser(err), domain.IsDomain(err),
			errors.Is(err, domain.ErrUserExists))
	}

	if _, err := s.Get("missing"); err != nil {
		fmt.Printf("get missing: %v\n", err)
		fmt.Printf("  IsUser=%v notFound=%v\n",
			domain.IsUser(err), errors.Is(err, domain.ErrUserNotFound))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
add duplicate: user: already exists: user: domain error
  IsUser=true IsDomain=true exists=true
get missing: user: not found: user: domain error
  IsUser=true notFound=true
```

### Tests

The central test proves the tree matches: a single not-found error `errors.Is` the
leaf, the mid base, *and* the root, which is the property that lets callers pick
their own granularity. The others cover the duplicate and delete paths, prove
`IsUser` rejects an unrelated error (the hierarchy is not a catch-all), and pin the
"the message carries the category" contract that a log pipeline relies on.

Create `internal/domain/domain_test.go`:

```go
package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNotFoundMatchesEveryLevelOfTheTree(t *testing.T) {
	t.Parallel()
	s := NewService()
	_, err := s.Get("missing")

	for _, want := range []struct {
		name   string
		target error
	}{
		{"leaf", ErrUserNotFound},
		{"mid base", ErrUser},
		{"root", ErrDomain},
	} {
		if !errors.Is(err, want.target) {
			t.Errorf("errors.Is(err, %s) = false; want true", want.name)
		}
	}
}

func TestServiceCategories(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  func(*Service) error
		want error
	}{
		{
			name: "duplicate add",
			run: func(s *Service) error {
				u, _ := NewUser("u1", "a@example.com")
				_ = s.Add(u)
				return s.Add(u)
			},
			want: ErrUserExists,
		},
		{
			name: "get missing",
			run:  func(s *Service) error { _, err := s.Get("x"); return err },
			want: ErrUserNotFound,
		},
		{
			name: "delete missing",
			run:  func(s *Service) error { return s.Delete("x") },
			want: ErrUserNotFound,
		},
		{
			name: "new user empty id",
			run:  func(s *Service) error { _, err := NewUser("", "a@x.com"); return err },
			want: ErrUserInvalid,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.run(NewService()); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v; want errors.Is %v", err, tc.want)
			}
		})
	}
}

func TestHelpersRejectUnrelated(t *testing.T) {
	t.Parallel()
	other := errors.New("unrelated")
	if IsUser(other) {
		t.Error("IsUser(unrelated) = true; want false")
	}
	if IsDomain(other) {
		t.Error("IsDomain(unrelated) = true; want false")
	}
}

func TestErrorMessageIncludesCategory(t *testing.T) {
	t.Parallel()
	_, err := NewService().Get("missing")
	msg := err.Error()
	for _, want := range []string{"user", "not found"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
}

func ExampleService_Get() {
	s := NewService()
	_, err := s.Get("missing")
	fmt.Println(errors.Is(err, ErrUser), errors.Is(err, ErrUserNotFound))
	// Output: true true
}
```

## Review

The hierarchy is correct when one leaf error answers three category questions
truthfully: `errors.Is` against the leaf, the mid base, and the root all return
true, because `%w` put each parent on the unwrap path. If any of those returns
false, you almost certainly wrapped with `%v` somewhere and severed the chain. The
`IsUser(unrelated) == false` assertion matters as much as the positive one — a
hierarchy that matches everything is as useless as one that matches nothing. Keep
the sentinels exported so packages outside `domain` can express the category checks
they need; an unexported leaf is invisible to the transport that has to map it.
Sentinels are the right call here precisely because none of these categories
carries data — the moment one needs to report *which* field was invalid, you reach
for the typed error of the next exercise.

## Resources

- [`errors.Is`](https://pkg.go.dev/errors#Is) — how the walk and the `==`/`Is` comparison at each node work.
- [`fmt.Errorf` and `%w`](https://pkg.go.dev/fmt#Errorf) — wrapping that keeps `Unwrap` returning the parent.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — the design rationale for `Is`/`As`/`Unwrap`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-typed-domain-error-with-is.md](02-typed-domain-error-with-is.md)
