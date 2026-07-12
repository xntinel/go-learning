# Exercise 6: Repository Lookup — Documenting a Sentinel Error via Example

A repository's error contract — which sentinel it returns when a record is
missing — is something callers must handle, so it belongs in the documentation as
runnable output. This exercise builds an in-memory store and documents both its
happy path and its miss path with suffixed examples, showing that examples are
the idiomatic place to demonstrate error behavior.

## What you'll build

```text
recordstore/                independent module: example.com/recordstore
  go.mod                    go 1.26
  store.go                  ErrNotFound; type Record; type Store; Get(id) (Record, error)
  cmd/
    demo/
      main.go               runnable demo exercising found and missing lookups
  store_test.go             table-driven Test + ExampleStore_Get_found, ExampleStore_Get_missing
```

Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
Implement: a package-level sentinel `ErrNotFound`, a `Record`, and a `Store` with `Get(id string) (Record, error)` that wraps the sentinel with `%w` on a miss.
Test: a table-driven `Test` asserting error identity with `errors.Is`, plus `ExampleStore_Get_found` and `ExampleStore_Get_missing`.
Verify: `go test -count=1 -race ./...`

## Examples are where error contracts get documented

The sentinel-error pattern is the backbone of Go error handling: a package
exports `var ErrNotFound = errors.New(...)`, wraps it with `%w` when it returns,
and callers test identity with `errors.Is(err, ErrNotFound)`. The wrapping is
what preserves the identity through the `fmt.Errorf` layer while adding context
(`get "id": ...`), so `errors.Is` still matches. An example is the natural place
to make this contract visible: `ExampleStore_Get_missing` prints both the error
string and the result of `errors.Is`, so a reader sees exactly what a miss looks
like and how to detect it — documentation and executed assertion in one.

The naming uses two levels of suffix. `ExampleStore_Get` would attach to the
method `Get` on `Store`. Appending a further lower-case suffix —
`ExampleStore_Get_found`, `ExampleStore_Get_missing` — attaches *multiple*
scenarios to that same method, each rendered as its own labeled block in
`go doc`. Both parts of the suffix matter: the first underscore separates the type
from the method, and the second, lower-case suffix names the scenario. Pinning the
error *string* in `ExampleStore_Get_missing` makes the message part of the public
contract too — change `ErrNotFound`'s text and the example fails, which is the
right friction for a message consumers may match on.

Create `store.go`:

```go
package recordstore

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned (wrapped) by Get when no record has the given id.
var ErrNotFound = errors.New("record not found")

// Record is a stored row.
type Record struct {
	ID   string
	Name string
}

// Store is an in-memory record repository.
type Store struct {
	data map[string]Record
}

// NewStore returns a Store seeded with the given records, keyed by ID.
func NewStore(records ...Record) *Store {
	data := make(map[string]Record, len(records))
	for _, r := range records {
		data[r.ID] = r
	}
	return &Store{data: data}
}

// Get returns the record with the given id, or an error wrapping ErrNotFound if
// none exists. Callers detect a miss with errors.Is(err, ErrNotFound).
func (s *Store) Get(id string) (Record, error) {
	r, ok := s.data[id]
	if !ok {
		return Record{}, fmt.Errorf("get %q: %w", id, ErrNotFound)
	}
	return r, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/recordstore"
)

func main() {
	s := recordstore.NewStore(recordstore.Record{ID: "u1", Name: "alice"})

	if r, err := s.Get("u1"); err == nil {
		fmt.Println("found:", r.Name)
	}

	_, err := s.Get("u2")
	fmt.Println("error:", err)
	fmt.Println("is not-found:", errors.Is(err, recordstore.ErrNotFound))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
found: alice
error: get "u2": record not found
is not-found: true
```

### Tests and examples

The `Test` is table-driven over found and missing IDs and asserts error identity
with `errors.Is` against the wrapped sentinel. The two suffixed examples document
the happy and miss paths as executed output.

Create `store_test.go`:

```go
package recordstore

import (
	"errors"
	"fmt"
	"testing"
)

func TestGet(t *testing.T) {
	t.Parallel()
	s := NewStore(Record{ID: "u1", Name: "alice"})

	tests := []struct {
		name    string
		id      string
		want    string
		wantErr error
	}{
		{"found", "u1", "alice", nil},
		{"missing", "u2", "", ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, err := s.Get(tt.id)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Get(%q) err = %v, want Is %v", tt.id, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Get(%q) unexpected err: %v", tt.id, err)
			}
			if r.Name != tt.want {
				t.Errorf("Get(%q).Name = %q, want %q", tt.id, r.Name, tt.want)
			}
		})
	}
}

func ExampleStore_Get_found() {
	s := NewStore(Record{ID: "u1", Name: "alice"})
	r, err := s.Get("u1")
	fmt.Println(r.Name, err)
	// Output: alice <nil>
}

func ExampleStore_Get_missing() {
	s := NewStore()
	_, err := s.Get("u2")
	fmt.Println(err)
	fmt.Println(errors.Is(err, ErrNotFound))
	// Output:
	// get "u2": record not found
	// true
}
```

## Review

The contract is correct when `Get` wraps `ErrNotFound` with `%w` so
`errors.Is(err, ErrNotFound)` matches through the added context — the `Test`
asserts exactly that, and `ExampleStore_Get_missing` documents it as runnable
output. The lesson-specific point is that examples are the idiomatic home for
error documentation: change `ErrNotFound`'s message and the missing example fails,
pinning the error text as public contract. The naming detail to keep straight is
the double suffix — `ExampleStore_Get_missing` attaches a *scenario* to the method
`Store.Get`; both the type-method underscore and the lower-case scenario suffix
are required. Keep `gofmt -l` empty and `go vet ./...` clean.

## Resources

- [errors.Is and error wrapping](https://pkg.go.dev/errors#Is) — how `%w` preserves sentinel identity through context.
- [The Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — the sentinel + `%w` + `errors.Is` pattern.
- [testing package — Examples](https://pkg.go.dev/testing#hdr-Examples) — suffix naming for multiple scenarios on one symbol.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-sorted-keys-stable-output.md](05-sorted-keys-stable-output.md) | Next: [07-whole-file-package-example.md](07-whole-file-package-example.md)
