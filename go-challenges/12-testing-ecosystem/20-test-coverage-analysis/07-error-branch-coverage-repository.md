# Exercise 7: Find and close untested error branches in a repository layer

The most valuable thing a coverage profile shows is not the headline percentage —
it is the red: the error branches no test ever drove. A failed query, a cancelled
context, a constraint violation, a retry that exhausted its budget: exactly the
paths that fire in production and are hardest to trigger on purpose. This module
builds a repository method with four failure branches, starts with happy-path-only
tests, uses `-func`/`-html` to locate the uncovered branches, and closes each with
a fault-injecting fake and `errors.Is` assertions.

This module is fully self-contained: its own `go mod init`, a demo, and tests.

## What you'll build

```text
repo/                      independent module: example.com/repo
  go.mod
  repo.go                  UserStore, GetUserWithRetry; four error branches + sentinels
  cmd/
    demo/
      main.go              runnable demo hitting the happy path and a fault
  repo_test.go             fault-injecting fake; a test per error branch
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: a `Store` interface, a `GetUserWithRetry` function that retries a transient error up to N times, and sentinels `ErrNotFound`, `ErrConflict`, `ErrRetriesExhausted` plus context-cancellation handling.
- Test: a fake `Store` that injects each failure; one table-driven test that drives and asserts every documented error branch with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

### The branches a happy-path test never sees

`GetUserWithRetry` is a realistic repository call: it takes a context, asks the
store for a user, and on a *transient* error (a dropped connection, a serialization
failure) it retries with a small budget; on a *permanent* error (not found,
constraint violation) it returns immediately. That gives it five distinct paths:
the success path, the not-found path, the conflict path, the retry-then-succeed
path, and the retries-exhausted path — plus context cancellation, which must be
honored before each attempt so a cancelled caller does not keep hammering a dead
backend.

A test that only calls `GetUserWithRetry` with a healthy store covers exactly one
of those paths. The `-func` output shows the function well below 100%, and `-html`
paints the four error branches red. Those reds are the coverage insight that
matters: they are the branches that will run in production and have never been
exercised. Closing them means injecting each failure with a fake store and
asserting — with `errors.Is`, because the sentinels are wrapped with `%w` for
context — that the function classifies and propagates it correctly. Not just
*executing* the branch (that alone would be an assertion-free trap), but asserting
the resulting error is the right sentinel.

Create `repo.go`:

```go
package repo

import (
	"context"
	"errors"
	"fmt"
)

// Sentinel errors the repository classifies. Callers match them with errors.Is.
var (
	ErrNotFound         = errors.New("user not found")
	ErrConflict         = errors.New("constraint violation")
	ErrTransient        = errors.New("transient failure")
	ErrRetriesExhausted = errors.New("retries exhausted")
)

// User is a stored record.
type User struct {
	ID   string
	Name string
}

// Store is the persistence port. A real implementation talks to a database;
// tests supply a fake that injects failures.
type Store interface {
	Find(ctx context.Context, id string) (User, error)
}

// GetUserWithRetry fetches a user, retrying a transient error up to maxRetries
// times. It returns immediately on a permanent error (not found, conflict), and
// honors context cancellation before each attempt.
func GetUserWithRetry(ctx context.Context, s Store, id string, maxRetries int) (User, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return User{}, fmt.Errorf("get user %q: %w", id, err)
		}
		u, err := s.Find(ctx, id)
		switch {
		case err == nil:
			return u, nil
		case errors.Is(err, ErrNotFound):
			return User{}, fmt.Errorf("get user %q: %w", id, ErrNotFound)
		case errors.Is(err, ErrConflict):
			return User{}, fmt.Errorf("get user %q: %w", id, ErrConflict)
		case errors.Is(err, ErrTransient):
			lastErr = err
			continue // retry
		default:
			return User{}, fmt.Errorf("get user %q: %w", id, err)
		}
	}
	return User{}, fmt.Errorf("get user %q after %d attempts: %w", id, maxRetries+1, ErrRetriesExhausted)
}

