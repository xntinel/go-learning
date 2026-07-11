# Exercise 6: RFC 8265 Identity Strings — PRECIS Username and Password Enforcement

Case folding a username is one line of a much larger job. A real identity service
must also reject control characters, bidi-override spoofs, and zero-width joiners,
normalize width and case, and do all of it with a versioned, auditable ruleset.
That framework is PRECIS (RFC 8264/8265), and this module builds the validation
layer on top of it: `UsernameCaseMapped` for usernames, `OpaqueString` for
passwords, with `CompareKey` and `Compare` for storage and equality.

This module is fully self-contained: its own `go mod init`, its own demo and tests.
It uses `golang.org/x/text/secure/precis`.

## What you'll build

```text
precisid/                   independent module: example.com/precisid
  go.mod                    requires golang.org/x/text
  identity.go               CanonicalUsername, UsernameKey, ValidatePassword, PasswordsMatch
  cmd/demo/main.go          canonicalize, reject, compare
  identity_test.go          rejects control/emoji username; CompareKey equality; password preserve+reject; NFC/NFD match
```

Files: `identity.go`, `cmd/demo/main.go`, `identity_test.go`.
Implement: `CanonicalUsername` / `UsernameKey` over `precis.UsernameCaseMapped`, `ValidatePassword` / `PasswordsMatch` over `precis.OpaqueString`, each wrapping failures in a package sentinel.
Test: an all-emoji or control-char username errors (`errors.Is`); two case variants share a `CompareKey`; `OpaqueString` preserves a valid password but errors on a control character; `Compare` is true across NFC/NFD password inputs.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/precisid/cmd/demo
cd ~/go-exercises/precisid
go mod init example.com/precisid
go get golang.org/x/text/secure/precis
```

### Two profiles, two jobs

PRECIS separates strings into an `IdentifierClass` (restrictive: letters, digits, a
small safe set) and a `FreeformClass` (permissive, for passwords and display
names), and ships ready-made *profiles* that combine a class with the enforcement
steps — width mapping, case mapping, normalization to NFC, and a disallowed-code-
point check — as one ordered pipeline. Two profiles cover the identity surface:

- **`precis.UsernameCaseMapped`** is the username profile. `Profile.String`
  canonicalizes: it case-maps to lower, maps width (full-width to ASCII),
  normalizes to NFC, and *rejects* anything with control characters, bidi hazards,
  or other disallowed code points, returning an error instead of a mangled string.
  `Profile.CompareKey` returns the same canonical form intended for storage and
  comparison, and `Profile.Compare(a, b)` reports whether two inputs are the same
  username after enforcement.
- **`precis.OpaqueString`** is the password profile. It *preserves* the password
  (spaces and most symbols are meaningful and kept), normalizes to NFC so the same
  typed password matches across keyboards and platforms, and still rejects
  disallowed control characters. It never truncates significant marks, so a
  password does not silently change when it round-trips.

The senior reason to use these rather than hand-rolled rules: PRECIS rejects
bidi-override spoofing and confusable control characters *by construction* and is
versioned, so its behavior is stable and auditable. Rolling your own inevitably
misses a category. Each helper wraps the profile call and, on failure, returns a
package sentinel (`ErrInvalidUsername` / `ErrInvalidPassword`) wrapped with `%w` so
callers can branch with `errors.Is` while still seeing the PRECIS detail.

Create `identity.go`:

```go
package precisid

import (
	"errors"
	"fmt"

	"golang.org/x/text/secure/precis"
)

// ErrInvalidUsername is returned when a username fails PRECIS UsernameCaseMapped
// enforcement (control chars, bidi hazards, disallowed code points).
var ErrInvalidUsername = errors.New("invalid username")

// ErrInvalidPassword is returned when a password fails PRECIS OpaqueString.
var ErrInvalidPassword = errors.New("invalid password")

// CanonicalUsername enforces and canonicalizes a username: case-mapped, width-
// mapped, NFC, with disallowed code points rejected.
func CanonicalUsername(s string) (string, error) {
	out, err := precis.UsernameCaseMapped.String(s)
	if err != nil {
		return "", fmt.Errorf("username %q: %w: %v", s, ErrInvalidUsername, err)
	}
	return out, nil
}

// UsernameKey returns the storable comparison key for a username, or an error if
// the username is not allowed.
func UsernameKey(s string) (string, error) {
	key, err := precis.UsernameCaseMapped.CompareKey(s)
	if err != nil {
		return "", fmt.Errorf("username key %q: %w: %v", s, ErrInvalidUsername, err)
	}
	return key, nil
}

// ValidatePassword enforces the OpaqueString profile and returns the canonical
// (NFC, preserved) password, or an error for disallowed input.
func ValidatePassword(s string) (string, error) {
	out, err := precis.OpaqueString.String(s)
	if err != nil {
		return "", fmt.Errorf("password: %w: %v", ErrInvalidPassword, err)
	}
	return out, nil
}

