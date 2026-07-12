# Exercise 2: A JSON PATCH Handler That Distinguishes Absent From Zero

The classic PATCH bug: a client sends `{"active": false}` to deactivate a user,
and the server cannot tell it apart from a body that omitted `active` entirely —
so it either ignores the field or clobbers unrelated fields. The fix is to model
"absent" and "present-and-zero" as different states with pointer fields.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
patch/                     independent module: example.com/patch
  go.mod
  patch.go                 User, userPatch (pointer fields), Handler, apply
  cmd/
    demo/
      main.go              sends three PATCH bodies through the handler
  patch_test.go            table tests for apply + httptest handler tests
```

Files: `patch.go`, `cmd/demo/main.go`, `patch_test.go`.
Implement: a `Handler` for `PATCH /user` whose request DTO uses `*string`/`*bool`/`*int` so a nil pointer means "not provided" and a non-nil pointer means "set to this value, including the zero value"; apply only provided fields onto the stored entity.
Test: an omitted field leaves the entity unchanged; explicit `false`/`0`/`""` overwrite to the zero value; malformed JSON returns 400.
Verify: `go test -count=1 -race ./...`

## Why pointer fields, and nothing else, work here

`encoding/json` leaves a struct field at its zero value when the corresponding
JSON key is absent — and it also sets a field to its zero value when the key is
present with a zero value. For a `bool` field those two cases are
indistinguishable after `Unmarshal`: both leave `false`. That is the entire bug.
A pointer field breaks the tie because `json` only allocates and assigns through
the pointer when the key is present: an omitted key leaves the pointer `nil`, and
a present `"active": false` leaves a non-nil `*bool` addressing `false`. So `nil`
means "the client did not mention this field" and non-nil means "the client set
this field, to exactly this value".

`apply` then merges only the provided fields: for each pointer, `if p.X != nil {
u.X = *p.X }`. A nil pointer is skipped and the existing value survives; a
non-nil pointer overwrites — even when it points at the zero value. That is what
lets `{"active": false}` actually deactivate a user while `{}` changes nothing.

One more production detail: decoding rejects unknown fields with
`DisallowUnknownFields`, so a typo like `{"activ": false}` is a 400 rather than a
silently-ignored no-op — the failure mode that makes "my PATCH did nothing"
tickets so painful. Malformed JSON is wrapped in a sentinel error so the handler
can map it to `400 Bad Request` and callers/tests can assert it with
`errors.Is`.

Create `patch.go`:

```go
package patch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// ErrInvalidPatch wraps any failure to decode a PATCH body.
var ErrInvalidPatch = errors.New("invalid patch body")

// User is the stored entity.
type User struct {
	Name   string `json:"name"`
	Email  string `json:"email"`
	Active bool   `json:"active"`
	Age    int    `json:"age"`
}

// userPatch is the request DTO. Every field is a pointer: nil means "not
// provided", non-nil means "set to this value" (including the zero value).
type userPatch struct {
	Name   *string `json:"name"`
	Email  *string `json:"email"`
	Active *bool   `json:"active"`
	Age    *int    `json:"age"`
}

// apply returns u with only the provided fields of p overwritten.
func apply(u User, p userPatch) User {
	if p.Name != nil {
		u.Name = *p.Name
	}
	if p.Email != nil {
		u.Email = *p.Email
	}
	if p.Active != nil {
		u.Active = *p.Active
	}
	if p.Age != nil {
		u.Age = *p.Age
	}
	return u
}

func decodePatch(data []byte) (userPatch, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p userPatch
	if err := dec.Decode(&p); err != nil {
		return userPatch{}, fmt.Errorf("%w: %v", ErrInvalidPatch, err)
	}
	return p, nil
}

// Handler stores one User and applies PATCH requests to it.
type Handler struct {
	mu   sync.Mutex
	user User
}

// NewHandler returns a Handler seeded with an initial user.
func NewHandler(initial User) *Handler {
	return &Handler{user: initial}
}

