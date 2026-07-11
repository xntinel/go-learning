# Exercise 10: Guard Against Use of an Un-Constructed Type Instead of Trusting the Zero Value

The zero value of every type is reachable without your constructor, so you must
decide what it means. This exercise builds both deliberate answers side by side: a
`Repository` whose methods fail loudly with `ErrNotInitialized` when called on a
zero value that skipped `New`, and a `LazyStore` whose zero value is genuinely
useful because it lazily initializes on first use — the same design distinction
`sync.Mutex` and `bytes.Buffer` embody.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
guardedrepo/                 independent module: example.com/must-be-constructed-guard
  go.mod
  repo.go                    Repository (initialized guard) + LazyStore (useful zero value), sentinels
  cmd/
    demo/
      main.go                a constructed repo, a zero-value repo, and a zero-value LazyStore
  repo_test.go               constructed serves, zero repo errors on every method, New validates, LazyStore zero works
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: `NewRepository(dsn string) (*Repository, error)` with an unexported `initialized` flag guarding `Get`/`Put`/`Close`, plus a `LazyStore` whose zero value works via `sync.Once`.
- Test: a constructed repo serves calls; a zero-value repo returns `ErrNotInitialized` via `errors.Is` on every method; `New` with an invalid DSN returns a validation sentinel and no half-live object; and the zero-value `LazyStore` works without a constructor.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/guardedrepo/cmd/demo
cd ~/go-exercises/guardedrepo
go mod init example.com/must-be-constructed-guard
```

### Two honest answers, one dishonest one

A type whose internal map is nil until `New` runs has a zero value that will
nil-panic on first use — deep inside a handler, three frames from the missing
constructor call, with a stack trace that points at the symptom rather than the
cause. There are two ways to make that honest. The first is to guard: keep an
unexported `initialized bool` set only by `New`, and have every exported method
check it first and return a typed `ErrNotInitialized`. Now a skipped constructor
surfaces as a clean, attributable error at the boundary — the caller learns
immediately that it used a zero value, not a crash later. The second is to make
the zero value genuinely useful, the way `sync.Mutex`, `bytes.Buffer`, and
`strings.Builder` do: design the type so no construction is needed at all, lazily
allocating any internal state on first use. `LazyStore` does this with `sync.Once`,
which runs the one-time initialization exactly once even under concurrent access.

The dishonest answer is the accidental middle: a zero value where some methods
work and one panics on the nil map. That turns a construction bug into a crash
whose cause is invisible. The design rule is to pick one of the two honest
answers deliberately and encode the choice — a guard flag for "must be
constructed", lazy init for "zero value is fine" — never to leave it to chance.
`New` still validates its argument and returns the zero-plus-error on failure, so
a bad DSN never yields a half-live repository.

Create `repo.go`:

```go
package guardedrepo

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

var (
	ErrNotInitialized = errors.New("repository not initialized: use NewRepository")
	ErrInvalidDSN     = errors.New("dsn must not be empty")
	ErrNotFound       = errors.New("key not found")
)

// Repository requires construction: its zero value fails loudly rather than
// nil-panicking, because an un-constructed repository has no store to serve.
type Repository struct {
	initialized bool
	mu          sync.Mutex
	data        map[string]string
}

// NewRepository validates the DSN and returns a ready Repository, or the zero
// value and a sentinel on failure.
func NewRepository(dsn string) (*Repository, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("%w: %q", ErrInvalidDSN, dsn)
	}
	return &Repository{
		initialized: true,
		data:        make(map[string]string),
	}, nil
}

// Put stores value under key, or ErrNotInitialized on a zero-value Repository.
func (r *Repository) Put(key, value string) error {
	if !r.initialized {
		return ErrNotInitialized
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data[key] = value
	return nil
}

// Get returns the value for key, ErrNotFound if absent, or ErrNotInitialized on
// a zero-value Repository.
func (r *Repository) Get(key string) (string, error) {
	if !r.initialized {
		return "", ErrNotInitialized
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.data[key]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNotFound, key)
	}
	return v, nil
}

// Close releases the repository, or ErrNotInitialized on a zero-value one.
func (r *Repository) Close() error {
	if !r.initialized {
		return ErrNotInitialized
	}
	return nil
}

// LazyStore contrasts with Repository: its zero value is useful. It allocates
// its map on first use via sync.Once, so no constructor is required, the way
// sync.Mutex and bytes.Buffer are usable at their zero value.
type LazyStore struct {
	once sync.Once
	mu   sync.Mutex
	data map[string]string
}

