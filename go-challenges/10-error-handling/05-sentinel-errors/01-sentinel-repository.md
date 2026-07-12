# Exercise 1: A Repository With Wrapped Sentinel Errors

The most common place sentinels appear in real backends is a repository: the
data-access layer that has to tell its callers "not found", "already exists",
"you can't touch this" without leaking storage details. This exercise builds
that repository, returning package-level sentinels wrapped with `%w`, and proves
with a test table that `errors.Is` finds each one through the wrapping.

This module is fully self-contained: its own `go mod init`, its own demo, its
own tests. Nothing here imports any other exercise.

## What you'll build

```text
repo/                         independent module: example.com/repo
  go.mod                      go 1.26
  repo.go                     ErrNotFound/ErrAlreadyExists/ErrInvalidID/ErrPermission; Repository Add/Get/Delete
  cmd/
    demo/
      main.go                 runnable demo exercising each sentinel path
  repo_test.go                table-driven errors.Is subtests + happy round-trip + identity-is-precise
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: an in-memory `Repository` with `Add`, `Get(caller, id)`, and `Delete(caller, id)` that return `fmt.Errorf("op: %w", ErrSentinel)` for each expected failure.
- Test: a `t.Run` subtest table asserting `errors.Is(err, want)` for every failure path, a happy round-trip, and a negative test proving identity is precise.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/05-sentinel-errors/01-sentinel-repository/cmd/demo
cd go-solutions/10-error-handling/05-sentinel-errors/01-sentinel-repository
```

### Why wrapped sentinels, not bare strings

Every failure this repository can report is an *expected* one that a caller will
want to branch on: a 404 on not-found, a 409 on duplicate, a 403 on a permission
miss, a 400 on a malformed id. If those were returned as
`errors.New("not found")` built fresh each call, a caller could only string-match
the message, and the first time someone reworded it every caller would break
silently. Instead each is a package-level sentinel, and every method returns it
wrapped with `fmt.Errorf("op: %w", ...)`. The `%w` is what matters: it adds
human context ("get \"x1\": not found") for the log line while keeping the
sentinel in the `Unwrap` chain so `errors.Is` still finds it. That is the whole
contract — the message is for humans, the sentinel is for code.

Note the design of `Delete`: it does its own existence and ownership checks
rather than delegating to `Get` and re-checking, so every branch is reachable
and independently testable. Delegating would make the second ownership check
dead code, because `Get` already rejects a non-owner first.

Create `repo.go`. Every failure path wraps a sentinel with `%w`:

```go
package repo

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrInvalidID     = errors.New("invalid id")
	ErrPermission    = errors.New("permission denied")
)

// Record is a stored item owned by a caller.
type Record struct {
	ID    string
	Owner string
	Data  map[string]string
}

// Repository is an in-memory store that reports expected failures as wrapped
// package-level sentinels, so callers branch with errors.Is.
type Repository struct {
	byID map[string]Record
}

func New() *Repository {
	return &Repository{byID: make(map[string]Record)}
}

// Add stores rec, rejecting a blank id or a duplicate.
func (r *Repository) Add(rec Record) error {
	if rec.ID == "" {
		return fmt.Errorf("add: %w", ErrInvalidID)
	}
	if _, ok := r.byID[rec.ID]; ok {
		return fmt.Errorf("add %q: %w", rec.ID, ErrAlreadyExists)
	}
	r.byID[rec.ID] = rec
	return nil
}

// Get returns the record if it exists and caller owns it.
func (r *Repository) Get(caller, id string) (Record, error) {
	if id == "" {
		return Record{}, fmt.Errorf("get %q: %w", id, ErrInvalidID)
	}
	rec, ok := r.byID[id]
	if !ok {
		return Record{}, fmt.Errorf("get %q: %w", id, ErrNotFound)
	}
	if rec.Owner != caller {
		return Record{}, fmt.Errorf("get %q: %w", id, ErrPermission)
	}
	return rec, nil
}

// Delete removes the record if it exists and caller owns it.
func (r *Repository) Delete(caller, id string) error {
	if id == "" {
		return fmt.Errorf("delete %q: %w", id, ErrInvalidID)
	}
	rec, ok := r.byID[id]
	if !ok {
		return fmt.Errorf("delete %q: %w", id, ErrNotFound)
	}
	if rec.Owner != caller {
		return fmt.Errorf("delete %q: %w", id, ErrPermission)
	}
	delete(r.byID, id)
	return nil
}
```

### The runnable demo

The demo drives one record through each sentinel path and matches every failure
with `errors.Is` against the exported sentinel, exactly as a caller in another
package would.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/repo"
)

