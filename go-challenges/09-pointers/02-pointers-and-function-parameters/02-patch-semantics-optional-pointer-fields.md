# Exercise 2: PATCH Handler — *string/*int Fields to Tell "Absent" from "Zero"

A REST PATCH must update only the fields the client sent and leave the rest alone.
That is impossible with plain value fields, because `json.Unmarshal` cannot tell an
omitted `email` from an `email: ""`. This exercise builds the real fix: a DTO of
pointer fields whose nil-ness carries the "present or absent" bit, and an `Apply`
that writes only present fields into the stored struct through a pointer parameter.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
patch/                      independent module: example.com/patch
  go.mod
  patch.go                  User; UserPatch (*string/*int/*bool); DecodePatch; Apply(*User); Store.Handler
  cmd/
    demo/
      main.go               decodes two PATCH bodies and shows absent vs zero
  patch_test.go             table test (absent/zero/full), httptest handler round-trip
```

- Files: `patch.go`, `cmd/demo/main.go`, `patch_test.go`.
- Implement: a `UserPatch` with pointer fields, a `DecodePatch`, an `Apply(*User)` that writes only non-nil fields, and an HTTP handler that applies a PATCH to a stored user.
- Test: prove an omitted field leaves the target unchanged, a field set to a zero value overwrites it, a full body updates everything, and the handler mutates the stored user through the pointer.
- Verify: `go test -count=1 -race ./...`

### Why the fields must be pointers

`json.Unmarshal` into a struct leaves a field at its zero value if the JSON omits
it. For a plain `Email string`, that means an omitted `email` and an explicit
`"email": ""` both produce `""` — indistinguishable. A PATCH handler that copies
every field into the stored record would therefore blank out the stored email on
any request that did not mention it. The bug is silent and destroys data.

A `*string` field fixes it because `Unmarshal` only allocates the pointer when the
key is present. Omitted key leaves the pointer `nil`; `"email": ""` decodes to a
non-nil pointer at `""`; `"email": "a@b.com"` decodes to a non-nil pointer at the
value. `Apply` then writes a field into the target only when its pointer is non-nil,
so absent fields are skipped and present fields — including present-but-zero — are
applied. The pointer parameter `*User` is the other half: `Apply` mutates the
caller's stored user in place, exactly the semantics a PATCH needs.

Create `patch.go`:

```go
package patch

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

// User is the stored resource.
type User struct {
	Name   string
	Email  string
	Age    int
	Active bool
}

// UserPatch is a decoded PATCH body. Pointer fields distinguish "absent from the
// request" (nil) from "present, possibly the zero value" (non-nil).
type UserPatch struct {
	Name   *string `json:"name"`
	Email  *string `json:"email"`
	Age    *int    `json:"age"`
	Active *bool   `json:"active"`
}

// DecodePatch parses a JSON PATCH body into a UserPatch.
func DecodePatch(body []byte) (UserPatch, error) {
	var p UserPatch
	if err := json.Unmarshal(body, &p); err != nil {
		return UserPatch{}, err
	}
	return p, nil
}

// Apply writes every present (non-nil) field of p into u through the pointer
// parameter. Absent fields leave u's current value untouched.
func (p UserPatch) Apply(u *User) {
	if p.Name != nil {
		u.Name = *p.Name
	}
	if p.Email != nil {
		u.Email = *p.Email
	}
	if p.Age != nil {
		u.Age = *p.Age
	}
	if p.Active != nil {
		u.Active = *p.Active
	}
}

// Store holds one user behind a mutex and serves PATCH requests against it.
type Store struct {
	mu   sync.Mutex
	user User
}

// NewStore seeds the store with an initial user.
func NewStore(initial User) *Store {
	return &Store{user: initial}
}

// Snapshot returns a copy of the stored user.
func (s *Store) Snapshot() User {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.user
}

// Handler decodes the PATCH body and applies it to the stored user in place,
// then writes the updated user back as JSON.
func (s *Store) Handler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := DecodePatch(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	p.Apply(&s.user)
	updated := s.user
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(updated)
}
```

### The runnable demo

The demo decodes two bodies against the same starting user: one omits `email` (it
must survive), one explicitly clears it to `""` (it must be blanked).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/patch"
)

func main() {
	base := patch.User{Name: "Ada", Email: "ada@x.com", Age: 36, Active: true}

	// Body omits email -> the stored email survives; only Age changes.
	omit := base
	p1, _ := patch.DecodePatch([]byte(`{"age": 37}`))
	p1.Apply(&omit)
	fmt.Printf("omit email:  %+v\n", omit)

	// Body sets email to "" explicitly -> the stored email is cleared.
	clear := base
	p2, _ := patch.DecodePatch([]byte(`{"email": ""}`))
	p2.Apply(&clear)
	fmt.Printf("clear email: %+v\n", clear)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
omit email:  {Name:Ada Email:ada@x.com Age:37 Active:true}
clear email: {Name:Ada Email: Age:36 Active:true}
```

### Tests

The table test is the crux: it distinguishes absent from zero. The `httptest`
round-trip proves the handler mutates the stored user through the `*User` parameter
and returns the merged result.

Create `patch_test.go`:

```go
package patch

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApply(t *testing.T) {
	t.Parallel()
	base := User{Name: "Ada", Email: "ada@x.com", Age: 36, Active: true}

	cases := map[string]struct {
		body string
		want User
	}{
		"omitted field survives": {
			body: `{"age": 37}`,
			want: User{Name: "Ada", Email: "ada@x.com", Age: 37, Active: true},
		},
		"explicit zero string overwrites": {
			body: `{"email": ""}`,
			want: User{Name: "Ada", Email: "", Age: 36, Active: true},
		},
		"explicit zero int overwrites": {
			body: `{"age": 0}`,
			want: User{Name: "Ada", Email: "ada@x.com", Age: 0, Active: true},
		},
		"explicit false overwrites": {
			body: `{"active": false}`,
			want: User{Name: "Ada", Email: "ada@x.com", Age: 36, Active: false},
		},
		"full body updates all": {
			body: `{"name":"Grace","email":"g@x.com","age":40,"active":false}`,
			want: User{Name: "Grace", Email: "g@x.com", Age: 40, Active: false},
		},
		"empty body changes nothing": {
			body: `{}`,
			want: base,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, err := DecodePatch([]byte(tc.body))
			if err != nil {
				t.Fatalf("DecodePatch(%s): %v", tc.body, err)
			}
			got := base // copy; Apply mutates through the pointer
			p.Apply(&got)
			if got != tc.want {
				t.Fatalf("Apply(%s) = %+v, want %+v", tc.body, got, tc.want)
			}
		})
	}
}

func TestHandlerMutatesStoredUser(t *testing.T) {
	t.Parallel()
	s := NewStore(User{Name: "Ada", Email: "ada@x.com", Age: 36, Active: true})

	req := httptest.NewRequest(http.MethodPatch, "/user", strings.NewReader(`{"age": 41}`))
	rec := httptest.NewRecorder()
	s.Handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := s.Snapshot(); got.Age != 41 || got.Email != "ada@x.com" {
		t.Fatalf("stored user = %+v, want Age 41 and untouched email", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"Age":41`) {
		t.Fatalf("response body = %q, want merged user", body)
	}
}
```

## Review

The package is correct when "absent" and "zero" are genuinely distinct at every
field. The proof is in the table: `{"age": 0}` must overwrite `Age` to 0, while
`{}` must leave it alone — a value-field DTO cannot tell those apart and would fail
one of them. `Apply` earns its `*User` parameter because a PATCH's entire job is to
mutate the stored record in place; returning a new `User` would force every call
site to remember to assign it back. If you ever need to distinguish "set to JSON
null" from "omitted" as well (a rarer requirement), a `json.RawMessage` field lets
you inspect whether the key was present and whether its value was literally `null`.
Run `go test -race`; the handler locks the store, so concurrent PATCHes are safe.

## Resources

- [`encoding/json` Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal) — how omitted keys leave fields untouched and pointer fields decode.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest`/`NewRecorder` for testing handlers without a socket.
- [JSON and Go](https://go.dev/blog/json) — the Go blog on decoding into structs and pointers.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-config-defaults-mutate-vs-return.md](01-config-defaults-mutate-vs-return.md) | Next: [03-functional-options-constructor.md](03-functional-options-constructor.md)
