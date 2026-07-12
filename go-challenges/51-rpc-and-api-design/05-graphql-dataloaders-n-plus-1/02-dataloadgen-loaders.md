# Exercise 2: Production Loaders with dataloadgen

You built the mechanism by hand; in production you use a maintained library.
`github.com/vikstrous/dataloadgen` is the generics-native dataloader most gqlgen
services reach for. This exercise builds the loader layer a real service ships: a
positional loader over a batched repository fetch, a mapped variant that handles
missing rows safely, and a no-cache variant for mutation paths.

This module is self-contained and imports only `dataloadgen`. Nothing here
imports another exercise.

## What you'll build

```text
userloader/                  independent module: example.com/userloader
  go.mod                     go 1.26; require github.com/vikstrous/dataloadgen v0.0.10
  users.go                   User; Repo (in-memory, round-trip counter)
                             getUsers (positional fetch), getUsersMapped (mapped fetch)
  loaders.go                 NewUserLoader, NewMappedUserLoader, NewMutationUserLoader
  cmd/
    demo/
      main.go                runnable demo: LoadAll with a duplicate, mapped ErrNotFound
  users_test.go              fetch alignment, missing-key handling, per-key errors,
                             round-trip counting, an Example
```

- Files: `users.go`, `loaders.go`, `cmd/demo/main.go`, `users_test.go`.
- Implement: a `Repo` whose `getUsers` does one round-trip returning values aligned by position with the requested ids, and `getUsersMapped` returning a `map[int]User`; loader constructors wiring both through `dataloadgen.NewLoader`/`NewMappedLoader` with `WithWait`, `WithBatchCapacity`, and `WithoutCache`.
- Test: assert positional alignment and per-key error partitioning of `getUsers`, that the mapped loader turns a missing id into `dataloadgen.ErrNotFound` (via `errors.Is`), and that a `LoadAll` over many ids costs exactly one repo round-trip.
- Verify: `go test -count=1 -race ./...` (this module pulls an external dependency; fetch it with `GOFLAGS=-mod=mod`).

Set up the module:

```bash
mkdir -p go-solutions/51-rpc-and-api-design/05-graphql-dataloaders-n-plus-1/02-dataloadgen-loaders/cmd/demo
cd go-solutions/51-rpc-and-api-design/05-graphql-dataloaders-n-plus-1/02-dataloadgen-loaders
go mod edit -go=1.26
go get github.com/vikstrous/dataloadgen@v0.0.10
```

### The two fetch shapes, and why mapped is safer

`dataloadgen` offers two constructors, and the difference is the fetch signature.
`NewLoader` takes a *positional* fetch, `func(ctx, keys []K) ([]V, []error)`:
you must return a `[]V` the same length as `keys`, aligned by index, plus a
parallel `[]error`. `NewMappedLoader` takes a *mapped* fetch,
`func(ctx, keys []K) (map[K]V, error)`: you return a map keyed by id and the
loader looks each key up itself, resolving any key absent from the map to
`dataloadgen.ErrNotFound`.

The positional form is a trap in exactly the way a real datastore makes it one.
`SELECT * FROM users WHERE id IN (3,1,2)` returns rows in the engine's order, not
your key order, and if id 1 does not exist it returns two rows for three keys.
Handing those rows back positionally shifts every value onto the wrong key —
silent corruption. To use `NewLoader` correctly you must re-index the rows by id
and fill each slot yourself, which is precisely what the mapped loader does for
you. Prefer `NewMappedLoader` unless you have a concrete reason to manage
alignment by hand; this exercise shows both so the contrast is explicit.

### Tuning: wait, capacity, and no-cache

Three options carry the trade-offs from the concepts file.
`WithWait(time.Millisecond)` keeps the batch window short so latency stays low
while still coalescing the concurrent sibling resolvers gqlgen dispatches.
`WithBatchCapacity(100)` caps fan-out so a query over thousands of parents does
not build one enormous `IN (...)`; the loader splits into capacity-sized batches
instead. `WithoutCache()` builds a loader that never caches results, for mutation
paths where reading a stale pre-write value in the same request would be a bug —
the alternative to sprinkling `Clear`/`Prime` calls after every write.

Create `users.go`:

