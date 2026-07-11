# Exercise 1: A UserService That Returns (value, error) With a ServiceError Wrapper

The service layer is where the `(value, error)` contract earns its keep: a
repository method either returns a fully-valid value and a nil error, or the zero
value and a wrapped sentinel. This exercise builds that layer with a custom
`ServiceError{Op, Err}` type so callers get both an operation-tagged message for
logs and a machine-comparable sentinel underneath for control flow.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
userservice/                 independent module: example.com/userservice
  go.mod                     go 1.26
  service.go                 sentinels; ServiceError{Op,Err}; UserService Create/Get/Delete
  cmd/
    demo/
      main.go                runnable demo: create, get, duplicate, not-found
  service_test.go            table + property tests; errors.Is through Unwrap
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: sentinels `ErrNotFound`/`ErrAlreadyExists`/`ErrInvalidID`; a `ServiceError` with `Error() string` and `Unwrap() error`; `UserService` with `Create`, `Get`, `Delete` returning the zero value plus a wrapped sentinel on failure and nil on success.
- Test: assert `errors.Is` reaches each sentinel through `*ServiceError.Unwrap`, the success path returns a nil error and the populated value, `Error()` includes the `Op`, and the error path returns `User{}`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/userservice/cmd/demo
cd ~/go-exercises/userservice
go mod init example.com/userservice
```

### Why a wrapping ServiceError instead of a bare sentinel

Returning a bare sentinel (`return User{}, ErrNotFound`) tells the caller *what*
failed but not *where*. In a service with a dozen methods, a log line reading
`user not found` forces the reader to guess which operation produced it. The
`ServiceError{Op, Err}` type solves both audiences at once: `Error()` renders
`Get: user not found` for the human reading logs, while `Unwrap()` exposes the
sentinel underneath so `errors.Is(err, ErrNotFound)` still matches for the code
branching on the failure. This is the smallest useful custom error type — one
operation string and one wrapped error — and it is the shape most real repository
and service layers converge on.

The two methods are exactly what the standard library expects. `Error() string`
is the whole `error` interface. `Unwrap() error` is the hook `errors.Is` and
`errors.As` call to walk the chain: without it, `errors.Is(err, ErrNotFound)`
would compare the outer `*ServiceError` against the sentinel, fail, and stop.
With it, `errors.Is` unwraps to `Err` and finds the match. The sentinels
themselves are package-level `var`s built once with `errors.New`, so they are
compared by identity — the property the whole scheme depends on.

Every failing branch returns `User{}` (or nothing, for `Delete`), never a
partially-built value. That is the "zero value on the error path" contract: a
caller that ignores the error and reads the `User` gets an obviously-empty struct,
not a plausible-looking half-record.

Create `service.go`:

```go
package userservice

import (
	"errors"
	"fmt"
)

// Sentinel errors are single package-level values, compared by identity through
// errors.Is. Callers branch on these; they are part of the exported contract.
var (
	ErrNotFound      = errors.New("user not found")
	ErrAlreadyExists = errors.New("user already exists")
	ErrInvalidID     = errors.New("invalid id")
)

// User is the domain value the service stores and returns.
type User struct {
	ID    string
	Email string
}

// ServiceError tags a failure with the operation that produced it and wraps the
// underlying sentinel. Error() serves logs; Unwrap() lets errors.Is reach Err.
type ServiceError struct {
	Op  string
	Err error
}

func (e *ServiceError) Error() string {
	return fmt.Sprintf("%s: %s", e.Op, e.Err)
}

func (e *ServiceError) Unwrap() error {
	return e.Err
}

// UserService is an in-memory repository keyed by user id.
type UserService struct {
	byID map[string]User
}

func NewUserService() *UserService {
	return &UserService{byID: make(map[string]User)}
}

// Create stores a new user. It returns User{} and a wrapped ErrInvalidID for an
// empty id, User{} and a wrapped ErrAlreadyExists for a duplicate, otherwise the
// stored user and a nil error.
func (s *UserService) Create(id, email string) (User, error) {
	if id == "" {
		return User{}, &ServiceError{Op: "Create", Err: ErrInvalidID}
	}
	if _, ok := s.byID[id]; ok {
		return User{}, &ServiceError{Op: "Create", Err: ErrAlreadyExists}
	}
	u := User{ID: id, Email: email}
	s.byID[id] = u
	return u, nil
}

// Get returns the stored user or User{} and a wrapped ErrNotFound.
func (s *UserService) Get(id string) (User, error) {
	u, ok := s.byID[id]
	if !ok {
		return User{}, &ServiceError{Op: "Get", Err: ErrNotFound}
	}
	return u, nil
}

