# Exercise 6: Enforce team boundaries with internal/ across the monorepo

A convention that says "please don't import our guts" is worth nothing under
deadline pressure. Go's `internal/` rule turns that boundary into a compiler
error. This exercise places a private package at `api/internal/store` and proves
the rule from both sides: the `api` service imports it and compiles, while the
worker importing it fails to build with "use of internal package not allowed".

## What you'll build

```text
mono/                         module: example.com/mono/api
  go.mod
  internal/
    store/
      store.go                package store: private in-memory UserStore
      store_test.go           unit test for the store itself
  service.go                  package api: uses internal/store (allowed)
  service_test.go             proves the allowed import path works
  cmd/
    demo/
      main.go                 exercises the service end to end
```

- Files: `internal/store/store.go`, `internal/store/store_test.go`, `service.go`, `service_test.go`, `cmd/demo/main.go`.
- Implement: a private `store.UserStore` (Put/Get) and a public `api.Service` that wraps it, so `api` legitimately imports `example.com/mono/api/internal/store`.
- Test: exercise `Service` through its public API (which reaches the internal store) and the store directly; a documented negative build proves an outside importer is rejected.
- Verify: `go build ./...` and `go test -race -count=1 ./...` succeed; the worker's cross-boundary import (shown below) fails to compile.

Set up the `api` module:

```bash
mkdir -p ~/go-exercises/api/internal/store ~/go-exercises/api/cmd/demo
cd ~/go-exercises/api
go mod init example.com/mono/api
```

### The rule, precisely

A package whose import path contains an `internal/` element is importable only by
code rooted at the directory that is the **parent** of that `internal/`. For
`example.com/mono/api/internal/store`, the parent is `example.com/mono/api`, so
only packages under `example.com/mono/api/...` may import it. The `api` service's
root package and its `cmd/` binaries qualify; the separate `worker` module does
not, and neither does any other service. The compiler enforces this — there is no
flag to turn it off and no lint to argue with.

This gives a monorepo two distinct scopes of privacy:

- A **repo-root** `internal/` (for example `example.com/mono/internal/telemetry`
  in a single-module repo) is private to the module but shared by every service
  in it — the place for cross-service platform code that must not escape the repo.
- A **service-local** `internal/` like `example.com/mono/api/internal/store` is
  private to that one service. It is the store's home precisely because no other
  team can reach into it: the `worker` cannot accidentally couple itself to the
  API's persistence details, because the import does not compile.

The design here puts the store — an implementation detail no other service should
touch — under `api/internal/store`, and exposes only a public `api.Service` that
mediates access. Callers get `Service`; the store stays sealed.

Create `internal/store/store.go`:

```go
package store

import "errors"

// ErrNotFound is returned when a user id is absent.
var ErrNotFound = errors.New("store: user not found")

// User is the persisted record.
type User struct {
	ID   string
	Name string
}

// UserStore is an in-memory user repository. It lives under internal/ so only
// the api service can import it; no other service can couple to it.
type UserStore struct {
	users map[string]User
}

// New returns an empty store.
func New() *UserStore {
	return &UserStore{users: make(map[string]User)}
}

// Put inserts or replaces a user.
func (s *UserStore) Put(u User) {
	s.users[u.ID] = u
}

// Get returns the user for id, or ErrNotFound.
func (s *UserStore) Get(id string) (User, error) {
	u, ok := s.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}
```

Create `internal/store/store_test.go`:

```go
package store

import (
	"errors"
	"testing"
)

func TestUserStore(t *testing.T) {
	t.Parallel()

	s := New()
	s.Put(User{ID: "1", Name: "alice"})

	got, err := s.Get("1")
	if err != nil {
		t.Fatalf("Get(1) error = %v", err)
	}
	if got.Name != "alice" {
		t.Errorf("Get(1).Name = %q, want alice", got.Name)
	}

	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
}
```