func main() {
	r := repo.New()

	if err := r.Add(repo.Record{ID: "u1", Owner: "alice"}); err != nil {
		fmt.Println("add:", err)
	}
	fmt.Println("added u1 for alice")

	if err := r.Add(repo.Record{ID: "u1", Owner: "alice"}); errors.Is(err, repo.ErrAlreadyExists) {
		fmt.Println("duplicate rejected:", err)
	}

	if _, err := r.Get("bob", "u1"); errors.Is(err, repo.ErrPermission) {
		fmt.Println("permission denied:", err)
	}

	if _, err := r.Get("alice", "missing"); errors.Is(err, repo.ErrNotFound) {
		fmt.Println("not found:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
added u1 for alice
duplicate rejected: add "u1": already exists
permission denied: get "u1": permission denied
not found: get "missing": not found
```

### Tests

The test table is the point of the exercise: each row triggers one failure path
and asserts `errors.Is(err, want)` succeeds through the `%w` wrapping.
`TestHappyRoundTrip` exercises the success paths of all three methods, and
`TestIdentityIsPrecise` proves the sentinels do not collide — an `ErrInvalidID`
must not satisfy `errors.Is(err, ErrNotFound)`, or the whole scheme is
meaningless.

Create `repo_test.go`:

```go
package repo

import (
	"errors"
	"fmt"
	"testing"
)

func TestRepositorySentinels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		op   func(r *Repository) error
		want error
	}{
		{
			name: "add empty id",
			op:   func(r *Repository) error { return r.Add(Record{ID: ""}) },
			want: ErrInvalidID,
		},
		{
			name: "add duplicate",
			op: func(r *Repository) error {
				_ = r.Add(Record{ID: "x1", Owner: "alice"})
				return r.Add(Record{ID: "x1", Owner: "alice"})
			},
			want: ErrAlreadyExists,
		},
		{
			name: "get missing",
			op:   func(r *Repository) error { _, err := r.Get("alice", "missing"); return err },
			want: ErrNotFound,
		},
		{
			name: "get empty id",
			op:   func(r *Repository) error { _, err := r.Get("alice", ""); return err },
			want: ErrInvalidID,
		},
		{
			name: "get wrong owner",
			op: func(r *Repository) error {
				_ = r.Add(Record{ID: "x1", Owner: "alice"})
				_, err := r.Get("bob", "x1")
				return err
			},
			want: ErrPermission,
		},
		{
			name: "delete wrong owner",
			op: func(r *Repository) error {
				_ = r.Add(Record{ID: "x1", Owner: "alice"})
				return r.Delete("bob", "x1")
			},
			want: ErrPermission,
		},
		{
			name: "delete missing",
			op:   func(r *Repository) error { return r.Delete("alice", "missing") },
			want: ErrNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.op(New())
			if !errors.Is(err, tt.want) {
				t.Fatalf("errors.Is(err, %v) = false; err = %v", tt.want, err)
			}
		})
	}
}

func TestHappyRoundTrip(t *testing.T) {
	t.Parallel()

	r := New()
	if err := r.Add(Record{ID: "x1", Owner: "alice", Data: map[string]string{"k": "v"}}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := r.Get("alice", "x1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Data["k"] != "v" {
		t.Fatalf("Data[k] = %q, want v", got.Data["k"])
	}
	if err := r.Delete("alice", "x1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get("alice", "x1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: errors.Is(err, ErrNotFound) = false; err = %v", err)
	}
}

func TestIdentityIsPrecise(t *testing.T) {
	t.Parallel()

	err := New().Add(Record{ID: ""}) // wraps ErrInvalidID
	if errors.Is(err, ErrNotFound) {
		t.Fatal("ErrInvalidID must not satisfy errors.Is for ErrNotFound")
	}
	if !errors.Is(err, ErrInvalidID) {
		t.Fatalf("want ErrInvalidID, got %v", err)
	}
}

func ExampleRepository_Get() {
	r := New()
	_ = r.Add(Record{ID: "x1", Owner: "alice"})
	_, err := r.Get("bob", "x1")
	fmt.Println(errors.Is(err, ErrPermission))
	// Output: true
}
```

## Review

The repository is correct when every expected failure is a wrapped sentinel and
`errors.Is` recovers it through the `%w` chain, while distinct sentinels never
alias. The subtest table proves the positive direction for each path;
`TestIdentityIsPrecise` proves the negative direction that keeps the sentinels
useful. The mistakes to avoid are the ones the design already sidesteps: never
return `errors.New("not found")` fresh per call (callers get no stable identity),
never swap `%w` for `%v` (the sentinel drops out of the chain and `errors.Is`
returns `false` with an unchanged-looking message), and never delegate `Delete`
to `Get` in a way that leaves the second ownership check unreachable. Run
`go test -race` to confirm.

## Resources

- [`errors` package](https://pkg.go.dev/errors) — `errors.New` and `errors.Is`.
- [`fmt.Errorf` and `%w`](https://pkg.go.dev/fmt#Errorf) — the wrapping verb that keeps sentinels in the chain.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `Is`/`As`/`%w` and the sentinel pattern.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-map-sentinels-to-http-status.md](02-map-sentinels-to-http-status.md)
