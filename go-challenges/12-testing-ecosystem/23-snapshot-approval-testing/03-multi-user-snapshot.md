# Exercise 3: Snapshot a composite/collection output

A snapshot captures more than a single record — it captures composition and
ordering. This module formats a slice of users into one concatenated document and
pins the combined output, then shows that reordering the inputs breaks the
snapshot. Ordering becomes a contract the test enforces, which is exactly what you
want for a paginated list or an export whose row order matters.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
multisnap/                 independent module: example.com/multisnap
  go.mod                   go 1.26
  format.go                User, Format(User), FormatAll([]User)
  cmd/
    demo/
      main.go              formats two users and prints the joined document
  format_test.go           composite inline snapshot + ordering-is-a-contract test
```

Files: `format.go`, `cmd/demo/main.go`, `format_test.go`.
Implement: `Format(User) (string, error)` and `FormatAll([]User) (string, error)` that joins per-user JSON with a newline separator, preserving input order.
Test: pin the concatenated output of two users as a snapshot; a second test proves reordering the inputs changes the output.
Verify: `go test -count=1 -race ./...`

### Composition and ordering are part of the contract

A single-record snapshot pins one object's shape. A collection snapshot pins
something more: how records are joined and in what order they appear. `FormatAll`
formats each user and joins the pieces with a newline using `strings.Builder`,
which is the right tool for accumulating a document without the quadratic cost of
repeated string concatenation. Because the builder writes users in slice order,
the input order *is* the output order, and the snapshot captures that. If a future
change accidentally sorts the slice, shuffles it, or drops the separator, the
concatenated bytes change and the snapshot fails.

That is the feature. `TestOrderIsContract` makes it explicit: it formats the same
two users in the opposite order and asserts the result differs from the pinned
snapshot. In a real system this guards a genuine contract — the order of rows in a
CSV export, the order of items in a paginated API list, the order of entries in a
generated changelog. Those orders are things clients rely on, and a snapshot turns
"the order changed" from a silent behavioral shift into a failing test with a
visible diff.

`FormatAll` propagates the first per-user error rather than swallowing it, so a
blank ID anywhere in the slice fails the whole render — the same fail-loud posture
as the single-record formatter, extended to the batch.

Create `format.go`:

```go
package multisnap

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrEmpty is returned when a required identifier is missing.
var ErrEmpty = errors.New("empty value")

// User is one record in the collection.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Format renders a single user as indented JSON, rejecting a blank ID.
func Format(u User) (string, error) {
	if u.ID == "" {
		return "", fmt.Errorf("format: %w", ErrEmpty)
	}
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return "", fmt.Errorf("format: marshal: %w", err)
	}
	return string(data), nil
}

// FormatAll renders users in slice order, joined by newlines. Input order is the
// output order, which the collection snapshot pins as a contract.
func FormatAll(users []User) (string, error) {
	var b strings.Builder
	for i, u := range users {
		s, err := Format(u)
		if err != nil {
			return "", fmt.Errorf("format all: user %d: %w", i, err)
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s)
	}
	return b.String(), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/multisnap"
)

func main() {
	users := []multisnap.User{
		{ID: "u1", Name: "Alice", Email: "alice@example.com"},
		{ID: "u2", Name: "Bob", Email: "bob@example.com"},
	}
	out, err := multisnap.FormatAll(users)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "id": "u1",
  "name": "Alice",
  "email": "alice@example.com"
}
{
  "id": "u2",
  "name": "Bob",
  "email": "bob@example.com"
}
```

### Tests

`TestFormatMultipleUsers` pins the concatenated contract of two users.
`TestOrderIsContract` formats the reversed slice and asserts the output differs,
demonstrating that ordering is enforced. `TestFormatAllPropagatesError` confirms a
blank ID in the batch fails with a wrapped `ErrEmpty`.

Create `format_test.go`:

```go
package multisnap

import (
	"errors"
	"testing"
)

const twoUsers = `{
  "id": "u1",
  "name": "Alice",
  "email": "alice@example.com"
}
{
  "id": "u2",
  "name": "Bob",
  "email": "bob@example.com"
}`

func TestFormatMultipleUsers(t *testing.T) {
	t.Parallel()

	got, err := FormatAll([]User{
		{ID: "u1", Name: "Alice", Email: "alice@example.com"},
		{ID: "u2", Name: "Bob", Email: "bob@example.com"},
	})
	if err != nil {
		t.Fatalf("FormatAll returned error: %v", err)
	}
	if got != twoUsers {
		t.Fatalf("collection snapshot mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, twoUsers)
	}
}

func TestOrderIsContract(t *testing.T) {
	t.Parallel()

	reversed, err := FormatAll([]User{
		{ID: "u2", Name: "Bob", Email: "bob@example.com"},
		{ID: "u1", Name: "Alice", Email: "alice@example.com"},
	})
	if err != nil {
		t.Fatalf("FormatAll returned error: %v", err)
	}
	if reversed == twoUsers {
		t.Fatal("reordering inputs did not change output; ordering is not being captured")
	}
}

func TestFormatAllPropagatesError(t *testing.T) {
	t.Parallel()

	_, err := FormatAll([]User{
		{ID: "u1", Name: "Alice", Email: "alice@example.com"},
		{Name: "Bob"}, // blank ID
	})
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want errors.Is(err, ErrEmpty)", err)
	}
}
```

## Review

The collection snapshot is correct when it captures both the per-record shape and
the join: the separator between records and the order they appear in are as much a
part of the pinned bytes as any field value. `TestOrderIsContract` is what proves
the ordering is really being enforced — without it, a snapshot that happened to be
order-insensitive would give false confidence. The trap to avoid is over-scoping:
this concatenated document is still small and readable, so an inline snapshot is
fine, but a collection of hundreds of records is precisely the case where you move
to a `testdata/` golden and a `-update` flag, because a thousand-line literal in a
test is unreadable. Run `go test -race` to confirm the composite snapshot, the
ordering contract, and the batch error path.

## Resources

- [strings: Builder](https://pkg.go.dev/strings#Builder) — accumulating a document without repeated string allocation.
- [encoding/json: MarshalIndent](https://pkg.go.dev/encoding/json#MarshalIndent) — the per-record producer.
- [errors: Is](https://pkg.go.dev/errors#Is) — matching the wrapped batch error.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-inline-snapshot-test.md](02-inline-snapshot-test.md) | Next: [04-update-flag-golden-file.md](04-update-flag-golden-file.md)
