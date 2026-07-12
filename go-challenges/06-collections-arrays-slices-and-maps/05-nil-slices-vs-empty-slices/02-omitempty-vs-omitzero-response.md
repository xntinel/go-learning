# Exercise 2: omitempty vs omitzero on a paginated list response

A list endpoint's `Items` field has three possible wire shapes: the key is
omitted (no data was fetched), the key is present as `[]` (a query ran and
matched zero rows), or the key is present with elements. Which of the first two
you get is decided entirely by the struct tag, and Go 1.24's `omitzero` is what
lets you keep them distinct.

This module is fully self-contained: its own `go mod init`, its own `api`
package, its own demo and tests.

## What you'll build

```text
listresp/                     independent module: example.com/listresp
  go.mod
  api/api.go                  PagerResponse (omitempty), EmptyAwareResponse (omitzero), encoders
  api/api_test.go             golden JSON for nil / empty / populated under both tags
  cmd/demo/main.go            prints all three states under both contracts
```

Files: `api/api.go`, `api/api_test.go`, `cmd/demo/main.go`.
Implement: `EncodePager` (Items tagged `omitempty`) and `EncodeEmptyAware` (Items
tagged `omitzero`), over a shared `User` row type.
Test: golden JSON asserting `omitempty` drops both nil and `[]`, while `omitzero`
drops nil but emits `"items":[]` for a non-nil empty slice.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/02-omitempty-vs-omitzero-response/api go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/02-omitempty-vs-omitzero-response/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/02-omitempty-vs-omitzero-response
go mod edit -go=1.24
```

### Two tags, two advertised schemas

`omitempty` omits a field when it is empty in the JSON sense — for a slice that
means length zero, which covers *both* a nil slice and a non-nil empty slice. So
a `PagerResponse` whose `Items` is tagged `omitempty` drops the key in two
different situations that a client might care about: when no query ran (nil) and
when a query ran and matched nothing (`[]`). To that client the two are
indistinguishable; the "items" key is simply absent. That is exactly the contract
a pager wants when its stop condition is "keep requesting pages until the items
key stops appearing."

`omitzero`, added in Go 1.24, omits a field only when it equals its zero value.
The zero value of a slice is nil, so an `EmptyAwareResponse` whose `Items` is
tagged `omitzero` drops the key only when the slice is nil, and renders a non-nil
empty slice as `"items":[]`. That is the contract a different client wants — one
whose stop condition is "keep requesting until items comes back as an empty
array." The empty array is a positive signal it must be able to see.

Neither tag is "more correct." They advertise different schemas, and the schema
is part of your API. The bug is shipping `omitempty` when a client depends on
seeing `[]`, and only discovering it when that client silently never terminates
its paging loop. Choose the tag that matches the contract you documented, and pin
it with a golden test so a later refactor cannot quietly change your advertised
schema.

Create `api/api.go`:

```go
package api

import "encoding/json"

// User is one row of a paginated list.
type User struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// PagerResponse tags Items with omitempty, so the field is dropped for BOTH a
// nil slice (no query ran) and a non-nil empty slice (query ran, zero rows).
// A client that pages until the "items" key is absent works with this shape.
type PagerResponse struct {
	Items []User `json:"items,omitempty"`
}

// EmptyAwareResponse tags Items with omitzero (Go 1.24+), so the field is
// dropped only for a nil slice; a non-nil empty slice renders as "items":[].
// A client that treats [] as "end of data" needs this shape.
type EmptyAwareResponse struct {
	Items []User `json:"items,omitzero"`
}

// EncodePager marshals items under the omitempty contract.
func EncodePager(items []User) ([]byte, error) {
	return json.Marshal(PagerResponse{Items: items})
}

// EncodeEmptyAware marshals items under the omitzero contract.
func EncodeEmptyAware(items []User) ([]byte, error) {
	return json.Marshal(EmptyAwareResponse{Items: items})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/listresp/api"
)

