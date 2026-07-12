# Exercise 1: Service Layer With Sentinels, Custom Is, and Typed Extraction

The service layer of a backend sits between HTTP handlers and the storage layer,
and its job is to return errors that upper layers can classify. This exercise
builds an `ItemService` whose failures carry both a semantic identity (a sentinel:
not-found, already-exists, permission-denied) and a typed payload (`*ServiceError`
with the operation name), and proves that `errors.Is` finds the sentinel through
two wrap layers while `errors.As` recovers the typed value.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
erris/                          independent module: example.com/erris
  go.mod                        go 1.25
  service/service.go            ServiceError{Op,Wrapped}; sentinels; ItemService.Create/Get
  service/service_test.go       Is/As/Unwrap table tests + custom-Is-by-Op
  cmd/demo/main.go              runnable demo classifying each failure
```

Files: `service/service.go`, `service/service_test.go`, `cmd/demo/main.go`.
Implement: `ItemService` with `Create`/`Get` returning `fmt.Errorf("...: %w", &ServiceError{...})`, and `ServiceError` implementing `Error`, `Unwrap`, `Is`.
Test: `errors.Is` finds each sentinel through two wraps; `errors.As` populates `*ServiceError`; custom `Is` matches by `Op`; `errors.Unwrap` yields the `*ServiceError`.
Verify: `go test -count=1 -race ./... && go vet ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Two kinds of information on one error

Every failure this service returns carries two independent facts. The first is
its *kind* — is this a missing item, a duplicate, a permission problem? That is
encoded as a sentinel: a package-level `var Err... = errors.New(...)` that upper
layers compare against with `errors.Is`. The kind is what a handler needs to pick
an HTTP status. The second fact is *which operation* produced it, which the
service records in a typed `*ServiceError{Op, Wrapped}`. The `Op` field is
diagnostic payload — useful in logs and for asserting the failure came from the
path you expect — and it is recovered with `errors.As`.

`ServiceError` implements three methods, and each earns its place. `Error()`
renders `"Op: wrapped"` for humans and logs. `Unwrap()` returns the wrapped
sentinel, which is what makes `errors.Is(err, ErrNotFound)` succeed: `Is` walks
from the outer `fmt.Errorf` wrapper, into the `*ServiceError`, through `Unwrap`,
to the sentinel. `Is(target error)` overrides matching so that a caller can ask
"did this come from `Get`?" by writing `errors.Is(err, &ServiceError{Op: "Get"})`
— the method compares only the `Op` field and returns false for any target that
is not a `*ServiceError`, so it never panics on an unrelated target.

Both `Create` and `Get` wrap their `*ServiceError` one more time with
`fmt.Errorf("op: %w", ...)`. That second layer is deliberate: it proves the tests
are exercising real traversal. `errors.Is` and `errors.As` have to descend two
`Unwrap` hops — the outer `fmt` wrapper, then the `*ServiceError` — to reach the
sentinel, which is exactly the shape production errors take once several layers
have added context.

Create `service/service.go`:

```go
package service

import (
	"errors"
	"fmt"
)

// Sentinels encode the KIND of a failure. Upper layers classify with errors.Is.
var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrPermission    = errors.New("permission denied")
)

// ServiceError carries the operation name (payload) and wraps the sentinel
// (identity). Callers recover it with errors.As and match its Op with a custom
// Is method.
type ServiceError struct {
	Op      string
	Wrapped error
}

func (e *ServiceError) Error() string {
	if e.Wrapped != nil {
		return fmt.Sprintf("%s: %s", e.Op, e.Wrapped)
	}
	return e.Op
}

// Unwrap exposes the wrapped sentinel so errors.Is/errors.As can descend.
func (e *ServiceError) Unwrap() error { return e.Wrapped }

// Is matches any *ServiceError target with the same Op, and returns false for
// every other target type (never panics).
func (e *ServiceError) Is(target error) bool {
	t, ok := target.(*ServiceError)
	if !ok {
		return false
	}
	return e.Op == t.Op
}

type Item struct {
	ID    string
	Owner string
}

type ItemService struct {
	byID map[string]Item
}

func NewItemService() *ItemService {
	return &ItemService{byID: make(map[string]Item)}
}

// Create stores a new item, or fails with ErrAlreadyExists wrapped in a
// *ServiceError, wrapped once more with fmt.Errorf.
func (s *ItemService) Create(id, owner string) (Item, error) {
	if _, ok := s.byID[id]; ok {
		return Item{}, fmt.Errorf("create %s: %w", id, &ServiceError{Op: "Create", Wrapped: ErrAlreadyExists})
	}
	it := Item{ID: id, Owner: owner}
	s.byID[id] = it
	return it, nil
}

// Get returns the item if it exists and the caller owns it; otherwise it wraps
// ErrNotFound or ErrPermission in a *ServiceError with Op "Get".
func (s *ItemService) Get(caller, id string) (Item, error) {
	it, ok := s.byID[id]
	if !ok {
		return Item{}, fmt.Errorf("get %s: %w", id, &ServiceError{Op: "Get", Wrapped: ErrNotFound})
	}
	if it.Owner != caller {
		return Item{}, fmt.Errorf("get %s: %w", id, &ServiceError{Op: "Get", Wrapped: ErrPermission})
	}
	return it, nil
}
```

### The runnable demo