```go
package userloader

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

// User is the entity the loader fetches.
type User struct {
	ID    int
	Name  string
	Email string
}

// ErrUserNotFound is the sentinel a positional fetch wraps for a missing id.
var ErrUserNotFound = errors.New("userloader: user not found")

// Repo is an in-memory stand-in for a database. It counts round-trips so tests
// can prove batching collapses N lookups into one.
type Repo struct {
	users map[int]User
	calls atomic.Int64
}

// NewRepo builds a Repo seeded with users.
func NewRepo(users ...User) *Repo {
	m := make(map[int]User, len(users))
	for _, u := range users {
		m[u.ID] = u
	}
	return &Repo{users: m}
}

// Calls reports how many batch round-trips have run.
func (r *Repo) Calls() int64 { return r.calls.Load() }

// getUsers is the positional batch fetch: one round-trip (simulating
// SELECT ... WHERE id IN (...)) returning values aligned by position with ids.
// A missing id occupies its slot with the zero User and a wrapped error, so the
// slices never shift.
func (r *Repo) getUsers(ctx context.Context, ids []int) ([]User, []error) {
	r.calls.Add(1)
	values := make([]User, len(ids))
	var errs []error
	for i, id := range ids {
		u, ok := r.users[id]
		if !ok {
			if errs == nil {
				errs = make([]error, len(ids))
			}
			errs[i] = fmt.Errorf("id %d: %w", id, ErrUserNotFound)
			continue
		}
		values[i] = u
	}
	return values, errs
}

// getUsersMapped is the mapped batch fetch: one round-trip returning a map keyed
// by id. Ids with no row are simply absent; dataloadgen turns those into
// ErrNotFound, so there is no alignment to get wrong.
func (r *Repo) getUsersMapped(ctx context.Context, ids []int) (map[int]User, error) {
	r.calls.Add(1)
	out := make(map[int]User, len(ids))
	for _, id := range ids {
		if u, ok := r.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}
```

Create `loaders.go`:

```go
package userloader

import (
	"time"

	"github.com/vikstrous/dataloadgen"
)

// NewUserLoader wires the positional fetch through dataloadgen with a short
// window and a fan-out cap.
func NewUserLoader(r *Repo) *dataloadgen.Loader[int, User] {
	return dataloadgen.NewLoader(
		r.getUsers,
		dataloadgen.WithWait(time.Millisecond),
		dataloadgen.WithBatchCapacity(100),
	)
}

// NewMappedUserLoader wires the mapped fetch; missing ids resolve to
// dataloadgen.ErrNotFound instead of shifting the mapping.
func NewMappedUserLoader(r *Repo) *dataloadgen.Loader[int, User] {
	return dataloadgen.NewMappedLoader(
		r.getUsersMapped,
		dataloadgen.WithWait(time.Millisecond),
	)
}

// NewMutationUserLoader builds a loader that never caches, for a request that
// writes then reads the same id and must not see the stale cached value.
func NewMutationUserLoader(r *Repo) *dataloadgen.Loader[int, User] {
	return dataloadgen.NewLoader(r.getUsers, dataloadgen.WithoutCache())
}
```

### The runnable demo

The demo loads three ids where one is a duplicate (proving single-flight collapses
it), prints the round-trip count, then asks the mapped loader for a missing id to
show `ErrNotFound`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/vikstrous/dataloadgen"

	"example.com/userloader"
)