func main() {
	var notFetched []api.User // nil: no query ran
	zeroRows := []api.User{}  // non-nil empty: query ran, matched nothing
	oneRow := []api.User{{ID: 1, Name: "alice"}}

	for _, tc := range []struct {
		label string
		items []api.User
	}{
		{"not-fetched", notFetched},
		{"zero-rows", zeroRows},
		{"one-row", oneRow},
	} {
		pager, _ := api.EncodePager(tc.items)
		aware, _ := api.EncodeEmptyAware(tc.items)
		fmt.Printf("%-12s omitempty=%-30s omitzero=%s\n", tc.label, pager, aware)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
not-fetched  omitempty={}                             omitzero={}
zero-rows    omitempty={}                             omitzero={"items":[]}
one-row      omitempty={"items":[{"id":1,"name":"alice"}]} omitzero={"items":[{"id":1,"name":"alice"}]}
```

The one row that matters is `zero-rows`: `omitempty` erases it to `{}`, while
`omitzero` preserves the distinction as `{"items":[]}`.

### Tests

Two golden tables, one per tag, pin the exact bytes for nil, empty, and populated
inputs. The `omitempty` table asserts both nil and `[]` collapse to `{}`; the
`omitzero` table asserts nil collapses to `{}` but `[]` survives as
`"items":[]`. The `Example` documents the one behavioral difference that the
whole exercise turns on.

Create `api/api_test.go`:

```go
package api

import (
	"fmt"
	"testing"
)

func TestOmitEmptyDropsNilAndEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		items []User
		want  string
	}{
		{"nil dropped", nil, `{}`},
		{"empty dropped", []User{}, `{}`},
		{"populated present", []User{{ID: 1, Name: "a"}}, `{"items":[{"id":1,"name":"a"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := EncodePager(tc.items)
			if err != nil {
				t.Fatalf("EncodePager: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("EncodePager(%v) = %s, want %s", tc.items, got, tc.want)
			}
		})
	}
}

func TestOmitZeroKeepsEmptyDropsNil(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		items []User
		want  string
	}{
		{"nil dropped", nil, `{}`},
		{"empty rendered as array", []User{}, `{"items":[]}`},
		{"populated present", []User{{ID: 1, Name: "a"}}, `{"items":[{"id":1,"name":"a"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := EncodeEmptyAware(tc.items)
			if err != nil {
				t.Fatalf("EncodeEmptyAware: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("EncodeEmptyAware(%v) = %s, want %s", tc.items, got, tc.want)
			}
		})
	}
}

func ExampleEncodeEmptyAware() {
	nilOut, _ := EncodeEmptyAware(nil)
	emptyOut, _ := EncodeEmptyAware([]User{})
	fmt.Println(string(nilOut))
	fmt.Println(string(emptyOut))
	// Output:
	// {}
	// {"items":[]}
}
```

## Review

The two encoders are correct when the golden tables hold: under `omitempty` both
nil and `[]` produce `{}`, and under `omitzero` nil produces `{}` while `[]`
produces `{"items":[]}`. The design lesson is that this difference is your API's
advertised schema, not an implementation detail — a pager that stops on an absent
key and a pager that stops on an empty array need different tags, and shipping the
wrong one manifests as a client that never terminates. `omitzero` is the Go 1.24
lever that makes "absent" and "empty" both expressible on the wire; reach for it
whenever a non-nil empty collection must survive marshaling.

## Resources

- [encoding/json — struct tags, omitempty and omitzero](https://pkg.go.dev/encoding/json#Marshal) — the exact semantics of both options.
- [Go 1.24 release notes — encoding/json omitzero](https://go.dev/doc/go1.24) — where omitzero was added and why (see the encoding/json section).
- [Go Specification: Slice types](https://go.dev/ref/spec#Slice_types) — nil is the zero value of a slice, which is what omitzero keys on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-nil-safe-repository-list.md](03-nil-safe-repository-list.md)