// PasswordsMatch reports whether two password inputs are equal after OpaqueString
// enforcement (so NFC and NFD spellings of the same password match).
func PasswordsMatch(a, b string) bool {
	return precis.OpaqueString.Compare(a, b)
}
```

### The runnable demo

The demo canonicalizes a mixed-case username, shows a control-character username
rejected, shows two case variants sharing one key, and shows an NFC password
matching its NFD twin.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/precisid"
)

func main() {
	u, _ := precisid.CanonicalUsername("Alice")
	fmt.Printf("canonical username: %q\n", u)

	_, err := precisid.CanonicalUsername("ali" + string(rune(0x0007)) + "ce")
	fmt.Printf("control-char username rejected: %v\n", errors.Is(err, precisid.ErrInvalidUsername))

	k1, _ := precisid.UsernameKey("Alice")
	k2, _ := precisid.UsernameKey("ALICE")
	fmt.Printf("Alice and ALICE share a key: %v\n", k1 == k2)

	nfc := "s" + string(rune(0x00E9)) + "cret"                        // sécret precomposed
	nfd := "s" + string(rune(0x0065)) + string(rune(0x0301)) + "cret" // secret + acute
	fmt.Printf("NFC and NFD password match: %v\n", precisid.PasswordsMatch(nfc, nfd))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
canonical username: "alice"
control-char username rejected: true
Alice and ALICE share a key: true
NFC and NFD password match: true
```

### Tests

The tests assert the four properties an identity layer depends on: disallowed
usernames error with the wrapped sentinel; case variants collapse to one storable
key; a valid password is preserved verbatim while a control character is rejected;
and NFC/NFD spellings of a password compare equal.

Create `identity_test.go`:

```go
package precisid

import (
	"errors"
	"testing"
)

func TestRejectsDisallowedUsernames(t *testing.T) {
	t.Parallel()

	bad := map[string]string{
		"control char": "ali" + string(rune(0x0007)) + "ce",
		"emoji only":   string(rune(0x1F600)),
		"spaces only":  "   ",
	}
	for name, in := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := CanonicalUsername(in); !errors.Is(err, ErrInvalidUsername) {
				t.Fatalf("CanonicalUsername(%q) err = %v, want ErrInvalidUsername", in, err)
			}
		})
	}
}

func TestUsernameKeyCollapsesCase(t *testing.T) {
	t.Parallel()

	k1, err := UsernameKey("Alice")
	if err != nil {
		t.Fatalf("UsernameKey(Alice): %v", err)
	}
	k2, err := UsernameKey("ALICE")
	if err != nil {
		t.Fatalf("UsernameKey(ALICE): %v", err)
	}
	if k1 != k2 {
		t.Fatalf("keys differ: %q vs %q", k1, k2)
	}
	if k1 != "alice" {
		t.Fatalf("UsernameKey(Alice) = %q, want alice", k1)
	}
}

func TestPasswordPreservedAndValidated(t *testing.T) {
	t.Parallel()

	good := "correct horse battery staple"
	out, err := ValidatePassword(good)
	if err != nil {
		t.Fatalf("ValidatePassword(good): %v", err)
	}
	if out != good {
		t.Fatalf("password not preserved: %q -> %q", good, out)
	}

	bad := "pass" + string(rune(0x0007)) + "word" // embedded control char
	if _, err := ValidatePassword(bad); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("ValidatePassword(control) err = %v, want ErrInvalidPassword", err)
	}
}

func TestPasswordsMatchAcrossForms(t *testing.T) {
	t.Parallel()

	nfc := "s" + string(rune(0x00E9)) + "cret"                        // sécret precomposed
	nfd := "s" + string(rune(0x0065)) + string(rune(0x0301)) + "cret" // secret + acute
	if !PasswordsMatch(nfc, nfd) {
		t.Fatal("NFC and NFD spellings of the same password must match")
	}
	if PasswordsMatch("hunter2", "hunter3") {
		t.Fatal("different passwords must not match")
	}
}
```

## Review

The identity layer is correct when every accepted username and password has passed
one versioned pipeline: `CanonicalUsername`/`UsernameKey` run
`precis.UsernameCaseMapped` (case, width, NFC, disallowed-rune rejection) and
`ValidatePassword`/`PasswordsMatch` run `precis.OpaqueString` (preserve, NFC,
reject controls). The tests pin the security-relevant behavior: control-char and
emoji usernames error, case variants share a key, a valid password round-trips
unchanged while a control character is refused, and NFC/NFD passwords compare equal.
The mistake to avoid is substituting hand-written allow/deny rules for PRECIS —
they miss the bidi-override and confusable-control categories that the profile
rejects by construction, which is precisely where account-takeover spoofing lives.

## Resources

- [`golang.org/x/text/secure/precis`](https://pkg.go.dev/golang.org/x/text/secure/precis) — `UsernameCaseMapped`, `OpaqueString`, `Profile.String`/`CompareKey`/`Compare`.
- [RFC 8265: PRECIS for Usernames and Passwords](https://www.rfc-editor.org/rfc/rfc8265) — the username and password profiles.
- [RFC 8264: PRECIS Framework](https://www.rfc-editor.org/rfc/rfc8264) — IdentifierClass vs FreeformClass and the enforcement steps.

---

Back to [05-locale-aware-case-folding-for-identifiers.md](05-locale-aware-case-folding-for-identifiers.md) | Next: [07-locale-aware-collation-for-listings.md](07-locale-aware-collation-for-listings.md)