// Delete removes a user or returns a wrapped ErrNotFound if it is absent.
func (s *UserService) Delete(id string) error {
	if _, ok := s.byID[id]; !ok {
		return &ServiceError{Op: "Delete", Err: ErrNotFound}
	}
	delete(s.byID, id)
	return nil
}
```

### The runnable demo

The demo exercises each path once and prints the outcome, including the
operation-tagged error message a duplicate `Create` produces.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/userservice"
)

func main() {
	s := userservice.NewUserService()

	u, err := s.Create("u1", "alice@example.com")
	if err != nil {
		fmt.Println("create failed:", err)
		return
	}
	fmt.Printf("created: %s <%s>\n", u.ID, u.Email)

	got, _ := s.Get("u1")
	fmt.Printf("got: %s <%s>\n", got.ID, got.Email)

	if _, err := s.Create("u1", "alice2@example.com"); err != nil {
		fmt.Println("duplicate create:", err)
		fmt.Println("is ErrAlreadyExists:", errors.Is(err, userservice.ErrAlreadyExists))
	}

	if _, err := s.Get("missing"); err != nil {
		fmt.Println("missing get:", err)
		fmt.Println("is ErrNotFound:", errors.Is(err, userservice.ErrNotFound))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
created: u1 <alice@example.com>
got: u1 <alice@example.com>
duplicate create: Create: user already exists
is ErrAlreadyExists: true
missing get: Get: user not found
is ErrNotFound: true
```

### Tests

The table covers the four public behaviors plus the two properties the lesson is
really about. `TestServiceErrorUnwrapsToSentinel` is the pinned contract: it calls
`errors.Unwrap` directly and asserts the underlying value *is* `ErrNotFound`,
proving the chain is one hop deep and lands on the sentinel.
`TestServiceErrorMessageIncludesOp` proves the `Op` tag survives into the rendered
message. Every error-path assertion also checks the returned `User` is the zero
value.

Create `service_test.go`:

```go
package userservice

import (
	"errors"
	"strings"
	"testing"
)

func TestCreateAndGet(t *testing.T) {
	t.Parallel()

	s := NewUserService()
	u, err := s.Create("u1", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "u1" {
		t.Fatalf("u.ID = %q, want u1", u.ID)
	}

	got, err := s.Get("u1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "alice@example.com" {
		t.Fatalf("got.Email = %q, want alice@example.com", got.Email)
	}
}

func TestCreateRejectsEmptyID(t *testing.T) {
	t.Parallel()

	s := NewUserService()
	u, err := s.Create("", "alice@example.com")
	if !errors.Is(err, ErrInvalidID) {
		t.Fatalf("err = %v, want ErrInvalidID", err)
	}
	if u != (User{}) {
		t.Fatalf("value on error path = %+v, want zero User", u)
	}
}

func TestCreateRejectsDuplicate(t *testing.T) {
	t.Parallel()

	s := NewUserService()
	_, _ = s.Create("u1", "alice@example.com")
	u, err := s.Create("u1", "alice2@example.com")
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
	if u != (User{}) {
		t.Fatalf("value on error path = %+v, want zero User", u)
	}
}

func TestGetReturnsNotFound(t *testing.T) {
	t.Parallel()

	s := NewUserService()
	u, err := s.Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if u != (User{}) {
		t.Fatalf("value on error path = %+v, want zero User", u)
	}
}

func TestServiceErrorMessageIncludesOp(t *testing.T) {
	t.Parallel()

	s := NewUserService()
	_, err := s.Get("missing")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Get") {
		t.Fatalf("err.Error() = %q, want it to include Get", err.Error())
	}
}

// TestServiceErrorUnwrapsToSentinel pins the contract that a ServiceError unwraps
// exactly to its sentinel, so errors.Is callers can rely on it.
func TestServiceErrorUnwrapsToSentinel(t *testing.T) {
	t.Parallel()

	s := NewUserService()
	_, err := s.Get("missing")

	var se *ServiceError
	if !errors.As(err, &se) {
		t.Fatalf("err is not *ServiceError: %v", err)
	}
	if got := errors.Unwrap(err); got != ErrNotFound {
		t.Fatalf("Unwrap(err) = %v, want ErrNotFound", got)
	}
}

func TestDeleteRemovesAndThenNotFound(t *testing.T) {
	t.Parallel()

	s := NewUserService()
	_, _ = s.Create("u1", "alice@example.com")
	if err := s.Delete("u1"); err != nil {
		t.Fatalf("Delete = %v, want nil", err)
	}
	if err := s.Delete("u1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
}
```

## Review

The service is correct when every method obeys the two-part contract: on success
a nil error and a fully-populated value, on failure a wrapped sentinel and the
zero value. The property tests are the ones that matter most.
`TestServiceErrorUnwrapsToSentinel` proves `errors.Is` will reach `ErrNotFound`
through the `*ServiceError`, which is what any caller branching on the failure
depends on; if `Unwrap` were missing, that test fails immediately. The
zero-value checks on each error path guard the subtle regression where a method
starts returning a half-built `User` next to its error.

The traps here are the chapter's core lessons in miniature. Do not compare the
returned error to a sentinel with `==` — it is a `*ServiceError`, so `==` misses;
use `errors.Is`. Do not assert on `err.Error()` text beyond the operation tag —
the message is for logs and will drift. Keep the sentinels as shared package-level
values; rebuilding one inline would break every `errors.Is` in the codebase. Run
`go test -race` to confirm nothing here trips the race detector.

## Resources

- [pkg.go.dev: errors package](https://pkg.go.dev/errors) — `New`, `Is`, `As`, `Unwrap`, and the `Unwrap() error` convention.
- [pkg.go.dev: builtin error](https://pkg.go.dev/builtin#error) — the one-method interface definition.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — wrapping, `Unwrap`, and `errors.Is`/`errors.As`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-config-loader-zero-value.md](02-config-loader-zero-value.md)