func (s *LazyStore) init() {
	s.data = make(map[string]string)
}

// Set stores value under key. It works on a zero-value LazyStore.
func (s *LazyStore) Set(key, value string) {
	s.once.Do(s.init)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

// Get returns the value and whether it was present. It works on a zero-value
// LazyStore.
func (s *LazyStore) Get(key string) (string, bool) {
	s.once.Do(s.init)
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}
```

### The runnable demo

The demo uses a constructed repository, shows a zero-value repository returning a
clean error instead of panicking, and shows a zero-value `LazyStore` working with
no constructor.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/must-be-constructed-guard"
)

func main() {
	repo, _ := guardedrepo.NewRepository("postgres://localhost/app")
	_ = repo.Put("user:1", "alice")
	v, _ := repo.Get("user:1")
	fmt.Printf("constructed repo: user:1=%s\n", v)

	var zero guardedrepo.Repository
	fmt.Printf("zero repo Put err: %v\n", zero.Put("k", "v"))

	var store guardedrepo.LazyStore
	store.Set("k", "v")
	got, ok := store.Get("k")
	fmt.Printf("zero LazyStore: k=%s present=%t\n", got, ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
constructed repo: user:1=alice
zero repo Put err: repository not initialized: use NewRepository
zero LazyStore: k=v present=true
```

### Tests

`TestZeroRepoErrorsEverywhere` is the guard's proof: every exported method on a
zero-value `Repository` returns `ErrNotInitialized` rather than nil-panicking.
`TestLazyStoreZeroWorks` is the contrast: the zero-value `LazyStore` serves calls
with no constructor, making the design distinction explicit.

Create `repo_test.go`:

```go
package guardedrepo

import (
	"errors"
	"fmt"
	"testing"
)

func TestConstructedServes(t *testing.T) {
	t.Parallel()
	r, err := NewRepository("dsn")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Put("k", "v"); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get("k")
	if err != nil || got != "v" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if _, err := r.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key err = %v, want ErrNotFound", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestZeroRepoErrorsEverywhere(t *testing.T) {
	t.Parallel()
	var r Repository
	if err := r.Put("k", "v"); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Put err = %v, want ErrNotInitialized", err)
	}
	if _, err := r.Get("k"); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Get err = %v, want ErrNotInitialized", err)
	}
	if err := r.Close(); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Close err = %v, want ErrNotInitialized", err)
	}
}

func TestNewValidatesDSN(t *testing.T) {
	t.Parallel()
	r, err := NewRepository("   ")
	if !errors.Is(err, ErrInvalidDSN) {
		t.Fatalf("err = %v, want ErrInvalidDSN", err)
	}
	if r != nil {
		t.Fatal("no repository should be returned on validation failure")
	}
}

func TestLazyStoreZeroWorks(t *testing.T) {
	t.Parallel()
	var s LazyStore // no constructor
	s.Set("k", "v")
	got, ok := s.Get("k")
	if !ok || got != "v" {
		t.Fatalf("zero LazyStore Get = %q, %t; want v, true", got, ok)
	}
	if _, ok := s.Get("absent"); ok {
		t.Fatal("absent key reported present")
	}
}

func ExampleRepository() {
	var zero Repository
	fmt.Println(zero.Put("k", "v"))
	// Output: repository not initialized: use NewRepository
}
```

## Review

The two types are correct when the guarded `Repository` serves calls after `New`
and returns `ErrNotInitialized` on every method of a zero value, while the
`LazyStore` serves calls at its zero value with no constructor. The lesson is that
the zero value is a design decision you make on purpose: guard it when construction
is mandatory, make it useful when it is not, and never leave the half-working
middle where one method nil-panics. `New` validating the DSN and returning
nil-plus-error keeps a bad argument from producing a half-live object.

## Resources

- [sync.Once](https://pkg.go.dev/sync#Once) — the one-time lazy initialization behind `LazyStore`.
- [bytes.Buffer](https://pkg.go.dev/bytes#Buffer) — the canonical useful-zero-value type.
- [Go wiki: zero values](https://go.dev/wiki/CodeReviewComments#useful-zero-values) — when to make the zero value work.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-normalize-endpoint-canonical.md](09-normalize-endpoint-canonical.md) | Next: [../07-method-sets-and-addressability/00-concepts.md](../07-method-sets-and-addressability/00-concepts.md)
