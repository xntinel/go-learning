# Exercise 4: Repository layer — return a nil *Record for not-found, guard at the call site

A repository that returns `(*Record, error)` uses a `nil` pointer as the idiomatic
"not found" signal, and the service layer that calls it must nil-check before it
dereferences. This module builds both sides and proves the guard is load-bearing:
remove it and the caller panics on a miss.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
userrepo/                  independent module: example.com/userrepo
  go.mod                   module example.com/userrepo
  repo.go                  Record; Repo{FindByID}; ErrNotFound; Greeting caller with a nil guard
  cmd/
    demo/
      main.go              looks up a hit and a miss, prints both outcomes
  repo_test.go             hit returns non-nil; miss returns nil + ErrNotFound; guard is load-bearing
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: `FindByID(id string) (*Record, error)` returning `(&record, nil)` on a hit and `(nil, ErrNotFound)` on a miss, plus a `Greeting` caller that nil-checks before dereferencing.
- Test: hit returns a non-nil pointer whose fields match; miss returns a nil pointer and `errors.Is(err, ErrNotFound)`; a caller that skips the guard would panic, proving the guard matters.
- Verify: `go test -count=1 -race ./...`

### nil-as-not-found and the discipline it demands

`Repo` is an in-memory store backed by `map[string]Record`. `FindByID` does a
comma-ok map lookup: on a hit it returns the address of a fresh copy of the stored
record (`r := stored; return &r, nil`) so the caller cannot reach back through the
returned pointer and mutate the map's copy; on a miss it returns `(nil,
ErrNotFound)`. Returning `nil` for a miss is idiomatic — the caller distinguishes
found from not-found by checking the error (or the pointer), and there is no need
to invent a sentinel `Record`.

The discipline the pattern demands lives at the call site. `Greeting(repo, id)`
must check the error (or `rec == nil`) *before* it touches `rec.Name`. If it
dereferences first, a miss returns `nil` and `rec.Name` panics with a runtime
nil-pointer-dereference — in a real handler that is a 500 (or a crashed goroutine)
on every lookup of an absent id. The guard is not defensive decoration; it is the
difference between a handled 404 and a panic. The test proves this by driving both
the guarded caller (returns `ErrNotFound`) and, in a `recover`-wrapped helper, an
unguarded dereference (panics), pinning why the guard exists.

Create `repo.go`:

```go
package userrepo

import (
	"errors"
	"fmt"
)

// ErrNotFound signals that no record exists for the requested id.
var ErrNotFound = errors.New("record not found")

// Record is a stored user row.
type Record struct {
	ID   string
	Name string
}

// Repo is an in-memory record store.
type Repo struct {
	rows map[string]Record
}

// NewRepo builds a Repo seeded with the given records.
func NewRepo(seed ...Record) *Repo {
	rows := make(map[string]Record, len(seed))
	for _, r := range seed {
		rows[r.ID] = r
	}
	return &Repo{rows: rows}
}

// FindByID returns a pointer to a copy of the stored record on a hit, or
// (nil, ErrNotFound) on a miss. The nil pointer is the not-found signal; the
// copy keeps callers from mutating the map's record through the returned
// pointer.
func (r *Repo) FindByID(id string) (*Record, error) {
	stored, ok := r.rows[id]
	if !ok {
		return nil, fmt.Errorf("find %q: %w", id, ErrNotFound)
	}
	rec := stored
	return &rec, nil
}

// Greeting looks up a user and, guarding against a nil result, formats a
// greeting. On a miss it returns the error rather than dereferencing nil.
func (r *Repo) Greeting(id string) (string, error) {
	rec, err := r.FindByID(id)
	if err != nil {
		return "", err
	}
	// rec is guaranteed non-nil here because err was nil.
	return "hello, " + rec.Name, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/userrepo"
)

