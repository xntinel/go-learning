# Exercise 5: A PATCH handler distinguishing absent vs null vs empty list

This is the canonical place where nil-vs-empty stops being academic and changes
production behavior. A PATCH body's `tags` field has four meanings — key absent
means leave unchanged, `null` means clear, `[]` means set to explicit empty, and
`[...]` means replace — and a plain `[]string` field cannot tell the first two
apart. This exercise models the tri-state with `json.RawMessage` and applies the
right mutation for each.

This module is fully self-contained: its own `go mod init`, its own `profile`
package, its own demo and tests.

## What you'll build

```text
patchprofile/                 independent module: example.com/patchprofile
  go.mod
  profile/profile.go          Profile, Apply (tri-state via json.RawMessage), ErrDecode sentinel
  profile/profile_test.go     four-state table, absent!=null guard, decode-error test
  cmd/demo/main.go            applies the four body shapes to a seeded profile
```

Files: `profile/profile.go`, `profile/profile_test.go`, `cmd/demo/main.go`.
Implement: `Profile.Apply(body []byte)` that decodes into a `json.RawMessage`
`tags` field and mutates `Profile.Tags` per the four request shapes; a sentinel
`ErrDecode` wrapped with `%w`.
Test: a four-row table pinning the stored value for `{}`, `{"tags":null}`,
`{"tags":[]}`, and `{"tags":["x"]}`; a guard that absent is never conflated with
null; a decode-error test asserting `errors.Is(err, ErrDecode)`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/patchprofile/profile ~/go-exercises/patchprofile/cmd/demo
cd ~/go-exercises/patchprofile
go mod init example.com/patchprofile
```

### Why a plain slice — and even a pointer — is not enough

Decode `{}` and `{"tags":null}` into a struct with a plain `Tags []string` field
and you get the same thing both times: a nil slice, because in the first case
`Unmarshal` never touched the field and in the second it set it to nil. The
information "was the key present" is gone. A pointer `*[]string` is only a partial
rescue: `Unmarshal` leaves it nil for an absent key *and* sets it to nil for an
explicit `null`, so it still cannot separate absent from null — it only tells you
"present with a value" apart from "absent-or-null."

`json.RawMessage` recovers the full picture because it captures the raw bytes of
the field exactly as they appeared, or captures nothing if the key was absent.
An absent key leaves the `RawMessage` length zero; `null` gives it the four bytes
`null`; `[]` gives it `[]`; `["x"]` gives it the element bytes. Four inputs, four
distinguishable states. `Apply` switches on them: length zero returns without
touching `Tags`; the bytes `null` (trimmed of any surrounding whitespace) clear
`Tags` to nil; anything else is a JSON array that we unmarshal into a `[]string`
and store — normalizing a decoded nil to a non-nil empty slice so that `[]` is
stored as explicit-empty rather than collapsing back into nil.

That last normalization is what makes the empty case meaningful: after
`{"tags":[]}`, `Profile.Tags` is a non-nil length-zero slice, which a later
re-serialization renders as `[]`, exactly matching the client's intent to set an
empty list. Errors are wrapped with `%w` around a package sentinel so a caller
can classify a malformed body with `errors.Is`.

Create `profile/profile.go`:

```go
package profile

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ErrDecode wraps any JSON decoding failure so callers can match with errors.Is.
var ErrDecode = fmt.Errorf("patch: decode")

// Profile is the stored resource. Tags follows the nil-vs-empty contract:
// nil means "no tags", a non-nil empty slice means "explicitly cleared to none".
type Profile struct {
	Tags []string
}

// patchBody uses json.RawMessage for Tags so the handler can tell four states
// apart: absent (len 0), null ("null"), empty ("[]"), and populated. A plain
// []string field would collapse absent and null into the same zero value.
type patchBody struct {
	Tags json.RawMessage `json:"tags"`
}

// Apply mutates the profile per the PATCH semantics of the tags field:
//
//	key absent   -> leave Tags unchanged
//	"tags":null  -> clear Tags to nil
//	"tags":[]    -> set Tags to an explicit empty (non-nil) slice
//	"tags":[...] -> replace Tags with the given elements
func (p *Profile) Apply(body []byte) error {
	var req patchBody
	if err := json.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("%w: %v", ErrDecode, err)
	}

	switch {
	case len(req.Tags) == 0:
		// Key absent: json.Unmarshal never touched the field. Leave unchanged.
		return nil
	case bytes.Equal(bytes.TrimSpace(req.Tags), []byte("null")):
		// Explicit null: clear to nil.
		p.Tags = nil
		return nil
	default:
		// Explicit array (possibly empty): replace. Normalize a decoded nil to a
		// non-nil empty slice so "[]" is stored as explicit-empty, not nil.
		var tags []string
		if err := json.Unmarshal(req.Tags, &tags); err != nil {
			return fmt.Errorf("%w: %v", ErrDecode, err)
		}
		if tags == nil {
			tags = []string{}
		}
		p.Tags = tags
		return nil
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/patchprofile/profile"
)