func main() {
	repo := userloader.NewRepo(
		userloader.User{ID: 1, Name: "Alice", Email: "alice@example.com"},
		userloader.User{ID: 2, Name: "Bob", Email: "bob@example.com"},
	)
	loader := userloader.NewUserLoader(repo)
	ctx := context.Background()

	users, err := loader.LoadAll(ctx, []int{1, 2, 1})
	if err != nil {
		fmt.Println("load error:", err)
	}
	for _, u := range users {
		fmt.Printf("%d=%s\n", u.ID, u.Name)
	}
	fmt.Println("round-trips:", repo.Calls())

	mapped := userloader.NewMappedUserLoader(repo)
	if _, err := mapped.Load(ctx, 999); err != nil {
		fmt.Println("missing is ErrNotFound:", errors.Is(err, dataloadgen.ErrNotFound))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
1=Alice
2=Bob
1=Alice
round-trips: 1
missing is ErrNotFound: true
```

### Tests

The fetches are pure functions over the in-memory `Repo`, so their contract is
directly testable: `TestGetUsersAlignment` checks positional alignment under a
shuffled id order, `TestGetUsersMissingKeyWrapsSentinel` checks that a missing id
lands a wrapped `ErrUserNotFound` in its own slot without disturbing siblings.
`TestMappedLoaderErrNotFound` drives the mapped loader and asserts a missing id
surfaces `dataloadgen.ErrNotFound` via `errors.Is`. `TestLoaderBatchesRoundTrips`
drives the positional loader with `LoadAll` over many ids and asserts the repo saw
exactly one round-trip.

Create `users_test.go`:

```go
package userloader

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/vikstrous/dataloadgen"
)

func seededRepo() *Repo {
	return NewRepo(
		User{ID: 1, Name: "Alice"},
		User{ID: 2, Name: "Bob"},
		User{ID: 3, Name: "Carol"},
	)
}

func TestGetUsersAlignment(t *testing.T) {
	t.Parallel()
	r := seededRepo()
	ids := []int{3, 1, 2}
	got, errs := r.getUsers(context.Background(), ids)
	if errs != nil {
		t.Fatalf("unexpected errors: %v", errs)
	}
	want := []string{"Carol", "Alice", "Bob"}
	for i := range ids {
		if got[i].Name != want[i] {
			t.Errorf("value[%d] = %q; want %q", i, got[i].Name, want[i])
		}
	}
}

func TestGetUsersMissingKeyWrapsSentinel(t *testing.T) {
	t.Parallel()
	r := seededRepo()
	ids := []int{1, 9, 2}
	got, errs := r.getUsers(context.Background(), ids)
	if len(errs) != len(ids) {
		t.Fatalf("errs len = %d; want %d aligned with keys", len(errs), len(ids))
	}
	if !errors.Is(errs[1], ErrUserNotFound) {
		t.Errorf("errs[1] = %v; want wrapped ErrUserNotFound", errs[1])
	}
	if errs[0] != nil || errs[2] != nil {
		t.Errorf("siblings poisoned: errs = %v", errs)
	}
	if got[0].Name != "Alice" || got[2].Name != "Bob" {
		t.Errorf("present ids misaligned: %v", got)
	}
}

func TestMappedLoaderErrNotFound(t *testing.T) {
	t.Parallel()
	r := seededRepo()
	loader := NewMappedUserLoader(r)
	ctx := context.Background()

	if _, err := loader.Load(ctx, 999); !errors.Is(err, dataloadgen.ErrNotFound) {
		t.Fatalf("Load(999) err = %v; want dataloadgen.ErrNotFound", err)
	}
	if u, err := loader.Load(ctx, 1); err != nil || u.Name != "Alice" {
		t.Fatalf("Load(1) = %v, %v; want Alice, nil", u, err)
	}
}

func TestLoaderBatchesRoundTrips(t *testing.T) {
	t.Parallel()
	r := seededRepo()
	loader := NewUserLoader(r)
	ctx := context.Background()

	got, err := loader.LoadAll(ctx, []int{1, 2, 3, 1, 2})
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d values; want 5", len(got))
	}
	if r.Calls() != 1 {
		t.Errorf("repo round-trips = %d; want 1 (all ids batched)", r.Calls())
	}
}

func ExampleNewMappedUserLoader() {
	r := NewRepo(User{ID: 1, Name: "Alice"})
	loader := NewMappedUserLoader(r)
	u, _ := loader.Load(context.Background(), 1)
	fmt.Println(u.Name)
	// Output: Alice
}
```

## Review

The loader layer is correct when the fetch honors its contract and the loader is
tuned for the request. For the positional fetch, `getUsers` must keep values
aligned with ids and place each missing id's error in its own slot — the tests
assert both, because a shifted slice is silent corruption the compiler cannot
catch. The mapped fetch removes that risk entirely: a missing id is just an absent
map entry that `dataloadgen` reports as `ErrNotFound`, which is why
`NewMappedLoader` is the safer default.

The mistakes to avoid mirror the concepts. Do not loop inside the batch fetch
issuing one query per id — that reintroduces N+1 inside the loader; the whole point
is a single `IN`/`JOIN` at the batch boundary. Do not lengthen `WithWait` to force
bigger batches; cap fan-out with `WithBatchCapacity` and keep the window small. And
reach for `WithoutCache` (or `Clear`/`Prime`) on mutation paths so a read after a
write in the same request does not return the pre-write value. Confirm the round-
trip collapse with `TestLoaderBatchesRoundTrips`: a `LoadAll` over five ids that
records exactly one repo call is the proof the loader batched.

## Resources

- [`dataloadgen` reference](https://pkg.go.dev/github.com/vikstrous/dataloadgen) — `NewLoader`, `NewMappedLoader`, the options, and `ErrNotFound`.
- [vikstrous/dataloadgen source and README](https://github.com/vikstrous/dataloadgen) — usage examples and the positional vs mapped contrast.
- [gqlgen: Dataloaders](https://gqlgen.com/reference/dataloaders/) — how a real gqlgen service wires these loaders into resolvers.
- [graph-gophers/dataloader v7](https://pkg.go.dev/github.com/graph-gophers/dataloader/v7) — an alternative generic loader API, for comparison.

---

Back to [01-batch-loader-from-scratch.md](01-batch-loader-from-scratch.md) | Next: [03-gqlgen-request-scoped-wiring.md](03-gqlgen-request-scoped-wiring.md)
