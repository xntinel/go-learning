# Exercise 3: v1-vs-v2 Semantics and a Deliberate Compatibility Shim

The risk in adopting `encoding/json/v2` is not that it is worse — it is that its
stricter defaults reject payloads v1 accepted, and if you do not know exactly which
ones, a lenient integration breaks in production. This exercise builds a divergence
harness that pins down each changed default with a test, plus two named option
bundles: a strict bundle for internal trust boundaries and a deliberate
compatibility bundle that re-enables v1 leniency only where an external boundary
demands it.

The files carry the `//go:build goexperiment.jsonv2` constraint;
`encoding/json/v2` and `encoding/json/jsontext` exist only under
`GOEXPERIMENT=jsonv2`.

## What you'll build

```text
jsoncompat/                   independent module: example.com/jsoncompat
  go.mod                      go 1.26
  compat.go                   User, Server, Record; StrictOptions, CompatOptions; Marshal/Unmarshal helpers
  cmd/
    demo/
      main.go                 runnable demo printing each v1-vs-v2 divergence
  compat_test.go              divergence table, omitzero, format tag, MarshalToFunc example
```

Files: `compat.go`, `cmd/demo/main.go`, `compat_test.go`.
Implement: `StrictOptions()` (rejects unknown members) and `CompatOptions()` (case-insensitive, duplicate names, invalid UTF-8, `null` for nil collections), composed with `json.JoinOptions`; plus `UnmarshalUser`/`MarshalUser` helpers, an `omitzero` struct, and a `format`-tagged struct.
Test: a table where each row is a v1-vs-v2 divergence (duplicates, invalid UTF-8, case, unknown members, nil slice/map), asserting the strict and compat bundles behave as designed; `omitzero` and `format` output; an `Example` verifying `MarshalToFunc` emits the expected token stream.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/48-modern-go-language-and-stdlib/12-encoding-json-v2/03-v2-semantics-and-compat-shim/cmd/demo
cd go-solutions/48-modern-go-language-and-stdlib/12-encoding-json-v2/03-v2-semantics-and-compat-shim
go mod edit -go=1.26
export GOEXPERIMENT=jsonv2
```

### Options are values you compose, not flags you scatter

The design principle is that leniency lives in one place. `StrictOptions` and
`CompatOptions` each return a single `json.Options` built with `json.JoinOptions`,
and every call site passes one of the two. There is no global switch and no boolean
soup: the internal path uses the strict bundle, the one external endpoint that must
swallow a legacy vendor's payloads uses the compat bundle, and a reader can see the
exact leniency each boundary grants.

`StrictOptions` only needs `RejectUnknownMembers(true)`: duplicate names and invalid
UTF-8 are *already* rejected by the v2 defaults, so strict is mostly the default
plus one tightening. `CompatOptions` is where you spell out, item by item, every v1
behavior you are choosing to restore: `MatchCaseInsensitiveNames(true)` so
`{"Password":...}` binds a `password` field again; `AllowDuplicateNames(true)` and
`AllowInvalidUTF8(true)` (both `jsontext` options) to accept the malformed shapes v1
tolerated; and `FormatNilSliceAsNull(true)`/`FormatNilMapAsNull(true)` so a nil
slice or map marshals back to `null` instead of v2's `[]`/`{}`. Because marshal
ignores decode-only options and unmarshal ignores encode-only options, the same
bundle is safe to pass in either direction.

Each of these reverts a specific footgun, and naming them in one function is the
whole point: you can audit the compatibility surface in a code review instead of
grepping for stray `true`s. The divergences the harness pins down:

- Duplicate keys: v2 errors (`jsontext.ErrDuplicateName`); compat accepts, last wins.
- Invalid UTF-8: v2 errors; compat accepts.
- Case-differing key: v2 does not bind the field; compat binds it.
- Unknown member: v2 ignores it by default; strict rejects it.
- Nil slice/map: v2 marshals `[]`/`{}`; compat marshals `null`.

Create `compat.go`:

```go
//go:build goexperiment.jsonv2

