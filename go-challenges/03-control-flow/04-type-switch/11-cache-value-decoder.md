# Exercise 11: Decode a Generic Cache Value into a Typed Struct

**Nivel: Intermedio** — validacion rapida (un test corto).

A generic cache client's `Get` returns `any`. An in-process cache hit hands
back the concrete Go value with no serialization at all; a remote cache hit
hands back the serialized form as `[]byte` or `string`, depending on which of
two client libraries fetched it, and that form must be JSON-decoded. A miss is
`nil`. One decoder must handle all three without the caller caring which path
served the value.

## What you'll build

```text
cachedecode/                independent module: example.com/cachedecode
  go.mod                     go 1.24
  cachedecode.go             DecodeUser(v any) (User, error)
  cachedecode_test.go        one table test over hit/miss/malformed shapes
```

- Implement: `DecodeUser(v any) (User, error)`, handling `nil` (miss), the
  concrete `User` (in-process hit), `[]byte` and `string` (remote hit,
  JSON-encoded either way).
- Test: a cache miss, an in-process hit, both remote-hit encodings, malformed
  bytes, and an unsupported type.

Set up the module:

```bash
go mod edit -go=1.24
```

Create `cachedecode.go`:

```go
package cachedecode

import (
	"encoding/json"
	"errors"
	"fmt"
)

// User is the domain value stored and retrieved through the cache.
type User struct {
	ID   string
	Name string
}

// ErrCacheMiss is returned when the cache held no value for the key.
var ErrCacheMiss = errors.New("cache miss")

// DecodeUser normalizes whatever a generic cache client's Get returns into a
// User. An in-process cache hit returns the concrete User value directly with
// no serialization round trip. A remote cache hit returns the serialized form
// as []byte or string (two client libraries in this codebase disagree on
// which), which must be JSON-decoded. A nil value means the key was absent.
func DecodeUser(v any) (User, error) {
	switch c := v.(type) {
	case nil:
		return User{}, ErrCacheMiss
	case User:
		return c, nil
	case []byte:
		var u User
		if err := json.Unmarshal(c, &u); err != nil {
			return User{}, fmt.Errorf("decode cached bytes: %w", err)
		}
		return u, nil
	case string:
		var u User
		if err := json.Unmarshal([]byte(c), &u); err != nil {
			return User{}, fmt.Errorf("decode cached string: %w", err)
		}
		return u, nil
	default:
		return User{}, fmt.Errorf("cannot decode cache value of type %T", v)
	}
}
```

Create `cachedecode_test.go`:

```go
package cachedecode

import (
	"errors"
	"testing"
)

func TestDecodeUser(t *testing.T) {
	t.Parallel()
	want := User{ID: "u-1", Name: "Ada"}
	tests := []struct {
		name    string
		value   any
		want    User
		wantErr error
	}{
		{"cache miss", nil, User{}, ErrCacheMiss},
		{"in-process hit", want, want, nil},
		{"remote hit bytes", []byte(`{"ID":"u-1","Name":"Ada"}`), want, nil},
		{"remote hit string", `{"ID":"u-1","Name":"Ada"}`, want, nil},
		{"malformed bytes", []byte(`{`), User{}, nil},
		{"unsupported type", 42, User{}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecodeUser(tt.value)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("DecodeUser(%v) err = %v, want %v", tt.value, err, tt.wantErr)
				}
				return
			}
			if tt.name == "malformed bytes" || tt.name == "unsupported type" {
				if err == nil {
					t.Fatalf("DecodeUser(%v) = nil error, want error", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeUser(%v) unexpected error: %v", tt.value, err)
			}
			if got != tt.want {
				t.Fatalf("DecodeUser(%v) = %+v, want %+v", tt.value, got, tt.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The concrete `case User` is what makes the in-process fast path free of
serialization: nothing about the switch forces every hit through JSON, only
the remote-cache branches do. `case nil` guards the miss before any of the
type cases run, so a caller cannot mistake "key absent" for "value decoded to
the zero value." Both wire encodings — `[]byte` and `string` — funnel through
the same `json.Unmarshal`, so adding a third client library that returns, say,
a `bytes.Buffer`, is one more case rather than a rewrite.

## Resources

- [encoding/json.Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal)
- [Effective Go: Type switches](https://go.dev/doc/effective_go#type_switch)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-message-queue-payload-dispatcher.md](10-message-queue-payload-dispatcher.md) | Next: [12-api-response-field-normalizer.md](12-api-response-field-normalizer.md)