var _ = lastErrUnused

// lastErrUnused documents that lastErr is retained for diagnostics; it is
// intentionally not returned so callers match ErrRetriesExhausted.
func lastErrUnused() {}
```

Wait — `lastErr` is assigned but never used, which fails `go vet`/compile. Drop
the diagnostic scaffolding and either use `lastErr` in the exhausted message or
remove it. Use it, so the last transient cause is visible:

Create `repo.go`:

```go
package repo

import (
	"context"
	"errors"
	"fmt"
)

// Sentinel errors the repository classifies. Callers match them with errors.Is.
var (
	ErrNotFound         = errors.New("user not found")
	ErrConflict         = errors.New("constraint violation")
	ErrTransient        = errors.New("transient failure")
	ErrRetriesExhausted = errors.New("retries exhausted")
)

// User is a stored record.
type User struct {
	ID   string
	Name string
}

// Store is the persistence port. A real implementation talks to a database;
// tests supply a fake that injects failures.
type Store interface {
	Find(ctx context.Context, id string) (User, error)
}

// GetUserWithRetry fetches a user, retrying a transient error up to maxRetries
// times. It returns immediately on a permanent error (not found, conflict), and
// honors context cancellation before each attempt.
func GetUserWithRetry(ctx context.Context, s Store, id string, maxRetries int) (User, error) {
	var lastErr error = ErrRetriesExhausted
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return User{}, fmt.Errorf("get user %q: %w", id, err)
		}
		u, err := s.Find(ctx, id)
		switch {
		case err == nil:
			return u, nil
		case errors.Is(err, ErrNotFound):
			return User{}, fmt.Errorf("get user %q: %w", id, ErrNotFound)
		case errors.Is(err, ErrConflict):
			return User{}, fmt.Errorf("get user %q: %w", id, ErrConflict)
		case errors.Is(err, ErrTransient):
			lastErr = err
			continue // retry
		default:
			return User{}, fmt.Errorf("get user %q: %w", id, err)
		}
	}
	return User{}, fmt.Errorf("get user %q after %d attempts (last: %v): %w",
		id, maxRetries+1, lastErr, ErrRetriesExhausted)
}
```

The second `Create` of `repo.go` overwrites the first, so the compiled file is the
clean version — the first block above is shown only to name the mistake it
contains.

### The runnable demo

The demo drives the happy path and one fault (retries exhausted) so `go run
./cmd/demo` prints deterministic output.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/repo"
)

// flaky returns ErrTransient a fixed number of times, then a user.
type flaky struct {
	failsLeft int
}

func (f *flaky) Find(ctx context.Context, id string) (repo.User, error) {
	if f.failsLeft > 0 {
		f.failsLeft--
		return repo.User{}, repo.ErrTransient
	}
	return repo.User{ID: id, Name: "Alice"}, nil
}

func main() {
	ctx := context.Background()

	// Recovers after two transient failures within a budget of 3.
	u, err := repo.GetUserWithRetry(ctx, &flaky{failsLeft: 2}, "u1", 3)
	fmt.Printf("recovered: %+v err=%v\n", u, err)

	// Exhausts the budget: 5 failures, only 2 retries.
	_, err = repo.GetUserWithRetry(ctx, &flaky{failsLeft: 5}, "u2", 2)
	fmt.Println("exhausted is ErrRetriesExhausted:", errors.Is(err, repo.ErrRetriesExhausted))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
recovered: {ID:u1 Name:Alice} err=<nil>
exhausted is ErrRetriesExhausted: true
```

### Finding the gaps, then closing them

Start with a happy-path-only test to see the profile light up red. Write just this,
generate the profile, and read it:

```bash
go test -coverprofile=cover.out ./...
go tool cover -func=cover.out
```

With only a success test, `GetUserWithRetry` reports well under 100% — the
not-found, conflict, transient-retry, exhausted, and cancellation branches are all
uncovered. Open the HTML to see them painted red:

```bash
go tool cover -html=cover.out
```

Now close each branch with a fault-injecting fake. The test below is the complete
suite: a `fakeStore` whose behavior is programmable per call, and a table that
drives every documented branch and asserts the resulting sentinel with
`errors.Is`.

Create `repo_test.go`:

```go
package repo

import (
	"context"
	"errors"
	"testing"
)

// fakeStore returns queued results in order; the last result repeats.
type fakeStore struct {
	results []result
	calls   int
}

type result struct {
	user User
	err  error
}

func (f *fakeStore) Find(ctx context.Context, id string) (User, error) {
	i := f.calls
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	f.calls++
	return f.results[i].user, f.results[i].err
}

func TestGetUserWithRetry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		results    []result
		maxRetries int
		wantErr    error // nil means success
		wantName   string
	}{
		{
			name:     "success",
			results:  []result{{user: User{ID: "u1", Name: "Alice"}}},
			wantName: "Alice",
		},
		{
			name:    "not-found",
			results: []result{{err: ErrNotFound}},
			wantErr: ErrNotFound,
		},
		{
			name:    "conflict",
			results: []result{{err: ErrConflict}},
			wantErr: ErrConflict,
		},
		{
			name: "transient-then-success",
			results: []result{
				{err: ErrTransient},
				{err: ErrTransient},
				{user: User{ID: "u1", Name: "Bob"}},
			},
			maxRetries: 3,
			wantName:   "Bob",
		},
		{
			name:       "retries-exhausted",
			results:    []result{{err: ErrTransient}},
			maxRetries: 2,
			wantErr:    ErrRetriesExhausted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u, err := GetUserWithRetry(context.Background(), &fakeStore{results: tt.results}, "u1", tt.maxRetries)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && u.Name != tt.wantName {
				t.Errorf("user name = %q, want %q", u.Name, tt.wantName)
			}
		})
	}
}

func TestContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the first attempt
	_, err := GetUserWithRetry(ctx, &fakeStore{results: []result{{user: User{ID: "u1"}}}}, "u1", 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestUnexpectedErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("disk on fire")
	_, err := GetUserWithRetry(context.Background(), &fakeStore{results: []result{{err: sentinel}}}, "u1", 3)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the wrapped unexpected error", err)
	}
}
```

Re-run the profile with the full suite and every branch is green:

```bash
go test -coverprofile=cover.out ./... && go tool cover -func=cover.out | grep GetUserWithRetry
```

Expected output:

```
example.com/repo/repo.go:...  GetUserWithRetry  100.0%
```

## Review

`GetUserWithRetry` is correct when it returns the user on success, propagates
`ErrNotFound` and `ErrConflict` immediately (no retry), retries `ErrTransient`
within the budget and succeeds if a later attempt does, returns
`ErrRetriesExhausted` when the budget runs out, propagates an unexpected error
wrapped, and honors context cancellation before each attempt — every branch driven
and asserted by the table plus the cancellation and unexpected-error tests.

The mistake to avoid is generating `cover.out` and never reading it: the whole
value here is that `-func`/`-html` point at the red error branches, and the fix is
to write *asserting* tests for them, not merely tests that execute the line. Match
the sentinels with `errors.Is` against `%w`-wrapped errors, not string comparison.
Run `go test -race` to confirm the fake and retry loop are clean under
concurrency.

## Resources

- [`go tool cover`](https://pkg.go.dev/cmd/cover) — `-func` and `-html` for locating uncovered branches.
- [`errors`](https://pkg.go.dev/errors) — `errors.Is` and `%w` wrapping for sentinel classification.
- [`context`](https://pkg.go.dev/context#Context) — `ctx.Err`, `context.Canceled`, and cancellation semantics.

---

Back to [06-coverage-threshold-ci-gate.md](06-coverage-threshold-ci-gate.md) | Next: [08-assertion-free-coverage-trap.md](08-assertion-free-coverage-trap.md)
