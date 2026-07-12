# Exercise 1: A Contract-Tested User Repository

A repository is a collection-shaped interface that hides where data lives, plus at least one implementation and a test suite written against the interface rather than the implementation. This exercise builds all three: a `UserRepository` interface, an in-memory implementation that is safe for concurrent use, and a `runRepositoryContract` helper that any future backend can be held to.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
repository.go            User, domain sentinels, UserRepository interface,
                         MemoryUserRepository (RWMutex + byMail index), Count, Snapshot
cmd/
  demo/
    main.go              exercise the contract against the in-memory repo
repository_test.go       runRepositoryContract shared suite + accessor + email-reindex tests
```

- Files: `repository.go`, `cmd/demo/main.go`, `repository_test.go`.
- Implement: the `UserRepository` interface and `MemoryUserRepository` with `Create`, `GetByID`, `GetByEmail`, `Update`, `Delete`, `List`, plus the `Count` and `Snapshot` accessors.
- Test: `runRepositoryContract` exercises the full contract; additional tests cover the accessors and that `Update` keeps the email index in sync.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/05-repository-pattern/01-user-repository/cmd/demo && cd go-solutions/24-design-patterns-in-go/05-repository-pattern/01-user-repository
```

### Why an interface, two indexes, and domain sentinels

The interface is the contract the domain sees, and it is deliberately narrow: six methods that read like operations on a collection. `GetByEmail` is present alongside `GetByID` because email is the natural lookup key for a user and the domain genuinely needs it; everything else a caller might want (querying by other fields) belongs to the specification layer in the next exercise, not to a forest of `GetByX` methods here.

The in-memory implementation keeps two maps. `users` is keyed by id and is the source of truth. `byMail` maps an email to the id that owns it, which makes `GetByEmail` an O(1) lookup instead of a full scan and, more importantly, lets `Create` and `Update` enforce email uniqueness in constant time. The cost of a secondary index is that every mutation must keep it consistent with the primary: `Create` inserts into both, `Delete` removes from both, and `Update` is the subtle one — when an email changes it must delete the old `byMail` entry and add the new, and it must reject the change if the new email already belongs to someone else. The email-reindex test exists precisely because this is the step implementations get wrong.

The errors the interface exposes are domain sentinels, never storage types. `ErrNotFound`, `ErrDuplicateEmail`, and `ErrDuplicateID` are the complete vocabulary a caller must handle; `ErrEmptyID` and `ErrEmptyEmail` guard the inputs. A SQL-backed implementation of this same interface would catch its driver's no-rows and unique-violation errors and return these sentinels instead, so the domain code above it never changes. Note that a duplicate id and a duplicate email are distinct failures with distinct sentinels — collapsing both into one error, as a careless implementation does, makes a caller unable to tell which uniqueness constraint it violated.

Every method checks `ctx.Err()` at entry. For an in-memory map this rarely fires, but it keeps the implementation honest against the interface: a caller that passes an already-cancelled context gets a cancellation error, exactly as a database-backed implementation would produce when the query is abandoned. Mutating methods take `*User` so the timestamps they stamp are visible on the caller's own struct.

Reads return copies, not the stored pointers, and writes store copies. This is the snapshot discipline a real backend enforces for free: a SQL `GetByID` materializes a fresh struct from a row, so a caller mutating it cannot reach back into storage. Returning the live internal pointer would let a caller edit stored state without going through `Update` — and it would defeat `Update` itself, because `Update` detects an email change by comparing the incoming struct against the stored one; if they were the same pointer the comparison could never see a difference. Copying on read keeps the stored struct and the caller's struct distinct, which is exactly what the email-reindex test depends on.

Create `repository.go`:

