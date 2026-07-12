# Exercise 3: Translate Leaky Driver Sentinels At The Seam

`sql.ErrNoRows` and `io.EOF` are sentinels too — but they belong to your
infrastructure, not your domain. If a repository returns them unchanged, every
caller is coupled to `database/sql`. This exercise builds the adapter that
catches those infrastructure sentinels with `errors.Is` and re-wraps them as a
domain `ErrNotFound`, so nothing above the seam knows there is a SQL driver
underneath.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
users/                        independent module: example.com/users
  go.mod                      go 1.26
  users.go                    ErrNotFound; Repo over a queryFunc; GetUser translates driver sentinels
  cmd/
    demo/
      main.go                 wires a fake query returning sql.ErrNoRows, prints translated error
  users_test.go               injects sql.ErrNoRows / io.EOF; asserts translation is total
```

- Files: `users.go`, `cmd/demo/main.go`, `users_test.go`.
- Implement: a `Repo` over an injectable `queryFunc`, whose `GetUser` maps `sql.ErrNoRows` and `io.EOF` to `fmt.Errorf("getUser %s: %w", id, ErrNotFound)` and passes any other error through.
- Test: inject each driver sentinel and assert the result satisfies `errors.Is(err, ErrNotFound)` and does NOT satisfy `errors.Is(err, sql.ErrNoRows)` / `io.EOF`.
- Verify: `go test -count=1 -race ./...`

### The boundary rule: infrastructure sentinels stop at the adapter

A `SELECT ... WHERE id = ?` that matches no row is a normal, expected outcome,
and `database/sql` reports it as the sentinel `sql.ErrNoRows`. A row-scanning
adapter over a different transport might surface the same "no data" condition as
`io.EOF`. Both are *infrastructure* facts. The problem with returning them raw is
coupling: the instant a caller writes `errors.Is(err, sql.ErrNoRows)`, that
caller imports `database/sql`, and your storage choice has leaked into code that
should not care whether the data lives in Postgres, SQLite, or a remote service.
Replace the backend and every such caller breaks.

The fix is a one-line discipline at the adapter: catch the infrastructure
sentinel with `errors.Is`, and return your *own* domain sentinel wrapped with
`%w`. The key property to verify is that the translation is *total*: the returned
error satisfies `errors.Is(err, ErrNotFound)` **and** no longer satisfies
`errors.Is(err, sql.ErrNoRows)`. If the driver sentinel were merely wrapped
(still in the chain) rather than replaced, `errors.Is(err, sql.ErrNoRows)` would
still return `true` and the coupling would persist. Wrapping `ErrNotFound`
instead of `sql.ErrNoRows` is what makes the seam real.

An error that is *not* a recognized infrastructure "no data" sentinel — a
connection refusal, a timeout — must pass through unchanged (wrapped for
context). Masking a real failure as `ErrNotFound` would be worse than leaking:
the caller would return a cheerful 404 while the database is on fire.

Create `users.go`:

```go
package users

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
)

// ErrNotFound is the domain sentinel callers match. It is deliberately the only
// "no data" signal that crosses this package's boundary.
var ErrNotFound = errors.New("user not found")

type User struct {
	ID   string
	Name string
}

// queryFunc is the minimal query surface the repository depends on. In
// production it wraps a *sql.DB row scan; here it is injectable so a test can
// feed it a driver sentinel directly.
type queryFunc func(id string) (User, error)

type Repo struct {
	query queryFunc
}

func NewRepo(q queryFunc) *Repo { return &Repo{query: q} }

// GetUser translates the infrastructure sentinels sql.ErrNoRows and io.EOF into
// the domain sentinel ErrNotFound at the boundary, so callers never import
// database/sql. Any other error passes through (wrapped for context).
func (r *Repo) GetUser(id string) (User, error) {
	u, err := r.query(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, io.EOF) {
			return User{}, fmt.Errorf("getUser %s: %w", id, ErrNotFound)
		}
		return User{}, fmt.Errorf("getUser %s: %w", id, err)
	}
	return u, nil
}
```

### The runnable demo

The demo wires a fake query backed by an in-memory map that returns
`sql.ErrNoRows` for an unknown id — the same sentinel the real driver returns —
and shows that what escapes `GetUser` is `ErrNotFound`, with the driver sentinel
fully gone from the chain.

Create `cmd/demo/main.go`:

```go
package main

