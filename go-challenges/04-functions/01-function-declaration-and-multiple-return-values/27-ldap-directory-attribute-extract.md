# Exercise 27: LDAP Directory Attribute Lookup

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye casos borde).

An LDAP attribute lookup can fail in ways that look identical from a bare
`(string, error)` return but demand completely different handling: the
directory connection dropped (retry the bind), the entry itself does not
exist (the user was deleted), or the entry exists but simply never set that
attribute (`mobile` is optional; most entries do not have it). This exercise
builds `Directory.GetAttribute(dn, attr string) (values []string, found
bool, error)` so a caller can route each case correctly instead of treating
"no values" as one undifferentiated failure.

This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
ldapattr/                  independent module: example.com/ldap-directory-attribute-extract
  go.mod                   go 1.24
  ldapattr.go              package ldapattr; ErrEntryNotFound; Directory; GetAttribute(dn,attr) (values,found,error)
  cmd/
    demo/
      main.go              found multi-value, absent attribute, missing entry, forced connection failure
  ldapattr_test.go          found; absent-but-entry-exists; ErrEntryNotFound; connection failure is not ErrEntryNotFound; result is a defensive copy
```

- Files: `ldapattr.go`, `cmd/demo/main.go`, `ldapattr_test.go`.
- Implement: an in-memory `Directory` with a package-level `ErrEntryNotFound` sentinel; `GetAttribute` returns `(values, true, nil)` when the attribute is set, `(nil, false, nil)` when the entry exists but the attribute is absent, `(nil, false, wrapped ErrEntryNotFound)` when the DN itself does not exist, and `(nil, false, wrapped transport error)` when the connection is forced to fail.
- Test: a multi-value attribute round-trips; an attribute absent on an existing entry returns `found == false, err == nil`; a missing entry gives `errors.Is(err, ErrEntryNotFound) == true`; a forced connection failure gives `errors.Is(err, ErrEntryNotFound) == false` while still non-nil; the returned slice is a defensive copy so a caller mutating it cannot corrupt the directory's internal state.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ldapattr/cmd/demo
cd ~/go-exercises/ldapattr
go mod init example.com/ldap-directory-attribute-extract
go mod edit -go=1.24
```

### Three ways to get "nothing back", three different responses

A signature like `GetAttribute(dn, attr string) ([]string, error)` forces
every one of these into the same `error` bucket, and the caller cannot
recover which happened without parsing error strings:

- **connection failure**: the bind dropped, the socket reset, the LDAP
  server is unreachable. This is transient and retryable — the correct
  response is backoff-and-retry, not "this user has no phone number".
- **entry not found**: the DN itself is absent from the directory — the
  account was deleted or the DN was typo'd. This is a hard failure for
  anything that assumed the user exists, and the caller needs a sentinel
  it can match with `errors.Is` to decide whether to surface a 404-style
  error up the stack.
- **attribute absent**: the entry exists, the connection is fine, and the
  attribute genuinely has no value. This is not an error at all — it is
  the normal shape of an optional field, and code that treats it as one
  will alert on-call for every user who never set a mobile number.

Three outcomes, one `bool` and one `error` return: `found` tells you
whether there is a value; when `found` is `false`, `err` tells you whether
that absence is normal (`nil`) or a real problem (`errors.Is(err,
ErrEntryNotFound)` for a missing entry, anything else for a transport
failure). Collapsing any two of these loses information a caller needs.

Create `ldapattr.go`:

```go
package ldapattr

import (
	"errors"
	"fmt"
)

// ErrEntryNotFound is the sentinel for "no such distinguished name" -- the
// directory has no entry at all under that DN, as opposed to an entry that
// exists but simply lacks the requested attribute.
var ErrEntryNotFound = errors.New("ldap: entry not found")

// Directory is an in-memory stand-in for an LDAP connection. failNext, when
// set, simulates a connection/transport failure on the next lookup.
type Directory struct {
	entries  map[string]map[string][]string
	failNext error
}

func NewDirectory() *Directory {
	return &Directory{entries: make(map[string]map[string][]string)}
}

// AddEntry seeds a directory entry (stands in for a real LDAP add).
func (d *Directory) AddEntry(dn string, attrs map[string][]string) {
	d.entries[dn] = attrs
}

// FailNextWith forces the next GetAttribute call to return a wrapped copy
// of err, simulating a dropped connection or a bind failure.
func (d *Directory) FailNextWith(err error) {
	d.failNext = err
}

// GetAttribute looks up a single attribute on the entry identified by dn.
// It distinguishes three outcomes:
//   - connection down:      (nil, false, wrapped transport error)
//   - entry does not exist: (nil, false, wrapped ErrEntryNotFound)
//   - attribute absent:     (nil, false, nil) -- the entry exists but has no
//     value for attr, an entirely normal LDAP state (e.g. an optional
//     attribute like "mobile" that was never set)
//   - attribute present:    (values, true, nil)
func (d *Directory) GetAttribute(dn, attr string) (values []string, found bool, err error) {
	if d.failNext != nil {
		failure := d.failNext
		d.failNext = nil
		return nil, false, fmt.Errorf("ldap: query %q: %w", dn, failure)
	}

	entry, ok := d.entries[dn]
	if !ok {
		return nil, false, fmt.Errorf("ldap: %w: %s", ErrEntryNotFound, dn)
	}

	raw, ok := entry[attr]
	if !ok || len(raw) == 0 {
		return nil, false, nil
	}

	// Return a copy so the caller mutating the result can never corrupt
	// the directory's internal state.
	out := make([]string, len(raw))
	copy(out, raw)
	return out, true, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	ldapattr "example.com/ldap-directory-attribute-extract"
)

func main() {
	dir := ldapattr.NewDirectory()
	dir.AddEntry("uid=alice,ou=people,dc=example,dc=com", map[string][]string{
		"mail":     {"alice@example.com"},
		"memberOf": {"cn=eng,ou=groups,dc=example,dc=com", "cn=oncall,ou=groups,dc=example,dc=com"},
	})

	values, found, err := dir.GetAttribute("uid=alice,ou=people,dc=example,dc=com", "mail")
	fmt.Printf("mail:        values=%v found=%t err=%v\n", values, found, err)

	values, found, err = dir.GetAttribute("uid=alice,ou=people,dc=example,dc=com", "mobile")
	fmt.Printf("mobile:      values=%v found=%t err=%v\n", values, found, err)

	values, found, err = dir.GetAttribute("uid=bob,ou=people,dc=example,dc=com", "mail")
	fmt.Printf("missing dn:  values=%v found=%t errIsNotFound=%t\n", values, found, errors.Is(err, ldapattr.ErrEntryNotFound))

	dir.FailNextWith(errors.New("connection reset by peer"))
	values, found, err = dir.GetAttribute("uid=alice,ou=people,dc=example,dc=com", "mail")
	fmt.Printf("conn down:   values=%v found=%t err=%v\n", values, found, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
mail:        values=[alice@example.com] found=true err=<nil>
mobile:      values=[] found=false err=<nil>
missing dn:  values=[] found=false errIsNotFound=true
conn down:   values=[] found=false err=ldap: query "uid=alice,ou=people,dc=example,dc=com": connection reset by peer
```

