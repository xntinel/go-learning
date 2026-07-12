# Exercise 2: Inline snapshot — pin serialized output to a reviewable literal

The simplest snapshot lives right in the test file: a raw-string literal the test
compares against. When the serialized shape changes, the diff shows up next to the
code in review. This module reframes the formatter's happy-path assertion as an
explicit *inline snapshot* and explains when a literal-in-the-test beats an
external golden file.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
inlinesnap/                independent module: example.com/inlinesnap
  go.mod                   go 1.26
  format.go                User, ErrEmpty, Format(User) (string, error)
  cmd/
    demo/
      main.go              prints the formatted user
  format_test.go           inline snapshot literal + ErrEmpty rejection + Example
```

Files: `format.go`, `cmd/demo/main.go`, `format_test.go`.
Implement: the same deterministic `Format(User) (string, error)`, so the module gates alone.
Test: compare `Format` output to a hard-coded `want` snapshot literal held in the test; a separate test asserts the `ErrEmpty` rejection.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/23-snapshot-approval-testing/02-inline-snapshot-test/cmd/demo
cd go-solutions/12-testing-ecosystem/23-snapshot-approval-testing/02-inline-snapshot-test
```

### Why an inline snapshot, and when

An inline snapshot is a stored expected value that happens to live in the test
source as a raw string literal. The comparison is exact string equality against
that literal. Its virtue is locality: a reviewer reading the test sees the code,
the call, and the expected serialized output all in one place, with no jump to a
`testdata/` file. For a small, self-documenting output — a five-line JSON object
like this one — that locality is worth more than the terseness an external golden
would buy. When the serialized shape changes, the `want` literal changes in the
same diff hunk as the code that produced the change, so the reviewer sees cause and
effect together.

The trade-off is scale. A raw string literal is delightful at five lines and
miserable at fifty; it clutters the test file, and there is no `-update` flag to
regenerate it — you edit it by hand. That is the boundary: inline for small and
stable, external golden (the next module onward) for large or many-case. Choosing
between them is a readability-versus-scale decision, and picking wrong in either
direction hurts. A giant literal buried in a test is unreadable; a five-line
golden file scattered into `testdata/` is over-engineered.

Go's backtick raw-string literal is what makes the inline form pleasant: it holds
newlines and double quotes with no escaping, so the JSON in the test reads exactly
as it appears on the wire. The one gotcha is that a raw literal cannot contain a
backtick, and its bytes are taken verbatim — including leading whitespace — so the
literal must start immediately after the opening backtick with no accidental
indentation, or the comparison fails on invisible spaces.

Create `format.go`:

```go
package inlinesnap

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrEmpty is returned when a required identifier is missing.
var ErrEmpty = errors.New("empty value")

// User is the record whose serialized form this test pins inline.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Format renders u as stable, indented JSON, rejecting a blank ID.
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
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/inlinesnap"
)

func main() {
	out, err := inlinesnap.Format(inlinesnap.User{ID: "u1", Name: "Alice", Email: "alice@example.com"})
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
```

### Tests

`TestInlineSnapshot` is the point: it formats a user and compares against the
`want` raw-string literal, so any change to the serialized shape is a diff in this
file. On mismatch it prints both sides so the diff is legible in the failure
output. `TestRejectsEmptyID` keeps the sentinel path covered.

Create `format_test.go`:

```go
package inlinesnap

import (
	"errors"
	"fmt"
	"testing"
)

func TestInlineSnapshot(t *testing.T) {
	t.Parallel()

	got, err := Format(User{ID: "u1", Name: "Alice", Email: "alice@example.com"})
	if err != nil {
		t.Fatalf("Format returned error: %v", err)
	}

	// Inline snapshot: the expected serialized contract, reviewable in this diff.
	const want = `{
  "id": "u1",
  "name": "Alice",
  "email": "alice@example.com"
}`

	if got != want {
		t.Fatalf("inline snapshot mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRejectsEmptyID(t *testing.T) {
	t.Parallel()

	_, err := Format(User{Name: "Alice"})
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want errors.Is(err, ErrEmpty)", err)
	}
}

func ExampleFormat() {
	out, _ := Format(User{ID: "u7", Name: "Bob", Email: "bob@example.com"})
	fmt.Println(out)
	// Output:
	// {
	//   "id": "u7",
	//   "name": "Bob",
	//   "email": "bob@example.com"
	// }
}
```

## Review

The inline snapshot is correct when the `want` literal is byte-identical to what
`Format` emits, including the absence of a trailing newline (a raw literal that
closes with `}` on its own line has none). The classic failure is invisible
whitespace: a `want` literal accidentally indented to line up with the surrounding
Go code carries leading spaces that `MarshalIndent` never produced, so the exact
comparison fails on bytes you cannot see in the diff — keep the literal flush to
column zero. Reach for this form only while the output stays small; the moment it
grows or you want more than one case, move to the `testdata/` golden and `-update`
workflow of Exercise 4. Run `go test -race` to confirm the snapshot and the
sentinel path both hold.

## Resources

- [Go spec: string literals](https://go.dev/ref/spec#String_literals) — raw (backtick) literals take their bytes verbatim, newlines included.
- [testing: T.Fatalf](https://pkg.go.dev/testing#T.Fatalf) — reporting a mismatch with both sides for a readable diff.
- [encoding/json: MarshalIndent](https://pkg.go.dev/encoding/json#MarshalIndent) — the deterministic producer behind the snapshot.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-implement-json-formatter.md](01-implement-json-formatter.md) | Next: [03-multi-user-snapshot.md](03-multi-user-snapshot.md)
