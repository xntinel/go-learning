# Exercise 9: HTTP Handler Golden Test: cmp.Diff over Response DTOs

Golden tests for an API response are where `reflect.DeepEqual` and hand-rolled
field asserts go to die: a `nil`-vs-`[]` slice from JSON flips the result, a
`CreatedAt` timestamp is never byte-equal, and a failure prints "not equal" with no
hint of which field. `google/go-cmp` with `cmpopts` fixes all three. This exercise
tests a JSON handler returning a `UserDTO` (with a time field and a slice field)
using `cmp.Diff` plus `EquateEmpty`, `IgnoreFields`, and `EquateApproxTime`.

This module imports `github.com/google/go-cmp`. It is otherwise fully
self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
userapi/                    independent module: example.com/userapi
  go.mod                    go 1.26; requires github.com/google/go-cmp
  userapi.go                type UserDTO (time + slice fields); UserHandler (JSON)
  cmd/
    demo/
      main.go               runnable demo: request the handler, golden-match the body
  userapi_test.go           EquateEmpty; EquateApproxTime; IgnoreFields; readable diff
```

- Files: `userapi.go`, `cmd/demo/main.go`, `userapi_test.go`.
- Implement: a `UserDTO{ID, Name, Roles []string, CreatedAt time.Time}` and a `UserHandler` that writes it as JSON.
- Test: exact match yields empty `Diff`; `nil` vs empty slice passes under `EquateEmpty` (but fails `DeepEqual`); a timestamp within tolerance passes via `EquateApproxTime` (but fails `==`); a changed field produces a readable diff.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/userapi/cmd/demo
cd ~/go-exercises/userapi
go mod init example.com/userapi
go get github.com/google/go-cmp/cmp
```

### Why golden tests need cmpopts, not DeepEqual

The `UserDTO` has two fields that make naive comparison fail for reasons that have
nothing to do with a real bug:

- **`Roles []string`.** The handler serves a user with no roles as the JSON array
  `[]`. Decoding `[]` yields a non-nil empty slice; a hand-written `want` almost
  always leaves `Roles` as `nil`. `reflect.DeepEqual(nil, []string{})` is `false`,
  so a `DeepEqual` golden test fails on a difference that does not exist.
  `cmpopts.EquateEmpty()` declares nil and empty equal, killing that false failure
  without weakening anything real.
- **`CreatedAt time.Time`.** The handler stamps `time.Now()`, which is never equal
  to a fixed `want` and — as the time exercise showed — is not even `==` to another
  reading of the same instant because of the monotonic clock. Two policies apply,
  depending on intent. If the timestamp is *irrelevant* to the contract, drop it with
  `cmpopts.IgnoreFields(UserDTO{}, "CreatedAt")`. If it must be *recent* (within a
  request's duration), allow a bounded tolerance with `cmpopts.EquateApproxTime(d)`,
  which compares two times as equal when they are within `d` of each other. Both are
  policy encoded honestly in the assertion, not a weaker "close enough" hack.

The last win is ergonomic and matters at 2 a.m.: `cmp.Diff(want, got, opts...)`
returns a *human-readable diff* pointing at the exact field that changed, and it is
empty exactly when the values are equal. The idiom
`if diff := cmp.Diff(want, got, opts...); diff != "" { t.Errorf("mismatch (-want +got):\n%s", diff) }`
gives you a failing test that tells you *what* differs, not merely *that* something
does.

Create `userapi.go`:

```go
package userapi

import (
	"encoding/json"
	"net/http"
	"time"
)

// UserDTO is an API response body. Roles serializes as a JSON array (empty as []),
// and CreatedAt is a volatile timestamp — both are golden-test hazards handled with
// cmpopts in the tests.
type UserDTO struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Roles     []string  `json:"roles"`
	CreatedAt time.Time `json:"created_at"`
}

// UserHandler writes a fixed user as JSON. Roles is an empty (non-nil) slice, so
// the body contains "roles":[]; CreatedAt is stamped at request time.
func UserHandler(w http.ResponseWriter, r *http.Request) {
	u := UserDTO{
		ID:        7,
		Name:      "alice",
		Roles:     []string{},
		CreatedAt: time.Now().UTC(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(u)
}
```

### The runnable demo

The demo drives the handler with `httptest`, decodes the body, and golden-matches it
against a `want` that leaves `Roles` nil and ignores the timestamp — printing a
deterministic result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"example.com/userapi"
)

func main() {
	req := httptest.NewRequest(http.MethodGet, "/user", nil)
	rec := httptest.NewRecorder()
	userapi.UserHandler(rec, req)

	var got userapi.UserDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		panic(err)
	}

	want := userapi.UserDTO{ID: 7, Name: "alice", Roles: nil}
	diff := cmp.Diff(want, got,
		cmpopts.EquateEmpty(),
		cmpopts.IgnoreFields(userapi.UserDTO{}, "CreatedAt"),
	)

	fmt.Printf("status: %d\n", rec.Code)
	fmt.Printf("id=%d name=%s roles=%v\n", got.ID, got.Name, got.Roles)
	fmt.Printf("golden match: %v\n", diff == "")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 200
