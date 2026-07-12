# Exercise 8: Seed a repository from a testdata fixture set

Query-layer tests need known data to query against. Hand-inserting rows in every test is noise; the clean form loads a fixed dataset from `testdata/seed.json` into a fresh repository, then exercises the query methods against that known state. This module seeds an in-memory user repository and tests `FindByStatus` and paging.

## What you'll build

```text
repofixture/                  independent module: example.com/repofixture
  go.mod                      go 1.26
  repo.go                     Repository; Insert, FindByStatus, Page, Count, Reset; LoadSeedBytes
  cmd/
    demo/
      main.go                 seeds from an inline dataset and prints counts
  repo_test.go                loadSeed(t) with t.Cleanup; query assertions; SaveTo via t.TempDir
  testdata/
    seed.json                 the known dataset
```

Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`, `testdata/seed.json`.
Implement: an in-memory `Repository` with `FindByStatus` (sorted), `Page(offset, limit)`, `Count`, `Reset`, and `LoadSeedBytes` decoding a `[]User`.
Test: `loadSeed(t)` reads `testdata/seed.json` into a fresh repo and registers `t.Cleanup(repo.Reset)`; query tests assert result-set IDs; a save/reload test uses `t.TempDir` for a file-backed snapshot.
Verify: `go test -count=1 -race ./...`

### Seed data as the source of truth for query tests

A query method is only as testable as the data behind it. If each test inserts its own ad-hoc rows, the dataset is scattered across the file and no single place tells you what the repository contains — and two tests that each insert "a couple of active users" will disagree on the counts. A seed fixture fixes that: `testdata/seed.json` is the canonical dataset, reviewed and versioned like any other fixture, and every query test asserts against that one known state. `FindByStatus("active")` has a definite, checkable answer because the seed defines exactly which users are active.

The isolation rule is: every test gets a fresh repository. `loadSeed(t)` constructs a new repository, loads the seed, and registers `t.Cleanup(repo.Reset)` so the state is torn down when the test ends. Because each test builds its own repository, they cannot leak state into each other even when run in parallel — the `t.Cleanup` here is as much documentation of the reset discipline as it is mechanically necessary for an in-memory store. The moment a store is *file-backed*, the discipline stops being optional: a test that writes to disk must write somewhere private, and `t.TempDir` gives each test a unique directory that the framework removes afterward, so a snapshot from one test never bleeds into another. The save/reload test below uses exactly that.

The repository keeps its query results deterministic by sorting on ID before returning, so `FindByStatus` and `Page` produce a stable order regardless of Go's randomized map iteration. Without that sort, a paging assertion would flake — the data-determinism lesson from the golden exercises applies just as much to query results as to serialized output.

Create `repo.go`:

```go
package repofixture

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
)

// User is a stored record.
type User struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Repository is a concurrency-safe in-memory user store.
type Repository struct {
	mu    sync.RWMutex
	users map[string]User
}

// New returns an empty repository.
func New() *Repository {
	return &Repository{users: make(map[string]User)}
}

// LoadSeedBytes decodes a JSON array of users into a fresh repository.
func LoadSeedBytes(data []byte) (*Repository, error) {
	var users []User
	if err := json.Unmarshal(data, &users); err != nil {
		return nil, fmt.Errorf("decode seed: %w", err)
	}
	r := New()
	for _, u := range users {
		r.Insert(u)
	}
	return r, nil
}

// Insert stores or replaces a user by ID.
func (r *Repository) Insert(u User) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[u.ID] = u
}

// Reset clears all stored users.
func (r *Repository) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users = make(map[string]User)
}

// Count reports the number of stored users.
func (r *Repository) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.users)
}

// all returns every user sorted by ID for a deterministic order.
func (r *Repository) all() []User {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]User, 0, len(r.users))
	for _, u := range r.users {
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b User) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// FindByStatus returns all users with the given status, sorted by ID.
func (r *Repository) FindByStatus(status string) []User {
	var out []User
	for _, u := range r.all() {
		if u.Status == status {
			out = append(out, u)
		}
	}
	return out
}

// Page returns a window of users sorted by ID. An offset past the end returns nil.
func (r *Repository) Page(offset, limit int) []User {
	all := r.all()
	if offset < 0 || offset >= len(all) {
		return nil
	}
	end := min(offset+limit, len(all))
	return all[offset:end]
}

