# Exercise 2: Stop Internal State Leaking Through a JSON API Response

The export rule is not only about what callers can name; it is also what
`encoding/json` can see. `json.Marshal` serializes exported fields and silently
ignores unexported ones, which makes the visibility boundary double as a
wire-safety boundary: a password hash or an internal row id held in an unexported
field physically cannot leak over the API. This exercise builds an HTTP-response
DTO that leans on that rule, and proves both its safety property and its matching
footgun.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
userapi/                   independent module: example.com/userapi
  go.mod                   go 1.26
  userapi.go               UserResponse (exported+tagged fields, unexported bookkeeping); brokenReport
  cmd/
    demo/
      main.go              marshal a populated UserResponse, print the JSON
  userapi_test.go          asserts unexported fields never serialize; omitempty/omitzero; json:"-"
```

- Files: `userapi.go`, `cmd/demo/main.go`, `userapi_test.go`.
- Implement: a `UserResponse` DTO whose exported fields carry `json` tags and whose unexported fields (`passwordHash`, `rowID`, `computedAt`) hold internal bookkeeping; use `omitempty` and `omitzero` (Go 1.24) correctly, and `json:"-"` to drop an exported field.
- Test: marshal a fully populated struct and assert no unexported field appears; assert `omitempty`/`omitzero` fields are absent when zero and present when set; round-trip decode to confirm unexported fields never populate from input; show the footgun with `brokenReport`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/userapi/cmd/demo
cd ~/go-exercises/userapi
go mod init example.com/userapi
go mod edit -go=1.26
```

### Why the tags are the way they are

`ID` and `Email` are always present, so they carry a plain `json:"id"` /
`json:"email"` tag. `Name` is optional and should be omitted when empty, so it uses
`omitempty`; that works because `Name` is a string and `omitempty` omits empty
strings. `DeletedAt` is a `time.Time` that is meaningful only when the user is
soft-deleted, so it must be omitted when unset, and here `omitempty` would be a bug:
a struct is never "empty", so `omitempty` on a `time.Time` does nothing and a zero
timestamp serializes as `"0001-01-01T00:00:00Z"`. The correct tag is `omitzero`
(added in Go 1.24), which omits the field when the type's `IsZero()` method reports
zero, and `time.Time` has exactly that method. `InternalNote` is exported (so other
code in the package can set it) but must never reach the wire, so it carries
`json:"-"`, which force-drops it regardless of value.

The unexported fields, `passwordHash`, `rowID`, `computedAt`, are the safety story.
They hold internal bookkeeping the server needs but the client must never see. We do
not add them to a "do not serialize" list; the language's visibility rule is the list.
`json.Marshal` cannot access an unexported field, so there is no tag, no option, and no
way for these to appear in the JSON, now or after any future refactor of the tags.

The footgun lives in `brokenReport`: its author meant to serialize a `total`, but wrote
it lowercase. `json.Marshal` drops it silently and returns `{}` with no error. The only
defense is a test that asserts the wire format, which is exactly what
`TestBrokenReportSilentlyDropsField` does.

Create `userapi.go`:

```go
package userapi

import "time"

// UserResponse is the DTO returned by the user HTTP API. Exported fields are the
// wire contract; unexported fields hold internal state that json.Marshal cannot
// see, so it can never leak over the network.
type UserResponse struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name,omitempty"`     // omit when empty string
	CreatedAt    time.Time `json:"createdAt"`          // always present
	DeletedAt    time.Time `json:"deletedAt,omitzero"` // omit when zero (IsZero); omitempty would NOT work here
	InternalNote string    `json:"-"`                  // exported but never serialized

	// Unexported bookkeeping: json.Marshal cannot access these, so they never
	// appear on the wire and can never be populated from input.
	passwordHash string
	rowID        int64
	computedAt   time.Time
}

// NewUserResponse builds a response, stashing internal bookkeeping in the
// unexported fields. computedAt records when the DTO was assembled server-side.
func NewUserResponse(id, email, name, passwordHash string, rowID int64, createdAt time.Time) UserResponse {
	return UserResponse{
		ID:           id,
		Email:        email,
		Name:         name,
		CreatedAt:    createdAt,
		InternalNote: "assembled by user-service",
		passwordHash: passwordHash,
		rowID:        rowID,
		computedAt:   time.Now(),
	}
}

// RowID exposes the internal row id to same-package callers without putting it on
// the wire; it is here so the field is not dead and to show that "readable inside
// the package" and "serialized" are independent decisions.
func (u UserResponse) RowID() int64 { return u.rowID }

// PasswordHashSet reports whether an internal hash is stored, again without ever
// exposing the hash itself over JSON.
func (u UserResponse) PasswordHashSet() bool { return u.passwordHash != "" }

// brokenReport shows the footgun: total is lowercase, so json.Marshal silently
// omits it and the payload is missing data with no error.
type brokenReport struct {
	total int
}
```

### The runnable demo

The demo marshals a fully populated `UserResponse` and prints the JSON. Because the
unexported fields cannot serialize, the `omitempty` `Name` is set so it appears, the
`omitzero` `DeletedAt` is left zero so it is omitted, and `InternalNote` is dropped by
`json:"-"`, the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"time"

	"example.com/userapi"
)

func main() {
	u := userapi.NewUserResponse(
		"u_1",
		"ada@example.com",
		"Ada",
		"$argon2id$v=19$secret",
		42,
		time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	)

	b, err := json.Marshal(u)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(b))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"id":"u_1","email":"ada@example.com","name":"Ada","createdAt":"2026-01-02T03:04:05Z"}
```