id=7 name=alice roles=[]
golden match: true
```

### Tests

`TestGoldenMatch` is the exact-match golden test using `EquateEmpty` (nil-vs-empty
roles) and `IgnoreFields` (volatile timestamp), expecting an empty diff.
`TestEquateEmptyBeatsDeepEqual` asserts directly that the decoded empty roles slice
is not `DeepEqual` to a nil slice, but the DTOs match under `cmp` with `EquateEmpty`.
`TestEquateApproxTime` builds a `want` whose timestamp differs from `got` by a fixed
offset, asserts they are not `Equal` (so `==` would fail), yet match under
`EquateApproxTime`. `TestReadableDiff` changes an expected field and asserts the diff
is non-empty and names the field.

Create `userapi_test.go`:

```go
package userapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func fetch(t *testing.T) UserDTO {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/user", nil)
	rec := httptest.NewRecorder()
	UserHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got UserDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func TestGoldenMatch(t *testing.T) {
	t.Parallel()

	got := fetch(t)
	want := UserDTO{ID: 7, Name: "alice", Roles: nil} // nil, not []

	if diff := cmp.Diff(want, got,
		cmpopts.EquateEmpty(),
		cmpopts.IgnoreFields(UserDTO{}, "CreatedAt"),
	); diff != "" {
		t.Errorf("response mismatch (-want +got):\n%s", diff)
	}
}

func TestEquateEmptyBeatsDeepEqual(t *testing.T) {
	t.Parallel()

	got := fetch(t)

	// The decoded roles slice is empty but non-nil; a nil want differs under DeepEqual.
	if reflect.DeepEqual([]string(nil), got.Roles) {
		t.Fatal("precondition: decoded roles should be non-nil empty, unequal to nil under DeepEqual")
	}

	want := UserDTO{ID: 7, Name: "alice", Roles: nil}
	if !cmp.Equal(want, got, cmpopts.EquateEmpty(), cmpopts.IgnoreFields(UserDTO{}, "CreatedAt")) {
		t.Fatal("EquateEmpty should make nil and empty roles match")
	}
}

func TestEquateApproxTime(t *testing.T) {
	t.Parallel()

	got := fetch(t)

	// A want stamped a fixed offset away: never == to got, but within tolerance.
	want := UserDTO{ID: 7, Name: "alice", Roles: nil}
	want.CreatedAt = got.CreatedAt.Add(200 * time.Millisecond)

	if want.CreatedAt.Equal(got.CreatedAt) {
		t.Fatal("precondition: timestamps should differ so == would fail")
	}
	if !cmp.Equal(want, got, cmpopts.EquateEmpty(), cmpopts.EquateApproxTime(time.Second)) {
		t.Fatal("EquateApproxTime(1s) should accept a 200ms difference")
	}
	// And a difference beyond tolerance is correctly rejected.
	want.CreatedAt = got.CreatedAt.Add(2 * time.Second)
	if cmp.Equal(want, got, cmpopts.EquateEmpty(), cmpopts.EquateApproxTime(time.Second)) {
		t.Fatal("EquateApproxTime(1s) should reject a 2s difference")
	}
}

func TestReadableDiff(t *testing.T) {
	t.Parallel()

	got := fetch(t)
	want := UserDTO{ID: 7, Name: "bob", Roles: nil} // wrong name

	diff := cmp.Diff(want, got,
		cmpopts.EquateEmpty(),
		cmpopts.IgnoreFields(UserDTO{}, "CreatedAt"),
	)
	if diff == "" {
		t.Fatal("expected a non-empty diff for a changed field")
	}
	if !strings.Contains(diff, "Name") {
		t.Fatalf("diff should name the changed field; got:\n%s", diff)
	}
}
```

## Review

The golden test is correct when it fails only for a real contract change and prints
which field changed. `TestEquateEmptyBeatsDeepEqual` and `TestEquateApproxTime` are
the two that justify the toolset: they reproduce the exact `DeepEqual`/`==` false
failures — nil-vs-empty slice and a non-byte-equal timestamp — and show the
`cmpopts` policies absorbing them without hiding a genuine mismatch (the 2-second
case is still rejected). Choose deliberately between `IgnoreFields` (the timestamp is
outside the contract) and `EquateApproxTime` (it must be recent); do not reach for
`DeepEqual` on a response body. And read `cmp.Diff` as a string: empty means equal,
non-empty is the failure and the explanation. Run `go test -race`.

## Resources

- [google/go-cmp: cmp.Diff](https://pkg.go.dev/github.com/google/go-cmp/cmp#Diff) — the human-readable, empty-on-equal diff.
- [cmpopts: EquateEmpty, IgnoreFields, EquateApproxTime](https://pkg.go.dev/github.com/google/go-cmp/cmp/cmpopts) — the golden-test policy options.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — driving a handler in-process for a golden test.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../06-constructor-functions-and-validation/00-concepts.md](../06-constructor-functions-and-validation/00-concepts.md)