func main() {
	for _, body := range []string{`{}`, `{"tags":null}`, `{"tags":[]}`, `{"tags":["go","api"]}`} {
		p := profile.Profile{Tags: []string{"initial"}}
		if err := p.Apply([]byte(body)); err != nil {
			fmt.Printf("%-24s error: %v\n", body, err)
			continue
		}
		fmt.Printf("%-24s -> tags=%v nil=%v len=%d\n", body, p.Tags, p.Tags == nil, len(p.Tags))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{}                       -> tags=[initial] nil=false len=1
{"tags":null}            -> tags=[] nil=true len=0
{"tags":[]}              -> tags=[] nil=false len=0
{"tags":["go","api"]}    -> tags=[go api] nil=false len=2
```

Note the two middle rows: `%v` prints both a nil and an empty slice as `[]`, but
the `nil=` column shows they are different stored values — precisely the
distinction the handler exists to preserve.

### Tests

`TestApplyTriState` is the four-row table that pins the stored `Tags` for each
body, asserting both the contents and the nil-ness. `TestApplyDoesNotConflateAbsentWithNull`
is the sharp one: it applies `{}` and `{"tags":null}` to two profiles seeded
identically and asserts the first is left unchanged while the second is cleared —
the exact bug a plain `[]string` or `*[]string` field would introduce.
`TestApplyDecodeError` feeds a malformed body and asserts the error wraps
`ErrDecode`.

Create `profile/profile_test.go`:

```go
package profile

import (
	"errors"
	"slices"
	"testing"
)

func TestApplyTriState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		body     string
		wantTags []string
		wantNil  bool
	}{
		{"absent leaves unchanged", `{}`, []string{"initial"}, false},
		{"null clears to nil", `{"tags":null}`, nil, true},
		{"empty sets explicit empty", `{"tags":[]}`, []string{}, false},
		{"array replaces", `{"tags":["go","api"]}`, []string{"go", "api"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := Profile{Tags: []string{"initial"}}
			if err := p.Apply([]byte(tc.body)); err != nil {
				t.Fatalf("Apply(%s): %v", tc.body, err)
			}
			if (p.Tags == nil) != tc.wantNil {
				t.Fatalf("Tags nil = %v, want %v", p.Tags == nil, tc.wantNil)
			}
			if !slices.Equal(p.Tags, tc.wantTags) {
				t.Fatalf("Tags = %v, want %v", p.Tags, tc.wantTags)
			}
		})
	}
}

func TestApplyDoesNotConflateAbsentWithNull(t *testing.T) {
	t.Parallel()
	absent := Profile{Tags: []string{"keep"}}
	if err := absent.Apply([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	null := Profile{Tags: []string{"keep"}}
	if err := null.Apply([]byte(`{"tags":null}`)); err != nil {
		t.Fatal(err)
	}
	if absent.Tags == nil {
		t.Fatal("absent must leave Tags unchanged (non-nil)")
	}
	if null.Tags != nil {
		t.Fatal("null must clear Tags to nil")
	}
}

func TestApplyDecodeError(t *testing.T) {
	t.Parallel()
	p := Profile{}
	err := p.Apply([]byte(`{"tags": not-json}`))
	if !errors.Is(err, ErrDecode) {
		t.Fatalf("err = %v, want wrap of ErrDecode", err)
	}
}
```

## Review

The handler is correct when all four bodies map to distinct stored values:
`{}` leaves `Tags` untouched, `null` clears it to nil, `[]` sets a non-nil empty
slice, and `["x"]` replaces it. The load-bearing idea is that `json.RawMessage`
is what preserves the absent-vs-null distinction that both a plain slice and a
bare pointer throw away, and that the empty case must be normalized to non-nil so
it round-trips back to `[]`. Wrapping decode failures with `%w` around `ErrDecode`
lets the transport layer classify a bad body without string-matching. Get this
field wrong and a client that sends `null` to clear a value silently has its
update ignored, or a client that omits the field accidentally wipes it — both
real incidents that this tri-state model prevents.

## Resources

- [encoding/json — RawMessage](https://pkg.go.dev/encoding/json#RawMessage) — deferred decoding that preserves the raw bytes and the absent/null distinction.
- [encoding/json — Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal) — how absent keys and null are decoded into Go values.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel error.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-append-shared-backing-array-bug.md](06-append-shared-backing-array-bug.md)