package jsoncompat

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"time"
)

// User models a credential record whose field names collide with common
// attacker-supplied variants (differing case, duplicates).
type User struct {
	Name     string            `json:"name"`
	Password string            `json:"password"`
	Roles    []string          `json:"roles"`
	Meta     map[string]string `json:"meta"`
}

// Server shows omitzero: a zero Port or a false TLS is omitted from the wire,
// unlike omitempty, whose fuzzier "empty" test also drops other values.
type Server struct {
	Host string `json:"host"`
	Port int    `json:"port,omitzero"`
	TLS  bool   `json:"tls,omitzero"`
}

// Record shows the declarative format tag: time as RFC 3339, bytes as base64,
// moving representation out of a hand-written MarshalJSON method.
type Record struct {
	When time.Time `json:"when,format:RFC3339"`
	Raw  []byte    `json:"raw,format:base64"`
}

// StrictOptions is the bundle for an internal trust boundary: unknown members
// are rejected. Duplicate names and invalid UTF-8 are already rejected by the
// v2 defaults, so they need no explicit option here.
func StrictOptions() json.Options {
	return json.JoinOptions(
		json.RejectUnknownMembers(true),
	)
}

// CompatOptions is the bundle for an external boundary that must accept
// v1-lenient payloads: case-insensitive names, duplicate names (last wins),
// invalid UTF-8, and null for nil slices/maps.
func CompatOptions() json.Options {
	return json.JoinOptions(
		json.MatchCaseInsensitiveNames(true),
		json.FormatNilSliceAsNull(true),
		json.FormatNilMapAsNull(true),
		jsontext.AllowDuplicateNames(true),
		jsontext.AllowInvalidUTF8(true),
	)
}

// UnmarshalUser decodes data into a User with the given options.
func UnmarshalUser(data []byte, opts ...json.Options) (User, error) {
	var u User
	err := json.Unmarshal(data, &u, opts...)
	return u, err
}

// MarshalUser encodes u with the given options.
func MarshalUser(u User, opts ...json.Options) ([]byte, error) {
	return json.Marshal(u, opts...)
}
```

### The runnable demo

The demo prints one line per divergence, contrasting the v2 default against the
matching bundle. It shows a duplicate-key payload rejected by default but accepted
(last-wins) by compat, a mixed-case key that only binds under compat, an unknown
member that is silently ignored by default but rejected by strict, and a value with
nil collections marshaled as `[]`/`{}` by default versus `null` under compat.

Create `cmd/demo/main.go`:

```go
//go:build goexperiment.jsonv2

package main

import (
	"fmt"

	"example.com/jsoncompat"
)