### Tests

Create `ldapattr_test.go`:

```go
package ldapattr

import (
	"errors"
	"testing"
)

const aliceDN = "uid=alice,ou=people,dc=example,dc=com"

func newTestDirectory() *Directory {
	dir := NewDirectory()
	dir.AddEntry(aliceDN, map[string][]string{
		"mail":     {"alice@example.com"},
		"memberOf": {"cn=eng,ou=groups,dc=example,dc=com", "cn=oncall,ou=groups,dc=example,dc=com"},
	})
	return dir
}

func TestGetAttributeFoundMultiValue(t *testing.T) {
	t.Parallel()
	dir := newTestDirectory()

	values, found, err := dir.GetAttribute(aliceDN, "memberOf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if len(values) != 2 {
		t.Fatalf("len(values) = %d, want 2", len(values))
	}
}

func TestGetAttributeAbsentOnExistingEntry(t *testing.T) {
	t.Parallel()
	dir := newTestDirectory()

	values, found, err := dir.GetAttribute(aliceDN, "mobile")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("found = true, want false for an absent attribute")
	}
	if values != nil {
		t.Fatalf("values = %v, want nil", values)
	}
}

func TestGetAttributeEntryNotFound(t *testing.T) {
	t.Parallel()
	dir := newTestDirectory()

	_, found, err := dir.GetAttribute("uid=bob,ou=people,dc=example,dc=com", "mail")
	if found {
		t.Fatal("found = true, want false")
	}
	if !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("err = %v, want ErrEntryNotFound", err)
	}
}

func TestGetAttributeConnectionFailureIsNotEntryNotFound(t *testing.T) {
	t.Parallel()
	dir := newTestDirectory()
	dir.FailNextWith(errors.New("connection reset by peer"))

	_, found, err := dir.GetAttribute(aliceDN, "mail")
	if err == nil {
		t.Fatal("want an error when the connection is down")
	}
	if found {
		t.Fatal("found = true despite a connection failure")
	}
	if errors.Is(err, ErrEntryNotFound) {
		t.Fatal("a connection failure must NOT be reported as ErrEntryNotFound")
	}
}

func TestGetAttributeResultIsACopy(t *testing.T) {
	t.Parallel()
	dir := newTestDirectory()

	values, _, err := dir.GetAttribute(aliceDN, "memberOf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	values[0] = "tampered"

	again, _, err := dir.GetAttribute(aliceDN, "memberOf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if again[0] == "tampered" {
		t.Fatal("mutating the returned slice corrupted the directory's internal state")
	}
}
```

## Review

`GetAttribute` is correct when its four outcomes stay distinct: a
connection failure never masquerades as `ErrEntryNotFound`, a missing entry
never masquerades as "the attribute just isn't set", and a genuinely absent
attribute never gets escalated to an `error` at all.
`TestGetAttributeConnectionFailureIsNotEntryNotFound` is the load-bearing
test — it proves a transient transport error cannot be mistaken for "delete
this user's session", which is exactly the wrong reaction to a dropped
connection. `TestGetAttributeResultIsACopy` guards a second, quieter bug:
returning the internal slice by reference would let one caller's mutation
silently corrupt every future lookup against the same entry.

The mistake to avoid is folding "entry not found" and "attribute absent"
into the same `(nil, false, nil)` shape for convenience — a caller that
needs to tell "this user does not exist" from "this user has no phone
number" cannot do so once both look identical, and that distinction is
exactly what drives whether the caller retries, 404s, or just renders an
empty field.

## Resources

- [RFC 4511: LDAP — The Protocol](https://www.rfc-editor.org/rfc/rfc4511) — the search-result and attribute model this exercise mirrors.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a wrapped chain against the `ErrEntryNotFound` sentinel.
- [golang.org/x/crypto/... ldap client patterns](https://pkg.go.dev/gopkg.in/ldap.v3) — a real Go LDAP client showing multi-value attribute and search-entry shapes.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-json-number-safe-coerce.md](26-json-number-safe-coerce.md) | Next: [28-postgres-pool-acquire-metrics.md](28-postgres-pool-acquire-metrics.md)