```go
package repository

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// User is the domain entity the repository stores.
type User struct {
	ID        string
	Name      string
	Email     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Domain sentinels. These are the only errors a caller must handle; storage
// implementations map their backend-specific errors onto these.
var (
	ErrNotFound       = errors.New("repository: not found")
	ErrDuplicateEmail = errors.New("repository: duplicate email")
	ErrDuplicateID    = errors.New("repository: duplicate id")
	ErrEmptyID        = errors.New("repository: id is required")
	ErrEmptyEmail     = errors.New("repository: email is required")
	ErrNilUser        = errors.New("repository: nil user")
)

// UserRepository is the collection-shaped contract the domain depends on.
// Nothing in it mentions a storage mechanism.
type UserRepository interface {
	Create(ctx context.Context, user *User) error
	GetByID(ctx context.Context, id string) (*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
	Update(ctx context.Context, user *User) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) ([]*User, error)
}

// MemoryUserRepository is an in-memory UserRepository safe for concurrent use.
// users is keyed by id; byMail indexes email -> id so GetByEmail and the
// uniqueness checks are O(1).
type MemoryUserRepository struct {
	mu     sync.RWMutex
	users  map[string]*User
	byMail map[string]string
}

// NewMemoryUserRepository returns a ready-to-use in-memory repository.
func NewMemoryUserRepository() *MemoryUserRepository {
	return &MemoryUserRepository{
		users:  make(map[string]*User),
		byMail: make(map[string]string),
	}
}

func (r *MemoryUserRepository) Create(ctx context.Context, user *User) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if user == nil {
		return ErrNilUser
	}
	if user.ID == "" {
		return ErrEmptyID
	}
	if user.Email == "" {
		return ErrEmptyEmail
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.users[user.ID]; exists {
		return ErrDuplicateID
	}
	if _, exists := r.byMail[user.Email]; exists {
		return ErrDuplicateEmail
	}

	now := time.Now()
	user.CreatedAt = now
	user.UpdatedAt = now
	stored := *user
	r.users[user.ID] = &stored
	r.byMail[user.Email] = user.ID
	return nil
}

func (r *MemoryUserRepository) GetByID(ctx context.Context, id string) (*User, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *u
	return &cp, nil
}

func (r *MemoryUserRepository) GetByEmail(ctx context.Context, email string) (*User, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byMail[email]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *r.users[id]
	return &cp, nil
}

func (r *MemoryUserRepository) Update(ctx context.Context, user *User) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if user == nil {
		return ErrNilUser
	}
	if user.ID == "" {
		return ErrEmptyID
	}
	if user.Email == "" {
		return ErrEmptyEmail
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.users[user.ID]
	if !ok {
		return ErrNotFound
	}
	if existing.Email != user.Email {
		if _, taken := r.byMail[user.Email]; taken {
			return ErrDuplicateEmail
		}
		delete(r.byMail, existing.Email)
		r.byMail[user.Email] = user.ID
	}
	user.CreatedAt = existing.CreatedAt
	user.UpdatedAt = time.Now()
	stored := *user
	r.users[user.ID] = &stored
	return nil
}

func (r *MemoryUserRepository) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok {
		return ErrNotFound
	}
	delete(r.users, id)
	delete(r.byMail, u.Email)
	return nil
}

func (r *MemoryUserRepository) List(ctx context.Context) ([]*User, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshotLocked(), nil
}

// Count reports how many users are stored. Read-only accessor for tests/demos.
func (r *MemoryUserRepository) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.users)
}

// Snapshot returns every stored user sorted by id. Read-only accessor.
func (r *MemoryUserRepository) Snapshot() []*User {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshotLocked()
}

// snapshotLocked returns copies of every user sorted by id; the caller must
// hold at least the read lock.
func (r *MemoryUserRepository) snapshotLocked() []*User {
	out := make([]*User, 0, len(r.users))
	for _, u := range r.users {
		cp := *u
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
```

`MemoryUserRepository` satisfies `UserRepository`; the `var _ UserRepository = (*MemoryUserRepository)(nil)` assertion in the test pins that at compile time. `List` and `Snapshot` share `snapshotLocked` so the sort logic lives in one place; the two public methods differ only in the lock they take, which is why the shared helper is documented as requiring the caller to already hold the lock.

### The runnable demo

The demo exercises the whole contract through the interface type, never the concrete one — `demo` takes a `repository.UserRepository`, so the exact same function would drive a SQL-backed implementation. It creates two users, reads one back by email, updates a name, proves a duplicate email is rejected as a domain sentinel, and confirms a delete makes the user vanish.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/user-repository"
)