func main() {
	// 1. Duplicate keys: default rejects, compat accepts (last wins).
	dup := []byte(`{"name":"a","name":"b"}`)
	_, errDefault := jsoncompat.UnmarshalUser(dup)
	u, _ := jsoncompat.UnmarshalUser(dup, jsoncompat.CompatOptions())
	fmt.Printf("dup: default_err=%v compat_name=%s\n", errDefault != nil, u.Name)

	// 2. Case-differing key: default does not bind, compat does.
	mixed := []byte(`{"Password":"x"}`)
	strict, _ := jsoncompat.UnmarshalUser(mixed)
	lenient, _ := jsoncompat.UnmarshalUser(mixed, jsoncompat.CompatOptions())
	fmt.Printf("case: strict_pw=%q compat_pw=%q\n", strict.Password, lenient.Password)

	// 3. Unknown member: default ignores, strict rejects.
	unk := []byte(`{"name":"a","extra":1}`)
	_, defErr := jsoncompat.UnmarshalUser(unk)
	_, strErr := jsoncompat.UnmarshalUser(unk, jsoncompat.StrictOptions())
	fmt.Printf("unknown: default_err=%v strict_err=%v\n", defErr != nil, strErr != nil)

	// 4. Nil slice/map: default emits [] and {}, compat emits null.
	empty := jsoncompat.User{Name: "a"}
	v2out, _ := jsoncompat.MarshalUser(empty)
	compatOut, _ := jsoncompat.MarshalUser(empty, jsoncompat.CompatOptions())
	fmt.Printf("nil-v2: %s\n", v2out)
	fmt.Printf("nil-compat: %s\n", compatOut)
}
```

Run it:

```bash
GOEXPERIMENT=jsonv2 go run ./cmd/demo
```

Expected output:

```
dup: default_err=true compat_name=b
case: strict_pw="" compat_pw="x"
unknown: default_err=false strict_err=true
nil-v2: {"name":"a","password":"","roles":[],"meta":{}}
nil-compat: {"name":"a","password":"","roles":null,"meta":null}
```

### Tests

The table nails each divergence as an independent subtest so a future toolchain
change that alters any one behavior fails loudly and specifically. Duplicate-key
rejection is asserted with `errors.Is(err, jsontext.ErrDuplicateName)`; the other
rejections assert a non-nil error, and the acceptances assert the bound value.
`TestOmitZero` and `TestFormatTag` check the wire bytes for the tag features, and
`ExampleMarshalToFunc` verifies a caller-registered streaming marshaler emits the
expected token stream for `bool` values.

Create `compat_test.go`:

```go
//go:build goexperiment.jsonv2

package jsoncompat

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDivergence(t *testing.T) {
	t.Parallel()

	t.Run("duplicate rejected by default", func(t *testing.T) {
		t.Parallel()
		_, err := UnmarshalUser([]byte(`{"name":"a","name":"b"}`))
		if !errors.Is(err, jsontext.ErrDuplicateName) {
			t.Fatalf("err = %v, want jsontext.ErrDuplicateName", err)
		}
	})
	t.Run("duplicate allowed in compat", func(t *testing.T) {
		t.Parallel()
		u, err := UnmarshalUser([]byte(`{"name":"a","name":"b"}`), CompatOptions())
		if err != nil {
			t.Fatalf("compat unmarshal: %v", err)
		}
		if u.Name != "b" {
			t.Errorf("Name = %q, want last-wins %q", u.Name, "b")
		}
	})
	t.Run("case-sensitive by default", func(t *testing.T) {
		t.Parallel()
		u, err := UnmarshalUser([]byte(`{"Password":"x"}`))
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if u.Password != "" {
			t.Errorf("Password = %q, want unbound (empty)", u.Password)
		}
	})
	t.Run("case-insensitive in compat", func(t *testing.T) {
		t.Parallel()
		u, err := UnmarshalUser([]byte(`{"Password":"x"}`), CompatOptions())
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if u.Password != "x" {
			t.Errorf("Password = %q, want %q", u.Password, "x")
		}
	})
	t.Run("unknown ignored by default", func(t *testing.T) {
		t.Parallel()
		u, err := UnmarshalUser([]byte(`{"name":"a","extra":1}`))
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if u.Name != "a" {
			t.Errorf("Name = %q, want %q", u.Name, "a")
		}
	})
	t.Run("unknown rejected by strict", func(t *testing.T) {
		t.Parallel()
		_, err := UnmarshalUser([]byte(`{"name":"a","extra":1}`), StrictOptions())
		if err == nil {
			t.Fatal("want error for unknown member, got nil")
		}
	})
	t.Run("invalid utf8 rejected by default", func(t *testing.T) {
		t.Parallel()
		bad := []byte("{\"name\":\"\xff\"}")
		_, err := UnmarshalUser(bad)
		if err == nil {
			t.Fatal("want error for invalid UTF-8, got nil")
		}
	})
	t.Run("invalid utf8 accepted by compat", func(t *testing.T) {
		t.Parallel()
		bad := []byte("{\"name\":\"\xff\"}")
		_, err := UnmarshalUser(bad, CompatOptions())
		if err != nil {
			t.Fatalf("compat unmarshal: %v", err)
		}
	})
}