Create `service.go` — the public face that legitimately imports the internal
package (it is rooted at `example.com/mono/api`):

```go
package api

import (
	"fmt"

	"example.com/mono/api/internal/store"
)

// Service is the public API of the api module. It mediates all access to the
// private store; callers never touch internal/store directly.
type Service struct {
	users *store.UserStore
}

// NewService builds a Service over a fresh store.
func NewService() *Service {
	return &Service{users: store.New()}
}

// Register adds a user.
func (s *Service) Register(id, name string) {
	s.users.Put(store.User{ID: id, Name: name})
}

// DisplayName returns a formatted name for id, or an error if absent.
func (s *Service) DisplayName(id string) (string, error) {
	u, err := s.users.Get(id)
	if err != nil {
		return "", fmt.Errorf("display name for %s: %w", id, err)
	}
	return "user:" + u.Name, nil
}
```

### The negative side of the boundary

The rule only means something if the other side really fails. In the real
monorepo the worker is a separate module; a file in it that tried to reach the
API's private store:

```go
// worker/main.go — DOES NOT COMPILE
package main

import "example.com/mono/api/internal/store" // use of internal package not allowed

func main() {
	_ = store.New()
}
```

fails at build time:

```text
go build ./...
main.go:3:8: use of internal package example.com/mono/api/internal/store not allowed
```

You can reproduce that rejection deliberately as a boundary check in CI: run
`go build` on a throwaway package outside `api/` that imports the internal path
and assert the command exits non-zero with that message. The positive proof is
this module compiling and testing green; the negative proof is that build error.

Create `service_test.go`:

```go
package api

import (
	"errors"
	"testing"

	"example.com/mono/api/internal/store"
)

func TestServiceUsesInternalStore(t *testing.T) {
	t.Parallel()

	svc := NewService()
	svc.Register("1", "alice")

	name, err := svc.DisplayName("1")
	if err != nil {
		t.Fatalf("DisplayName(1) error = %v", err)
	}
	if name != "user:alice" {
		t.Errorf("DisplayName(1) = %q, want user:alice", name)
	}

	// The api package is rooted at example.com/mono/api, so it may import the
	// internal store directly. A missing user surfaces the store's sentinel.
	_, err = svc.DisplayName("missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DisplayName(missing) error = %v, want store.ErrNotFound", err)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mono/api"
)

func main() {
	svc := api.NewService()
	svc.Register("1", "alice")

	name, err := svc.DisplayName("1")
	fmt.Printf("hit:  name=%q err=%v\n", name, err)

	name, err = svc.DisplayName("999")
	fmt.Printf("miss: name=%q err=%v\n", name, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hit:  name="user:alice" err=<nil>
miss: name="" err=display name for 999: store: user not found
```

## Review

The boundary is correct when the allowed side compiles and the forbidden side does
not. `api` imports `example.com/mono/api/internal/store` and both its tests pass,
because `api` is rooted at the internal package's parent. The worker's import of
the same path fails with "use of internal package not allowed" — that build error
is the boundary, enforced by the compiler rather than by review.

The mistake to avoid is assuming a service-local `internal/` is reachable from
elsewhere "because it's the same repo". The rule keys on the *parent of
`internal/`*, not on the repository: `api/internal/store` is private to `api`,
full stop. When code genuinely must be shared across services, do not widen the
service-local internal — promote it to a repo-root `internal/` (shared within the
module) or to a public package with a real API. Keep the sealed thing sealed and
export a mediator like `Service`.

## Resources

- [Go Modules Reference: internal packages](https://go.dev/ref/mod#internal-packages) — the exact import restriction rule.
- [`go help packages`](https://pkg.go.dev/cmd/go#hdr-Package_lists_and_patterns) — how `internal/` interacts with package patterns.
- [Organizing a Go module](https://go.dev/doc/modules/layout) — where `internal/` fits in module layout.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-guard-against-leaked-replace.md](07-guard-against-leaked-replace.md)