func demo(ctx context.Context, label string, repo repository.UserRepository) {
	fmt.Printf("--- %s ---\n", label)

	_ = repo.Create(ctx, &repository.User{ID: "1", Name: "Alice", Email: "alice@example.com"})
	_ = repo.Create(ctx, &repository.User{ID: "2", Name: "Bob", Email: "bob@example.com"})

	users, _ := repo.List(ctx)
	fmt.Printf("Total users: %d\n", len(users))

	u, _ := repo.GetByEmail(ctx, "alice@example.com")
	fmt.Printf("Found: %s <%s>\n", u.Name, u.Email)

	u.Name = "Alice Smith"
	_ = repo.Update(ctx, u)
	updated, _ := repo.GetByID(ctx, "1")
	fmt.Printf("Updated: %s\n", updated.Name)

	dup := repo.Create(ctx, &repository.User{ID: "3", Name: "Eve", Email: "alice@example.com"})
	fmt.Printf("Duplicate email rejected: %v\n", errors.Is(dup, repository.ErrDuplicateEmail))

	_ = repo.Delete(ctx, "2")
	_, missing := repo.GetByID(ctx, "2")
	fmt.Printf("After delete, GetByID not found: %v\n", errors.Is(missing, repository.ErrNotFound))
}

func main() {
	demo(context.Background(), "In-Memory Repository", repository.NewMemoryUserRepository())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
--- In-Memory Repository ---
Total users: 2
Found: Alice <alice@example.com>
Updated: Alice Smith
Duplicate email rejected: true
After delete, GetByID not found: true
```

### Tests

`runRepositoryContract` is the heart of the exercise: a single suite that takes any `UserRepository` and asserts every behavioral promise of the contract. A future SQL-backed implementation gets a three-line test that calls this helper and inherits all of it. `TestMemoryUserRepository_Accessors` covers `Count` and `Snapshot`, and `TestMemoryUserRepository_UpdateChangesEmail` pins the subtle email-reindex behavior: after changing a user's email, the new address resolves and the old one is gone.

Create `repository_test.go`:

```go
package repository

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

var _ UserRepository = (*MemoryUserRepository)(nil)

// runRepositoryContract exercises every UserRepository the same way.
func runRepositoryContract(t *testing.T, repo UserRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("CreateSetsTimestamps", func(t *testing.T) {
		u := &User{ID: "1", Name: "Alice", Email: "alice@example.com"}
		if err := repo.Create(ctx, u); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if u.CreatedAt.IsZero() || u.UpdatedAt.IsZero() {
			t.Errorf("timestamps not set: CreatedAt=%v UpdatedAt=%v", u.CreatedAt, u.UpdatedAt)
		}
	})

	t.Run("GetByIDAndByEmail", func(t *testing.T) {
		got, err := repo.GetByID(ctx, "1")
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.Email != "alice@example.com" {
			t.Errorf("Email = %q", got.Email)
		}
		got, err = repo.GetByEmail(ctx, "alice@example.com")
		if err != nil {
			t.Fatalf("GetByEmail: %v", err)
		}
		if got.ID != "1" {
			t.Errorf("ID = %q", got.ID)
		}
	})

	t.Run("CreateRejectsDuplicateEmail", func(t *testing.T) {
		err := repo.Create(ctx, &User{ID: "2", Name: "Eve", Email: "alice@example.com"})
		if !errors.Is(err, ErrDuplicateEmail) {
			t.Errorf("err = %v, want ErrDuplicateEmail", err)
		}
	})

	t.Run("CreateRejectsDuplicateID", func(t *testing.T) {
		err := repo.Create(ctx, &User{ID: "1", Name: "Clone", Email: "clone@example.com"})
		if !errors.Is(err, ErrDuplicateID) {
			t.Errorf("err = %v, want ErrDuplicateID", err)
		}
	})

	t.Run("CreateRejectsEmptyID", func(t *testing.T) {
		err := repo.Create(ctx, &User{Name: "X", Email: "x@example.com"})
		if !errors.Is(err, ErrEmptyID) {
			t.Errorf("err = %v, want ErrEmptyID", err)
		}
	})

	t.Run("GetMissingReturnsNotFound", func(t *testing.T) {
		_, err := repo.GetByID(ctx, "missing")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
		_, err = repo.GetByEmail(ctx, "missing@example.com")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("UpdateMutatesTimestamps", func(t *testing.T) {
		u, _ := repo.GetByID(ctx, "1")
		before := u.UpdatedAt
		time.Sleep(time.Millisecond)
		u.Name = "Alice Smith"
		if err := repo.Update(ctx, u); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, _ := repo.GetByID(ctx, "1")
		if got.Name != "Alice Smith" {
			t.Errorf("Name = %q", got.Name)
		}
		if !got.UpdatedAt.After(before) {
			t.Errorf("UpdatedAt did not advance: before=%v after=%v", before, got.UpdatedAt)
		}
	})

	t.Run("UpdateMissingReturnsNotFound", func(t *testing.T) {
		err := repo.Update(ctx, &User{ID: "ghost", Name: "G", Email: "g@example.com"})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("DeleteRemovesUser", func(t *testing.T) {
		if err := repo.Delete(ctx, "1"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		_, err := repo.GetByID(ctx, "1")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("after delete err = %v, want ErrNotFound", err)
		}
	})

	t.Run("ListIsSortedByID", func(t *testing.T) {
		_ = repo.Create(ctx, &User{ID: "b", Name: "B", Email: "b@example.com"})
		_ = repo.Create(ctx, &User{ID: "a", Name: "A", Email: "a@example.com"})
		all, err := repo.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		ids := make([]string, len(all))
		for i, u := range all {
			ids[i] = u.ID
		}
		sorted := append([]string(nil), ids...)
		sort.Strings(sorted)
		for i := range ids {
			if ids[i] != sorted[i] {
				t.Fatalf("List order not sorted: %v", ids)
			}
		}
	})
}

func TestMemoryUserRepository_Contract(t *testing.T) {
	t.Parallel()
	runRepositoryContract(t, NewMemoryUserRepository())
}

func TestMemoryUserRepository_Accessors(t *testing.T) {
	t.Parallel()
	repo := NewMemoryUserRepository()
	ctx := context.Background()

	_ = repo.Create(ctx, &User{ID: "1", Name: "Alice", Email: "alice@example.com"})
	_ = repo.Create(ctx, &User{ID: "2", Name: "Bob", Email: "bob@example.com"})

	if got := repo.Count(); got != 2 {
		t.Errorf("Count = %d, want 2", got)
	}
	snap := repo.Snapshot()
	if len(snap) != 2 || snap[0].ID != "1" || snap[1].ID != "2" {
		t.Errorf("Snapshot = %+v, want ids [1 2]", snap)
	}
}

func TestMemoryUserRepository_UpdateChangesEmail(t *testing.T) {
	t.Parallel()
	repo := NewMemoryUserRepository()
	ctx := context.Background()

	_ = repo.Create(ctx, &User{ID: "1", Name: "Alice", Email: "old@example.com"})

	u, _ := repo.GetByID(ctx, "1")
	u.Email = "new@example.com"
	if err := repo.Update(ctx, u); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := repo.GetByEmail(ctx, "new@example.com")
	if err != nil || got.ID != "1" {
		t.Fatalf("GetByEmail(new) = %v, %v; want user 1", got, err)
	}
	if _, err := repo.GetByEmail(ctx, "old@example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByEmail(old) err = %v, want ErrNotFound", err)
	}
}
```

## Review

The implementation is correct when the secondary index never lies. The email-reindex test is the one that catches the common defect: an `Update` that writes the new email into `users` but forgets to move the `byMail` entry leaves `GetByEmail(newEmail)` returning `ErrNotFound` and `GetByEmail(oldEmail)` returning the wrong user. Confirm that `Create` distinguishes a duplicate id (`ErrDuplicateID`) from a duplicate email (`ErrDuplicateEmail`) rather than collapsing both into one error, that mutating methods take `*User` so the timestamps they stamp are visible to the caller, and that every method honors a cancelled context at entry.

Common mistakes for this feature. The first is forgetting to keep `byMail` consistent on `Update` and `Delete`, which the reindex test and the contract's delete case together pin down. The second is taking `User` by value in `Create`, which makes the timestamp assignment invisible — the contract's `CreateSetsTimestamps` subtest fails immediately. The third is writing a separate, differently shaped test for each implementation instead of routing both through `runRepositoryContract`; the shared suite is what guarantees a second backend behaves identically. Running the whole thing under `go test -race ./...` also confirms the `RWMutex` actually guards the maps against concurrent access.

## Resources

- [Martin Fowler: Repository](https://martinfowler.com/eaaCatalog/repository.html) — the original catalog entry that defines the pattern as a collection-like interface over the data source.
- [`context` package](https://pkg.go.dev/context) — why every method takes a `context.Context` and how cancellation propagates.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `errors.Is`, `%w` wrapping, and why sentinels beat string matching.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-specification-queries.md](02-specification-queries.md)