func TestNilCollectionMarshal(t *testing.T) {
	t.Parallel()
	u := User{Name: "a"}

	v2out, err := MarshalUser(u)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(v2out), `{"name":"a","password":"","roles":[],"meta":{}}`; got != want {
		t.Errorf("v2 marshal = %s, want %s", got, want)
	}

	compatOut, err := MarshalUser(u, CompatOptions())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(compatOut), `{"name":"a","password":"","roles":null,"meta":null}`; got != want {
		t.Errorf("compat marshal = %s, want %s", got, want)
	}
}

func TestOmitZero(t *testing.T) {
	t.Parallel()

	out, err := json.Marshal(Server{Host: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), `{"host":"h"}`; got != want {
		t.Errorf("omitzero marshal = %s, want %s", got, want)
	}

	out2, err := json.Marshal(Server{Host: "h", Port: 8080, TLS: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out2), `{"host":"h","port":8080,"tls":true}`; got != want {
		t.Errorf("marshal = %s, want %s", got, want)
	}
}

func TestFormatTag(t *testing.T) {
	t.Parallel()
	rec := Record{
		When: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		Raw:  []byte("hi"),
	}
	out, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), `{"when":"2026-07-01T12:00:00Z","raw":"aGk="}`; got != want {
		t.Errorf("format marshal = %s, want %s", got, want)
	}
}

func ExampleMarshalToFunc() {
	// Register a streaming marshaler for bool without changing any type.
	yesNo := json.MarshalToFunc(func(enc *jsontext.Encoder, b bool) error {
		if b {
			return enc.WriteToken(jsontext.String("yes"))
		}
		return enc.WriteToken(jsontext.String("no"))
	})
	out, err := json.Marshal(
		map[string]bool{"active": true, "admin": false},
		json.WithMarshalers(yesNo),
		json.Deterministic(true),
	)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
	// Output: {"active":"yes","admin":"no"}
}
```

## Review

The harness is correct when each subtest independently pins one divergence: v2
rejects duplicates and invalid UTF-8, matches names case-sensitively, ignores
unknown members by default while `StrictOptions` rejects them, and marshals nil
collections as `[]`/`{}` while `CompatOptions` restores `null`. Keeping each as its
own subtest means a future behavior change surfaces as a named failure, which is
exactly the drift-detection technique for migrating a real service: run its suite
under the experiment and watch which rows break.

The mistakes to avoid are conceptual. Do not treat `CompatOptions` as the safe
default — it re-opens the holes v2 closed and belongs only at the one external
boundary that needs it; the internal path should be strict. Do not confuse
`omitzero` with `omitempty`: `TestOmitZero` shows a zero `Port`/`TLS` dropped by
`omitzero`, which is the precise "zero means omit" rule, not the fuzzy emptiness
test. Do not reach for a hand-written `MarshalJSON` when a `format` tag expresses
the wire representation declaratively, as `Record` does. And remember that
`MarshalToFunc` registered via `WithMarshalers` attaches streaming behavior to a
type you do not own without editing it. Run `go test -race` to confirm all of it.

## Resources

- [`encoding/json/v2`](https://pkg.go.dev/encoding/json/v2) — options, `JoinOptions`, `MarshalToFunc`, `WithMarshalers`, and struct-tag semantics.
- [`encoding/json/jsontext`](https://pkg.go.dev/encoding/json/jsontext) — `AllowDuplicateNames`, `AllowInvalidUTF8`, and `ErrDuplicateName`.
- [JSON evolution in Go: from v1 to v2](https://antonz.org/go-json-v2/) — a field guide to the changed defaults and the new tags.
- [Go 1.25 release notes](https://go.dev/doc/go1.25) — the `encoding/json/v2` experiment and how to enable it.

---

Prev: [02-jsontext-token-rewriting.md](02-jsontext-token-rewriting.md) | Back to [00-concepts.md](00-concepts.md) | Next: [../13-flight-recorder-runtime-trace/00-concepts.md](../13-flight-recorder-runtime-trace/00-concepts.md)