import (
	"database/sql"
	"errors"
	"fmt"

	"example.com/users"
)

func main() {
	db := map[string]users.User{"u1": {ID: "u1", Name: "alice"}}
	repo := users.NewRepo(func(id string) (users.User, error) {
		u, ok := db[id]
		if !ok {
			return users.User{}, sql.ErrNoRows // the driver's sentinel
		}
		return u, nil
	})

	u, err := repo.GetUser("u1")
	fmt.Printf("found: %s (err=%v)\n", u.Name, err)

	_, err = repo.GetUser("ghost")
	fmt.Println("domain error:", err)
	fmt.Println("is ErrNotFound:", errors.Is(err, users.ErrNotFound))
	fmt.Println("is sql.ErrNoRows:", errors.Is(err, sql.ErrNoRows))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
found: alice (err=<nil>)
domain error: getUser ghost: user not found
is ErrNotFound: true
is sql.ErrNoRows: false
```

### Tests

The table injects each infrastructure sentinel and asserts the translation is
total: `ErrNotFound` matches, `sql.ErrNoRows` and `io.EOF` do not.
`TestNonDriverErrorPassesThrough` guards the other direction — a real failure
must not be laundered into a 404.

Create `users_test.go`:

```go
package users

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestTranslatesDriverSentinels(t *testing.T) {
	t.Parallel()

	drivers := []struct {
		name string
		err  error
	}{
		{"sql.ErrNoRows", sql.ErrNoRows},
		{"io.EOF", io.EOF},
	}
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			repo := NewRepo(func(string) (User, error) { return User{}, d.err })
			_, err := repo.GetUser("x")

			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("errors.Is(err, ErrNotFound) = false; err = %v", err)
			}
			if errors.Is(err, sql.ErrNoRows) {
				t.Fatal("sql.ErrNoRows leaked out of the domain layer")
			}
			if errors.Is(err, io.EOF) {
				t.Fatal("io.EOF leaked out of the domain layer")
			}
		})
	}
}

func TestNonDriverErrorPassesThrough(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection refused")
	repo := NewRepo(func(string) (User, error) { return User{}, boom })
	_, err := repo.GetUser("x")

	if errors.Is(err, ErrNotFound) {
		t.Fatal("a real infra failure must not be masked as ErrNotFound")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("want wrapped boom, got %v", err)
	}
}

func TestHappyPath(t *testing.T) {
	t.Parallel()

	repo := NewRepo(func(id string) (User, error) { return User{ID: id, Name: "alice"}, nil })
	u, err := repo.GetUser("u1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.Name != "alice" {
		t.Fatalf("Name = %q, want alice", u.Name)
	}
}

func ExampleRepo_GetUser() {
	repo := NewRepo(func(string) (User, error) { return User{}, sql.ErrNoRows })
	_, err := repo.GetUser("ghost")
	fmt.Println(errors.Is(err, ErrNotFound), errors.Is(err, sql.ErrNoRows))
	// Output: true false
}
```

## Review

The adapter is correct when the driver sentinel is *replaced*, not merely
wrapped: `errors.Is(err, ErrNotFound)` is true and `errors.Is(err, sql.ErrNoRows)`
is false on the same value. That pair of assertions is the proof the seam holds
— if the second were true, `database/sql` would still be leaking. The mistake to
avoid is a blanket `if err != nil { return ErrNotFound }` that swallows every
error, including real infrastructure failures, into a not-found; only the
recognized "no data" sentinels translate, everything else passes through wrapped.

## Resources

- [`database/sql` variables](https://pkg.go.dev/database/sql#pkg-variables) — `sql.ErrNoRows` and when the driver returns it.
- [`io.EOF`](https://pkg.go.dev/io#pkg-variables) — the end-of-stream sentinel.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — chain-walking comparison used to catch the driver sentinel.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-map-sentinels-to-http-status.md](02-map-sentinels-to-http-status.md) | Next: [04-classify-retryable-vs-terminal.md](04-classify-retryable-vs-terminal.md)