// SaveTo writes the repository to a JSON file.
func (r *Repository) SaveTo(path string) error {
	data, err := json.MarshalIndent(r.all(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// LoadFrom reads a repository from a JSON file.
func LoadFrom(path string) (*Repository, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadSeedBytes(data)
}
```

Now the seed dataset. This is the source of truth: three active users, two suspended.

Create `testdata/seed.json`:

```json
[
  {"id": "u1", "name": "Alice", "status": "active"},
  {"id": "u2", "name": "Bob", "status": "suspended"},
  {"id": "u3", "name": "Carol", "status": "active"},
  {"id": "u4", "name": "Dave", "status": "active"},
  {"id": "u5", "name": "Eve", "status": "suspended"}
]
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/repofixture"
)

func main() {
	repo, err := repofixture.LoadSeedBytes([]byte(`[
		{"id":"u1","name":"Alice","status":"active"},
		{"id":"u2","name":"Bob","status":"suspended"}
	]`))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("total=%d active=%d\n", repo.Count(), len(repo.FindByStatus("active")))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total=2 active=1
```

### The test

`loadSeed` centralizes the read-and-seed-and-cleanup logic. The query tests assert result-set IDs (via a small `ids` extractor) against the known seed, using `slices.Equal` for an order-sensitive comparison. The save/reload test writes a snapshot into `t.TempDir()` and reloads it, proving persistence without leaking a file between tests.

Create `repo_test.go`:

```go
package repofixture

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func loadSeed(t *testing.T) *Repository {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "seed.json"))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	repo, err := LoadSeedBytes(data)
	if err != nil {
		t.Fatalf("load seed: %v", err)
	}
	t.Cleanup(repo.Reset)
	return repo
}

func ids(us []User) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.ID
	}
	return out
}

func TestFindByStatus(t *testing.T) {
	t.Parallel()
	repo := loadSeed(t)

	if got, want := ids(repo.FindByStatus("active")), []string{"u1", "u3", "u4"}; !slices.Equal(got, want) {
		t.Fatalf("active = %v, want %v", got, want)
	}
	if got, want := ids(repo.FindByStatus("suspended")), []string{"u2", "u5"}; !slices.Equal(got, want) {
		t.Fatalf("suspended = %v, want %v", got, want)
	}
}

func TestPaging(t *testing.T) {
	t.Parallel()
	repo := loadSeed(t)

	if got, want := ids(repo.Page(0, 2)), []string{"u1", "u2"}; !slices.Equal(got, want) {
		t.Fatalf("page(0,2) = %v, want %v", got, want)
	}
	if got, want := ids(repo.Page(2, 2)), []string{"u3", "u4"}; !slices.Equal(got, want) {
		t.Fatalf("page(2,2) = %v, want %v", got, want)
	}
	if got := repo.Page(10, 5); len(got) != 0 {
		t.Fatalf("page past end = %v, want empty", ids(got))
	}
}

func TestSaveReloadRoundtrips(t *testing.T) {
	t.Parallel()
	repo := loadSeed(t)

	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := repo.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	reloaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if reloaded.Count() != repo.Count() {
		t.Fatalf("reloaded count = %d, want %d", reloaded.Count(), repo.Count())
	}
}

func ExampleRepository_FindByStatus() {
	repo, _ := LoadSeedBytes([]byte(`[{"id":"a","name":"A","status":"active"}]`))
	fmt.Println(len(repo.FindByStatus("active")))
	// Output: 1
}
```

## Review

The query tests are trustworthy when they run against a known seed, each in a fresh repository, with results in a deterministic order. The habits: keep the dataset in `testdata/seed.json` as the reviewed source of truth rather than scattering inserts; build a fresh repository per test and register `t.Cleanup` for teardown; sort query results so paging assertions do not flake on map order; and use `t.TempDir` for any file-backed state so nothing leaks between tests. A query test that shares mutable state with its neighbors is a flake waiting to happen; a per-test seed plus cleanup is the antidote.

## Resources

- [testing: T.Cleanup](https://pkg.go.dev/testing#T.Cleanup) — registering teardown that runs when the test finishes.
- [testing: T.TempDir](https://pkg.go.dev/testing#T.TempDir) — a per-test directory removed automatically.
- [slices: SortFunc and Equal](https://pkg.go.dev/slices#SortFunc) — deterministic ordering and order-sensitive comparison.

---

Back to [07-test-data-builder.md](07-test-data-builder.md) | Next: [09-per-case-directories.md](09-per-case-directories.md)