func main() {
	repo := userrepo.NewRepo(
		userrepo.Record{ID: "u1", Name: "alice"},
		userrepo.Record{ID: "u2", Name: "bob"},
	)

	if g, err := repo.Greeting("u1"); err == nil {
		fmt.Println("hit:", g)
	}

	if _, err := repo.Greeting("u9"); errors.Is(err, userrepo.ErrNotFound) {
		fmt.Println("miss: handled as not-found, no panic")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hit: hello, alice
miss: handled as not-found, no panic
```

### Tests

`TestFindByIDHit` asserts a hit returns a non-nil pointer whose fields match the
stored record, and that mutating the returned copy does not change what a second
lookup returns (the copy semantics). `TestFindByIDMiss` asserts a miss returns a
`nil` pointer and `errors.Is(err, ErrNotFound)`. `TestUnguardedDerefPanics` uses
`defer`/`recover` to show that dereferencing the `nil` result without a guard
panics — the exact failure the `Greeting` guard prevents.

Create `repo_test.go`:

```go
package userrepo

import (
	"errors"
	"fmt"
	"testing"
)

func TestFindByIDHit(t *testing.T) {
	t.Parallel()

	repo := NewRepo(Record{ID: "u1", Name: "alice"})
	rec, err := repo.FindByID("u1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("hit returned a nil pointer")
	}
	if rec.Name != "alice" {
		t.Fatalf("Name = %q, want alice", rec.Name)
	}

	// Mutating the returned copy must not affect a later lookup.
	rec.Name = "mutated"
	again, _ := repo.FindByID("u1")
	if again.Name != "alice" {
		t.Fatalf("second lookup Name = %q, want alice (returned pointer is a copy)", again.Name)
	}
}

func TestFindByIDMiss(t *testing.T) {
	t.Parallel()

	repo := NewRepo()
	rec, err := repo.FindByID("u9")
	if rec != nil {
		t.Fatalf("miss returned %+v, want nil pointer", rec)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUnguardedDerefPanics(t *testing.T) {
	t.Parallel()

	repo := NewRepo()

	panicked := func() (p bool) {
		defer func() {
			if recover() != nil {
				p = true
			}
		}()
		rec, _ := repo.FindByID("missing") // rec is nil
		_ = rec.Name                       // unguarded deref of nil -> panic
		return false
	}()

	if !panicked {
		t.Fatal("dereferencing the nil not-found result should panic; the guard in Greeting is load-bearing")
	}
}

func TestGreetingHandlesMiss(t *testing.T) {
	t.Parallel()

	repo := NewRepo(Record{ID: "u1", Name: "alice"})

	if g, err := repo.Greeting("u1"); err != nil || g != "hello, alice" {
		t.Fatalf("Greeting(u1) = %q,%v; want hello, alice,nil", g, err)
	}
	if _, err := repo.Greeting("u9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Greeting(u9) err = %v, want ErrNotFound", err)
	}
}

func Example() {
	repo := NewRepo(Record{ID: "u1", Name: "alice"})
	g, _ := repo.Greeting("u1")
	fmt.Println(g)
	// Output: hello, alice
}
```

## Review

The repository contract is correct when a hit yields a non-nil pointer to a copy of
the stored record and a miss yields `(nil, ErrNotFound)` — the `nil` pointer is the
signal, and wrapping the sentinel with `%w` lets callers match it with `errors.Is`
even though the message includes the id. The returned copy matters: if `FindByID`
returned `&r.rows[id]` (which does not even compile, because map elements are not
addressable) or a pointer into shared state, callers could mutate the store by
accident. `TestUnguardedDerefPanics` is the point of the whole exercise: it shows
that skipping the nil-check turns a routine miss into a panic, which is why
`Greeting` checks `err` before touching `rec.Name`. Run `go test -race` to confirm
the store holds up under the test harness.

## Resources

- [Effective Go: Errors and the comma-ok idiom](https://go.dev/doc/effective_go#maps) — map lookup returning presence.
- [`errors.Is` and wrapping with %w](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel.
- [Go Language Specification: Address operators](https://go.dev/ref/spec#Address_operators) — why `&m[k]` is illegal.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-optional-config-fields-with-pointers.md](03-optional-config-fields-with-pointers.md) | Next: [05-patch-partial-update-semantics.md](05-patch-partial-update-semantics.md)
