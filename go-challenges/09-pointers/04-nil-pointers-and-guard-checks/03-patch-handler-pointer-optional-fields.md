# Exercise 3: PATCH Semantics — Distinguishing Absent From Zero

A correct HTTP PATCH updates only the fields the client sent and leaves the rest
untouched — including when the client sets a field to its zero value. This module
builds a PATCH handler whose DTO uses pointer fields so a nil field means "not
sent, leave unchanged" while a non-nil pointer to `false`/`0`/`""` means
"explicitly set to the zero value".

This module is fully self-contained.

## What you'll build

```text
patchuser/                independent module: example.com/patchuser
  go.mod                  go 1.24
  patch.go                type UserPatch (pointer fields); Apply; Handler over a store
  cmd/
    demo/
      main.go             runnable demo: partial patch keeps untouched fields
  patch_test.go           absent vs explicit-zero vs real-value payloads, httptest
```

Files: `patch.go`, `cmd/demo/main.go`, `patch_test.go`.
Implement: a `UserPatch` with `*string`, `*bool`, `*int` fields; `Apply(*User)` that writes only non-nil fields; an `http.Handler` that decodes the body and applies it to a stored user.
Test: three payloads for `active` — key absent (unchanged), explicit `false` (set to false), and a real value — asserting only intended fields mutate; a `"active": false` body proving false is not swallowed as "not sent".
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/patchuser/cmd/demo
cd ~/go-exercises/patchuser
go mod init example.com/patchuser
go mod edit -go=1.24
```

### Why the fields are pointers

Decode a PATCH body into a value struct and the semantics break. A `User` DTO
with `Active bool` cannot tell you whether the client sent `"active": false` or
omitted `active` entirely: both decode to `false`. If you then copy every field
onto the stored entity, an omitted `active` silently deactivates the user. The
only fix at the type level is a field that can hold one more state than its value
domain. A `*bool` has three observable states: nil (key absent), non-nil pointing
to `false`, and non-nil pointing to `true`. `encoding/json` leaves a pointer
field nil when the key is missing and allocates it when the key is present, so
nil precisely encodes "the client did not send this".

`Apply` then writes a field onto the entity only when its pointer is non-nil:

```go
if p.Active != nil {
	u.Active = *p.Active
}
```

A missing `active` leaves the stored `Active` untouched; an explicit
`"active": false` sets it to false. That is correct PATCH.

One honest limitation to teach: with a plain pointer, JSON `null` and an absent
key both decode to a nil pointer, so this handler treats `"name": null` the same
as omitting `name` (leave unchanged). Distinguishing "set to null / clear the
field" from "not sent" needs a third state — a `json.RawMessage` field you
inspect for the literal `null`, or a dedicated Optional type. For most PATCH APIs
"null means leave unchanged" is the desired behavior, so a pointer is the right
tool; reach for the richer type only when clearing-to-null is a real operation.

Create `patch.go`:

```go
package patchuser

import (
	"encoding/json"
	"net/http"
	"sync"
)

// User is the stored entity.
type User struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
	Age    int    `json:"age"`
}

// UserPatch is a partial update. A nil field means "the client did not send
// this key; leave it unchanged". A non-nil field sets the value, even to zero.
type UserPatch struct {
	Name   *string `json:"name"`
	Active *bool   `json:"active"`
	Age    *int    `json:"age"`
}

// Apply writes only the non-nil fields of p onto u.
func (p UserPatch) Apply(u *User) {
	if p.Name != nil {
		u.Name = *p.Name
	}
	if p.Active != nil {
		u.Active = *p.Active
	}
	if p.Age != nil {
		u.Age = *p.Age
	}
}

// Store is a tiny in-memory user store guarded by a mutex.
type Store struct {
	mu    sync.Mutex
	users map[string]*User
}

// NewStore returns a store seeded with the given users.
func NewStore(seed map[string]*User) *Store {
	if seed == nil {
		seed = map[string]*User{}
	}
	return &Store{users: seed}
}

// Get returns a copy of the stored user and whether it existed.
func (s *Store) Get(id string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return User{}, false
	}
	return *u, true
}

