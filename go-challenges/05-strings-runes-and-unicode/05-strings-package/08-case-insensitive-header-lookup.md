# Exercise 8: Case-Insensitive Header/Method Allowlist Without Unicode Traps

HTTP methods and header field names are case-insensitive, so an allowlist check
(`is this method permitted?`, `is this header in the CORS allow-list?`) must fold
case. The right tool is `strings.EqualFold`, not `ToLower`-as-map-key. But
`EqualFold` is *simple* folding, and this exercise pins exactly where it is and
is not safe: fine for ASCII protocol tokens, wrong as an identity oracle.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
allowlist/                      independent module: example.com/allowlist
  go.mod                        go 1.26
  allowlist.go                  Allowed (EqualFold) + BuildLookup (ASCII-only ToLower map)
  allowlist_test.go             allow/reject table + EqualFold Unicode-limit pins
  cmd/
    demo/
      main.go                   runnable demo
```

Files: `allowlist.go`, `allowlist_test.go`, `cmd/demo/main.go`.
Implement: `Allowed(name string, set []string) bool` and a `BuildLookup` that
documents its ASCII-only precondition.
Test: `GET`/`get`/`Get` allowed, `DELETE` rejected, ASCII fold; plus pins that
`EqualFold` folds the Kelvin sign and does not treat `ß` as `ss`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/05-strings-package/08-case-insensitive-header-lookup/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/05-strings-package/08-case-insensitive-header-lookup
```

### EqualFold for protocol tokens, and why not for identity

`strings.EqualFold(a, b)` walks both strings rune by rune and compares under
Unicode simple case folding, in one pass and without allocating. For an allowlist
of ASCII protocol tokens — `GET`, `POST`, `Content-Type` — it is exactly right: a
client sending `get` or `content-type` is conforming, and folding accepts them.
The security consequence of getting this wrong runs both ways: a case-sensitive
`==` check rejects a valid `get` (a correctness/interop bug), while a check that
folds *too much* could accept something it should not.

That second risk is why the module pins `EqualFold`'s limits with tests instead
of prose alone. `EqualFold` is *simple* folding, and it has surprising edges:

- It folds compatibility characters. The Kelvin sign `K` (U+212A) folds to ASCII
  `k`, so `EqualFold("K", "K")` is `true`. Harmless for method names, but a
  reminder that fold classes are wider than ASCII case.
- It is not locale-aware. It treats ASCII `I` and `i` as equal unconditionally,
  which is wrong under Turkish rules where the dotted and dotless i are distinct.
  `strings.ToLowerSpecial(unicode.TurkishCase, s)` exists for locale casing;
  `EqualFold` cannot express it.
- It does not do full folding. The German eszett does not fold to `ss`:
  `EqualFold("straße", "strasse")` is `false`.

The takeaway: `EqualFold` decides "is this the same ASCII protocol keyword",
never "are these two human identifiers the same person". For usernames and emails
across scripts, use `golang.org/x/text` or a PRECIS profile.

`BuildLookup` shows the `ToLower`-as-map-key pattern for when you want O(1)
membership instead of a linear `EqualFold` scan — but it carries an explicit
precondition: it is only correct for ASCII keys, because `ToLower` is not the same
canonicalization as folding for non-ASCII input.

Create `allowlist.go`:

```go
package allowlist

import "strings"

// Allowed reports whether name matches any entry in set, comparing
// case-insensitively per the HTTP rule for methods and header field names.
func Allowed(name string, set []string) bool {
	for _, s := range set {
		if strings.EqualFold(name, s) {
			return true
		}
	}
	return false
}

// BuildLookup returns a set keyed by lower-cased name for O(1) membership. It is
// correct ONLY for ASCII protocol tokens; ToLower is not identity folding for
// arbitrary Unicode, so do not use this for usernames or emails.
func BuildLookup(names []string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[strings.ToLower(n)] = struct{}{}
	}
	return m
}

// InLookup checks membership in a map built by BuildLookup, folding the query to
// lower case first. ASCII-only, same precondition as BuildLookup.
func InLookup(name string, m map[string]struct{}) bool {
	_, ok := m[strings.ToLower(name)]
	return ok
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/allowlist"
)

func main() {
	methods := []string{"GET", "POST", "PUT"}
	for _, m := range []string{"get", "POST", "delete"} {
		fmt.Printf("%-8s allowed=%v\n", m, allowlist.Allowed(m, methods))
	}

	lookup := allowlist.BuildLookup([]string{"Content-Type", "Authorization"})
	for _, h := range []string{"content-type", "X-Custom"} {
		fmt.Printf("%-14s allowed=%v\n", h, allowlist.InLookup(h, lookup))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
get      allowed=true
POST     allowed=true
delete   allowed=false
content-type   allowed=true
X-Custom       allowed=false
```

### Tests

Create `allowlist_test.go`:

```go
package allowlist

import (
	"fmt"
	"strings"
	"testing"
)

func TestAllowed(t *testing.T) {
	t.Parallel()

	methods := []string{"GET", "POST", "PUT"}
	tests := []struct {
		name string
		want bool
	}{
		{name: "GET", want: true},
		{name: "get", want: true},
		{name: "Get", want: true},
		{name: "POST", want: true},
		{name: "DELETE", want: false},
		{name: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Allowed(tc.name, methods); got != tc.want {
				t.Fatalf("Allowed(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestBuildLookupASCII(t *testing.T) {
	t.Parallel()

	m := BuildLookup([]string{"Content-Type", "Authorization"})
	if !InLookup("content-type", m) {
		t.Fatal("content-type should be in the lookup")
	}
	if InLookup("X-Custom", m) {
		t.Fatal("X-Custom should not be in the lookup")
	}
}

// TestEqualFoldUnicodeLimits pins the documented edges of simple folding so the
// prose cannot drift from the behavior: EqualFold folds the Kelvin sign, and it
// does NOT treat the German eszett as "ss".
func TestEqualFoldUnicodeLimits(t *testing.T) {
	t.Parallel()

	if !strings.EqualFold("K", "K") {
		t.Fatal("EqualFold should fold the Kelvin sign U+212A to ASCII k")
	}
	if strings.EqualFold("straße", "strasse") {
		t.Fatal("EqualFold must NOT treat eszett as ss (simple folding, not full)")
	}
	// ASCII I/i fold unconditionally, ignoring locale (Turkish) rules.
	if !strings.EqualFold("I", "i") {
		t.Fatal("EqualFold folds ASCII I and i regardless of locale")
	}
}

func ExampleAllowed() {
	fmt.Println(Allowed("get", []string{"GET", "POST"}))
	// Output: true
}
```

## Review

The allowlist is correct when `GET`, `get`, and `Get` all pass and folding is
confined to the case decision. The higher-value lesson is the boundary:
`TestEqualFoldUnicodeLimits` pins that `EqualFold` folds the Kelvin sign, does not
fold eszett to `ss`, and folds ASCII I/i regardless of Turkish locale — which is
why it is safe for protocol tokens and unsafe as an identity oracle. The trap to
avoid is reaching for `ToLower`-as-map-key on non-ASCII identifiers and treating
that as canonicalization. Confirm with `go test -race`.

## Resources

- [strings.EqualFold](https://pkg.go.dev/strings#EqualFold) — one-pass simple Unicode folding.
- [strings.ToLowerSpecial](https://pkg.go.dev/strings#ToLowerSpecial) — locale-aware casing (e.g. Turkish).
- [RFC 9110 field names](https://www.rfc-editor.org/rfc/rfc9110#name-field-names) — header names are case-insensitive.
- [golang.org/x/text/cases](https://pkg.go.dev/golang.org/x/text/cases) — correct casing for identity work.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-secret-log-redactor.md](07-secret-log-redactor.md) | Next: [09-sql-in-placeholder-builder.md](09-sql-in-placeholder-builder.md)