Note what is absent: no `passwordHash`, no `rowID`, no `computedAt` (unexported), no
`deletedAt` (omitzero, still zero), no `InternalNote` (`json:"-"`).

### Tests

The tests assert the wire format directly. `TestUnexportedFieldsNeverSerialize`
marshals a fully populated response and asserts none of the unexported field names
appear in the JSON. `TestOmitEmptyAndOmitZero` checks both directions: `Name` and
`DeletedAt` are absent when zero and present when set, proving the tags do what we
claim. `TestDashFieldExcluded` confirms `InternalNote` never appears even when set.
`TestRoundTripDoesNotPopulateUnexported` decodes a crafted JSON that includes keys
matching the unexported field names and proves they do not populate the unexported
fields. `TestBrokenReportSilentlyDropsField` demonstrates the footgun.

Create `userapi_test.go`:

```go
package userapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUnexportedFieldsNeverSerialize(t *testing.T) {
	t.Parallel()

	u := NewUserResponse("u_1", "ada@example.com", "Ada", "secret-hash", 99, time.Now())
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	for _, forbidden := range []string{"passwordHash", "secret-hash", "rowID", "99", "computedAt", "InternalNote", "assembled by"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("JSON leaked %q: %s", forbidden, got)
		}
	}

	// Decode back into a generic map and confirm only the intended keys exist.
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"passwordHash", "rowID", "computedAt", "InternalNote"} {
		if _, ok := m[forbidden]; ok {
			t.Fatalf("decoded map contains forbidden key %q", forbidden)
		}
	}
}

func TestOmitEmptyAndOmitZero(t *testing.T) {
	t.Parallel()

	// Zero Name and zero DeletedAt: both must be absent.
	empty := UserResponse{ID: "u_2", Email: "e@x.com", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	b, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, "name") || strings.Contains(s, "deletedAt") {
		t.Fatalf("omit tags failed, name/deletedAt present: %s", s)
	}

	// Set both: both must appear.
	full := empty
	full.Name = "Bob"
	full.DeletedAt = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	b2, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	s2 := string(b2)
	if !strings.Contains(s2, `"name":"Bob"`) {
		t.Fatalf("name missing when set: %s", s2)
	}
	if !strings.Contains(s2, `"deletedAt":"2026-06-01T12:00:00Z"`) {
		t.Fatalf("deletedAt missing when set: %s", s2)
	}
}

func TestDashFieldExcluded(t *testing.T) {
	t.Parallel()

	u := UserResponse{ID: "u_3", Email: "e@x.com", InternalNote: "do-not-ship"}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "do-not-ship") {
		t.Fatalf(`json:"-" field leaked: %s`, b)
	}
}

func TestRoundTripDoesNotPopulateUnexported(t *testing.T) {
	t.Parallel()

	// Attacker-supplied JSON tries to set the internal fields by name.
	input := `{"id":"u_4","email":"e@x.com","passwordHash":"injected","rowID":777,"computedAt":"2000-01-01T00:00:00Z"}`
	var u UserResponse
	if err := json.Unmarshal([]byte(input), &u); err != nil {
		t.Fatal(err)
	}
	if u.PasswordHashSet() {
		t.Fatal("unexported passwordHash was populated from input")
	}
	if u.RowID() != 0 {
		t.Fatalf("unexported rowID was populated from input: %d", u.RowID())
	}
}

// TestBrokenReportSilentlyDropsField documents the footgun: a lowercase field the
// author intended to serialize is dropped with no error.
func TestBrokenReportSilentlyDropsField(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(brokenReport{total: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != "{}" {
		t.Fatalf("expected {} because total is unexported, got %s", got)
	}
}
```

## Review

The safety property holds when the JSON of a fully populated `UserResponse` contains
none of the unexported field names or their values, which the map-decode assertion
proves, and the round-trip test proves the inverse: crafted input keyed to the internal
field names cannot populate them either, so an attacker cannot smuggle an internal row
id in through the decoder. The tag choices are load-bearing: `omitempty` on `Name`
works because it is a string, but `omitempty` on `DeletedAt` would silently fail because
a struct is never empty, which is why `DeletedAt` uses `omitzero`; the `TestOmitEmptyAndOmitZero`
assertions fail loudly if either tag is wrong. The footgun test is the habit worth
keeping: whenever a field must appear on the wire, assert it in a test, because a missed
capital letter produces an incomplete payload and no compiler or runtime error.

## Resources

- [`encoding/json`: struct field tags, omitempty and omitzero](https://pkg.go.dev/encoding/json) — the marshaling rules, including that only exported fields are encoded.
- [Go 1.24 release notes: encoding/json omitzero](https://go.dev/doc/go1.24#encodingjsonpkgencodingjson) — where `omitzero` was introduced.
- [`time.Time.IsZero`](https://pkg.go.dev/time#Time.IsZero) — the method `omitzero` calls on a `time.Time`.
- [`json.Marshal`](https://pkg.go.dev/encoding/json#Marshal) — the exact behavior for unexported and tagged fields.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-ttl-cache-exported-api.md](01-ttl-cache-exported-api.md) | Next: [03-blackbox-package-api-surface.md](03-blackbox-package-api-surface.md)