// PatchHandler decodes a UserPatch body and applies it to the user named by the
// "id" query parameter, returning the updated user as JSON.
func (s *Store) PatchHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")

	var patch UserPatch
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&patch); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	u, ok := s.users[id]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	patch.Apply(u)
	updated := *u
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(updated)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/patchuser"
)

func main() {
	u := patchuser.User{Name: "alice", Active: true, Age: 30}

	// Client sends only a new name; Active and Age must survive untouched.
	name := "alicia"
	patchuser.UserPatch{Name: &name}.Apply(&u)
	fmt.Printf("after name patch: %+v\n", u)

	// Client explicitly deactivates: false must be applied, not ignored.
	active := false
	patchuser.UserPatch{Active: &active}.Apply(&u)
	fmt.Printf("after active=false patch: %+v\n", u)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after name patch: {Name:alicia Active:true Age:30}
after active=false patch: {Name:alicia Active:false Age:30}
```

### Tests

The tests are the whole point of the pointer fields: the same `active` field is
driven three ways — absent, explicitly false, explicitly true — through the real
HTTP handler with `httptest`, asserting that only intended fields change and that
`false` is applied rather than swallowed.

Create `patch_test.go`:

```go
package patchuser

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func seededStore() *Store {
	return NewStore(map[string]*User{
		"u1": {Name: "alice", Active: true, Age: 30},
	})
}

func TestPatchSemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want User
	}{
		{
			name: "absent active leaves it unchanged",
			body: `{"name":"alicia"}`,
			want: User{Name: "alicia", Active: true, Age: 30},
		},
		{
			name: "explicit false is applied",
			body: `{"active":false}`,
			want: User{Name: "alice", Active: false, Age: 30},
		},
		{
			name: "explicit zero age is applied",
			body: `{"age":0}`,
			want: User{Name: "alice", Active: true, Age: 0},
		},
		{
			name: "null is treated as absent",
			body: `{"name":null}`,
			want: User{Name: "alice", Active: true, Age: 30},
		},
		{
			name: "real values applied together",
			body: `{"name":"bob","active":false,"age":41}`,
			want: User{Name: "bob", Active: false, Age: 41},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := seededStore()
			req := httptest.NewRequest(http.MethodPatch, "/user?id=u1", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()

			s.PatchHandler(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var got User
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("user = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPatchUnknownUser(t *testing.T) {
	t.Parallel()

	s := seededStore()
	req := httptest.NewRequest(http.MethodPatch, "/user?id=missing", strings.NewReader(`{"name":"x"}`))
	rec := httptest.NewRecorder()

	s.PatchHandler(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestApplyAbsentIsNoOp(t *testing.T) {
	t.Parallel()

	u := User{Name: "alice", Active: true, Age: 30}
	var empty UserPatch // all nil
	empty.Apply(&u)
	if (u != User{Name: "alice", Active: true, Age: 30}) {
		t.Fatalf("empty patch mutated user: %+v", u)
	}
}
```

## Review

The handler is correct when a field mutates the stored user exactly when its
pointer is non-nil, and only then. The decisive test is "explicit false is
applied": a value-field DTO would decode `{"active":false}` and an omitted
`active` identically, so this case is what proves the pointer field is doing its
job. "Absent leaves unchanged" and "null treated as absent" pin the nil branch.
`TestApplyAbsentIsNoOp` proves an all-nil patch touches nothing.

The trap avoided is collapsing absent and zero into one fact by decoding into
value fields, which corrupts PATCH by deactivating users who only meant to rename
themselves.

## Resources

- [encoding/json](https://pkg.go.dev/encoding/json#Decoder.Decode) — how a missing key leaves a pointer field nil and a present key allocates it.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRequest` and `NewRecorder` for handler tests.
- [RFC 5789: PATCH Method for HTTP](https://www.rfc-editor.org/rfc/rfc5789) — the semantics of a partial update.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-config-defaults-nil-guard.md](02-config-defaults-nil-guard.md) | Next: [04-typed-nil-interface-error-trap.md](04-typed-nil-interface-error-trap.md)