The demo drives every failure path and classifies each returned error the way a
caller would — `errors.Is` on the sentinels, `errors.As` on the type.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/erris/service"
)

func main() {
	s := service.NewItemService()

	if _, err := s.Create("i1", "alice"); err != nil {
		fmt.Println("unexpected:", err)
	} else {
		fmt.Println("created i1 for alice")
	}

	_, err := s.Create("i1", "bob")
	fmt.Printf("duplicate is ErrAlreadyExists: %v\n", errors.Is(err, service.ErrAlreadyExists))

	_, err = s.Get("alice", "missing")
	fmt.Printf("missing is ErrNotFound: %v\n", errors.Is(err, service.ErrNotFound))

	_, err = s.Get("bob", "i1")
	var se *service.ServiceError
	if errors.As(err, &se) {
		fmt.Printf("permission failure Op=%s isErrPermission=%v\n", se.Op, errors.Is(err, service.ErrPermission))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
created i1 for alice
duplicate is ErrAlreadyExists: true
missing is ErrNotFound: true
permission failure Op=Get isErrPermission=true
```

### Tests

The tests pin four contracts. `TestIsFindsSentinels` proves `errors.Is` reaches
each sentinel through both wrap layers. `TestAsExtractsServiceError` proves
`errors.As` recovers the typed value and reads `se.Op == "Get"`.
`TestIsByOp` proves the custom `Is` method matches `&ServiceError{Op: "Get"}` but
not `{Op: "Create"}`. `TestUnwrapReturnsServiceError` folds in the contract test
the original lesson asked for: `errors.Unwrap` on the outer error yields the
`*ServiceError` itself.

Create `service/service_test.go`:

```go
package service

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsFindsSentinels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		call     func(s *ItemService) error
		sentinel error
	}{
		{"not found", func(s *ItemService) error { _, err := s.Get("alice", "missing"); return err }, ErrNotFound},
		{"already exists", func(s *ItemService) error {
			_, _ = s.Create("i1", "alice")
			_, err := s.Create("i1", "bob")
			return err
		}, ErrAlreadyExists},
		{"permission", func(s *ItemService) error {
			_, _ = s.Create("i1", "alice")
			_, err := s.Get("bob", "i1")
			return err
		}, ErrPermission},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.call(NewItemService())
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("errors.Is(%v, %v) = false, want true", err, tc.sentinel)
			}
		})
	}
}

func TestAsExtractsServiceError(t *testing.T) {
	t.Parallel()
	_, err := NewItemService().Get("alice", "missing")
	var se *ServiceError
	if !errors.As(err, &se) {
		t.Fatalf("errors.As(%v, *ServiceError) = false, want true", err)
	}
	if se.Op != "Get" {
		t.Fatalf("se.Op = %q, want Get", se.Op)
	}
	if !errors.Is(se.Wrapped, ErrNotFound) {
		t.Fatalf("se.Wrapped = %v, want ErrNotFound", se.Wrapped)
	}
}

func TestIsByOp(t *testing.T) {
	t.Parallel()
	_, err := NewItemService().Get("alice", "missing")
	if !errors.Is(err, &ServiceError{Op: "Get"}) {
		t.Fatalf("errors.Is(%v, {Op:Get}) = false, want true", err)
	}
	if errors.Is(err, &ServiceError{Op: "Create"}) {
		t.Fatalf("errors.Is(%v, {Op:Create}) = true, want false", err)
	}
}

func TestUnwrapReturnsServiceError(t *testing.T) {
	t.Parallel()
	_, err := NewItemService().Get("alice", "missing")
	inner := errors.Unwrap(err) // peel the outer fmt.Errorf wrapper
	var se *ServiceError
	if !errors.As(inner, &se) {
		t.Fatalf("errors.Unwrap(%v) = %v, want a *ServiceError", err, inner)
	}
	if se.Op != "Get" {
		t.Fatalf("unwrapped Op = %q, want Get", se.Op)
	}
}

func Example() {
	s := NewItemService()
	_, _ = s.Create("i1", "alice")
	_, err := s.Get("bob", "i1")
	fmt.Println(errors.Is(err, ErrPermission))
	// Output: true
}
```

## Review

The service is correct when every failure is inspectable two ways: by kind
through `errors.Is` against the sentinel, and by payload through `errors.As` into
`*ServiceError`. The load-bearing detail is that both must survive two wrap
layers — the outer `fmt.Errorf` and the `*ServiceError`'s own `Unwrap` — so if you
drop the `Unwrap` method or switch a `%w` to `%v`, `TestIsFindsSentinels` fails
immediately. The custom `Is` method is the subtle piece: it compares only `Op`
and returns false for any non-`*ServiceError` target, which is what lets
`errors.Is(err, &ServiceError{Op: "Get"})` mean "did this come from Get" without
disturbing sentinel matching. Do not compare with `==` and do not pass a
`ServiceError` value to `errors.As` — pass `&se` where `se` is `*ServiceError`.
Run `go test -race` to confirm the map-backed service is used correctly under the
tests.

## Resources

- [errors.Is](https://pkg.go.dev/errors#Is) — matching semantics and the custom `Is` method.
- [errors.As](https://pkg.go.dev/errors#As) — target shape rules and the custom `As` method.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `%w`, `Is`, `As`, and sentinels.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-http-boundary-error-mapping.md](02-http-boundary-error-mapping.md)