// Current returns the current stored user.
func (h *Handler) Current() User {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.user
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	p, err := decodePatch(data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	h.user = apply(h.user, p)
	updated := h.user
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(updated)
}
```

## The runnable demo

The demo drives the handler with `httptest` — no real socket — and prints the
status and body for three bodies: an explicit `false`, an empty object, and a
single-field update. Watch how `{"active":false}` flips `active` while `{}`
leaves everything intact.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"example.com/patch"
)

func main() {
	h := patch.NewHandler(patch.User{
		Name:   "Ada",
		Email:  "ada@example.com",
		Active: true,
		Age:    41,
	})

	do := func(body string) {
		req := httptest.NewRequest(http.MethodPatch, "/user", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		fmt.Printf("PATCH %s -> %d %s", body, rec.Code, rec.Body.String())
	}

	do(`{"active":false}`)
	do(`{}`)
	do(`{"name":"Ada L."}`)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
PATCH {"active":false} -> 200 {"name":"Ada","email":"ada@example.com","active":false,"age":41}
PATCH {} -> 200 {"name":"Ada","email":"ada@example.com","active":false,"age":41}
PATCH {"name":"Ada L."} -> 200 {"name":"Ada L.","email":"ada@example.com","active":false,"age":41}
```

## Tests

`TestApply` is the core table: it asserts the merged entity for an omitted field,
an explicit `false`, an explicit `0`, and an explicit `""`, proving that a
present zero value overwrites while an absent field is preserved. `TestHandler`
drives the same distinction through the HTTP path with `httptest` and checks that
malformed JSON and an unknown field both return `400` with the sentinel error.

Create `patch_test.go`:

```go
package patch

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApply(t *testing.T) {
	t.Parallel()

	base := User{Name: "Ada", Email: "ada@example.com", Active: true, Age: 41}

	tests := []struct {
		name string
		body string
		want User
	}{
		{
			name: "empty patch changes nothing",
			body: `{}`,
			want: base,
		},
		{
			name: "explicit false overwrites active",
			body: `{"active":false}`,
			want: User{Name: "Ada", Email: "ada@example.com", Active: false, Age: 41},
		},
		{
			name: "explicit zero overwrites age",
			body: `{"age":0}`,
			want: User{Name: "Ada", Email: "ada@example.com", Active: true, Age: 0},
		},
		{
			name: "explicit empty string overwrites email",
			body: `{"email":""}`,
			want: User{Name: "Ada", Email: "", Active: true, Age: 41},
		},
		{
			name: "single field update leaves the rest",
			body: `{"name":"Ada L."}`,
			want: User{Name: "Ada L.", Email: "ada@example.com", Active: true, Age: 41},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, err := decodePatch([]byte(tt.body))
			if err != nil {
				t.Fatalf("decodePatch(%s) error: %v", tt.body, err)
			}
			if got := apply(base, p); got != tt.want {
				t.Fatalf("apply = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestHandler(t *testing.T) {
	t.Parallel()

	t.Run("explicit false deactivates", func(t *testing.T) {
		t.Parallel()
		h := NewHandler(User{Name: "Ada", Active: true})
		req := httptest.NewRequest(http.MethodPatch, "/user", strings.NewReader(`{"active":false}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := h.Current(); got.Active {
			t.Fatalf("Active = true, want false after explicit patch")
		}
	})

	t.Run("malformed json is 400", func(t *testing.T) {
		t.Parallel()
		h := NewHandler(User{Name: "Ada"})
		req := httptest.NewRequest(http.MethodPatch, "/user", strings.NewReader(`{"active":`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("unknown field is rejected", func(t *testing.T) {
		t.Parallel()
		_, err := decodePatch([]byte(`{"activ":false}`))
		if !errors.Is(err, ErrInvalidPatch) {
			t.Fatalf("error = %v, want ErrInvalidPatch", err)
		}
	})
}
```

## Review

The handler is correct when a present zero value and an absent field produce
different merges: `{"active":false}` must flip `active`, and `{}` must be a true
no-op across every field. If a `bool`/`int`/`string` field in the DTO is not a
pointer, that distinction collapses and the endpoint can never clear a field —
the exact bug this exercise exists to prevent. Keep `DisallowUnknownFields` on so
a mistyped key fails loudly instead of silently doing nothing, and keep the
malformed-body error wrapped with `%w` so the handler maps it to `400` and tests
assert it with `errors.Is`. Run `go test -race` because the handler shares one
`User` under a mutex.

## Resources

- [`encoding/json`](https://pkg.go.dev/encoding/json) — Unmarshal semantics for absent vs zero-valued keys.
- [`json.Decoder.DisallowUnknownFields`](https://pkg.go.dev/encoding/json#Decoder.DisallowUnknownFields) — reject unexpected keys.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest`/`NewRecorder` for handler tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-zero-value-metrics-collector.md](01-zero-value-metrics-collector.md) | Next: [03-nil-map-config-merge.md](03-nil-map-config-merge.md)
