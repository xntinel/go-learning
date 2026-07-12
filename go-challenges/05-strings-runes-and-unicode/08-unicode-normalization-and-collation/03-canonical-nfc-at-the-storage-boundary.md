# Exercise 3: Normalize-on-Write — NFC Canonicalization at the Repository Boundary

The fix for the NFC/NFD asymmetry the probe exposed is not a cleverer comparison —
it is a single canonicalization at the write boundary. This module builds a
repository that folds every inbound string field to NFC before it is stored, so the
database's byte-level unique constraints, primary keys, and equality joins behave
the way users expect: `café` typed as one code point and `café` typed as two
collapse to one on-disk representation.

This module is fully self-contained: its own `go mod init`, an in-memory repository
standing in for the database, its own demo and tests. It uses
`golang.org/x/text/unicode/norm`.

## What you'll build

```text
nfcrepo/                     independent module: example.com/nfcrepo
  go.mod                     requires golang.org/x/text
  repo.go                    Canonicalize, UserRepo (map keyed by NFC name)
  cmd/demo/main.go           insert NFD, look up NFC, see it hit
  repo_test.go               NFC(NFD)==NFC(NFC), cross-form lookup, invalid UTF-8, idempotency
```

Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
Implement: `Canonicalize(string) (string, error)` = reject invalid UTF-8 then `norm.NFC.String`; a `UserRepo` whose `Create`/`FindByName` canonicalize at the boundary and key an in-memory map by the NFC form.
Test: `norm.NFC(NFD) == norm.NFC(NFC)`; a row inserted as NFD is found when queried as NFC and vice versa; invalid UTF-8 is rejected via `errors.Is`; `norm.NFC.IsNormalString(Canonicalize(x))` is true.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/08-unicode-normalization-and-collation/03-canonical-nfc-at-the-storage-boundary/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/08-unicode-normalization-and-collation/03-canonical-nfc-at-the-storage-boundary
go get golang.org/x/text/unicode/norm
```

### Why the boundary, and why NFC specifically

A database unique index compares bytes. If one account is created with a display
name whose `é` is precomposed (U+00E9) and another with the decomposed `e` + U+0301,
the index sees two different byte strings and lets both in — you now have two
accounts that render identically and a support ticket you cannot reproduce. The
same asymmetry turns a cache key into a phantom miss and a join into a silent
no-match. The cure is an *invariant*: every string that enters the persistence
layer is in one canonical form. Impose it once, at the boundary, and the rest of
the stack — the unique index, the `WHERE name = $1`, the `map[string]` cache — can
keep using plain byte equality and be correct.

The canonical form to store is **NFC**. It is what browsers emit, it is the shorter
encoding for precomposed characters, and it is a *lossless* canonical form, unlike
the compatibility forms NFKC/NFKD which throw away distinctions (ligatures,
width, superscripts) the user may have meant to keep. `Canonicalize` does exactly
two things: reject invalid UTF-8 (so a malformed byte sequence never becomes a key)
and apply `norm.NFC.String`. Rejecting invalid UTF-8 first matters because
`norm.NFC.String` will pass invalid bytes through using the replacement rune
silently; a storage boundary should refuse them loudly instead.

`Create` and `FindByName` both run `Canonicalize` on the name, so an insert in one
form and a lookup in the other land on the same map key. That is the whole trick:
the store never sees a non-NFC key.

Create `repo.go`:

```go
package repo

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// ErrInvalidUTF8 is returned when a field is not valid UTF-8. A storage boundary
// refuses malformed bytes rather than silently substituting replacement runes.
var ErrInvalidUTF8 = errors.New("invalid utf-8")

// ErrDuplicate is returned when a name already exists after canonicalization.
var ErrDuplicate = errors.New("duplicate name")

// Canonicalize validates that s is UTF-8 and returns its NFC form. NFC is the
// lossless canonical composition, the correct on-disk representation.
func Canonicalize(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("canonicalize %q: %w", s, ErrInvalidUTF8)
	}
	return norm.NFC.String(s), nil
}

// User is a stored record. Name is canonicalized to NFC before storage.
type User struct {
	Name string
	City string
}

// UserRepo is an in-memory stand-in for a table with a UNIQUE(name) index. Its
// key is the NFC form of the name, imposed at the boundary.
type UserRepo struct {
	byName map[string]User
}

func NewUserRepo() *UserRepo {
	return &UserRepo{byName: make(map[string]User)}
}

// Create canonicalizes the inbound fields and stores the user, rejecting a name
// that already exists after canonicalization (the unique-constraint analogue).
func (r *UserRepo) Create(u User) error {
	key, err := Canonicalize(u.Name)
	if err != nil {
		return err
	}
	city, err := Canonicalize(u.City)
	if err != nil {
		return err
	}
	if _, ok := r.byName[key]; ok {
		return fmt.Errorf("create %q: %w", u.Name, ErrDuplicate)
	}
	r.byName[key] = User{Name: key, City: city}
	return nil
}

// FindByName canonicalizes the query and looks it up by the NFC key, so a lookup
// in any canonical-equivalent form finds the stored row.
func (r *UserRepo) FindByName(name string) (User, bool, error) {
	key, err := Canonicalize(name)
	if err != nil {
		return User{}, false, err
	}
	u, ok := r.byName[key]
	return u, ok, nil
}

// Len reports the number of stored rows.
func (r *UserRepo) Len() int { return len(r.byName) }
```

### The runnable demo

The demo inserts a user whose name is in NFD (decomposed `café`), then looks the
same name up in NFC (precomposed) and shows the row is found and that a second
insert of the other form is rejected as a duplicate — proving the two spellings
share one on-disk identity.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/nfcrepo"
)

func main() {
	nfd := "Caf" + string(rune(0x0065)) + string(rune(0x0301)) // Cafe + combining acute
	nfc := "Caf" + string(rune(0x00E9))                        // Café precomposed

	r := repo.NewUserRepo()
	if err := r.Create(repo.User{Name: nfd, City: "Lyon"}); err != nil {
		panic(err)
	}

	_, found, _ := r.FindByName(nfc)
	fmt.Printf("lookup NFC finds NFD row: %v\n", found)

	err := r.Create(repo.User{Name: nfc, City: "Paris"})
	fmt.Printf("second insert rejected as duplicate: %v\n", errors.Is(err, repo.ErrDuplicate))
	fmt.Printf("rows stored: %d\n", r.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
lookup NFC finds NFD row: true
second insert rejected as duplicate: true
rows stored: 1
```

### Tests

The tests pin the invariant from four angles: NFC collapses the two forms to one
string; a cross-form lookup succeeds in both directions; invalid UTF-8 is rejected
with a wrapped sentinel; and the output of `Canonicalize` is genuinely in NFC
(`IsNormalString` is true), so applying it again is a no-op.

Create `repo_test.go`:

```go
package repo

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func nfd(s string) string { return norm.NFD.String(s) }

func TestNFCCollapsesForms(t *testing.T) {
	t.Parallel()

	nfcCafe := "caf" + string(rune(0x00E9))  // precomposed
	nfdCafe := "cafe" + string(rune(0x0301)) // decomposed
	if norm.NFC.String(nfcCafe) != norm.NFC.String(nfdCafe) {
		t.Fatal("NFC(NFC) and NFC(NFD) must be equal")
	}
}

func TestCrossFormLookup(t *testing.T) {
	t.Parallel()

	names := []string{
		"caf" + string(rune(0x00E9)),        // café
		"nai" + string(rune(0x0308)) + "ve", // naïve (NFD dieresis)
	}
	for _, name := range names {
		r := NewUserRepo()
		if err := r.Create(User{Name: nfd(name)}); err != nil {
			t.Fatalf("Create(NFD): %v", err)
		}
		// Query in NFC: must hit the row inserted as NFD.
		if _, ok, err := r.FindByName(norm.NFC.String(name)); err != nil || !ok {
			t.Fatalf("FindByName(NFC) = ok %v err %v; want hit", ok, err)
		}
		// And the reverse: a fresh repo inserted as NFC, queried as NFD.
		r2 := NewUserRepo()
		if err := r2.Create(User{Name: norm.NFC.String(name)}); err != nil {
			t.Fatalf("Create(NFC): %v", err)
		}
		if _, ok, err := r2.FindByName(nfd(name)); err != nil || !ok {
			t.Fatalf("FindByName(NFD) = ok %v err %v; want hit", ok, err)
		}
	}
}

func TestRejectsInvalidUTF8(t *testing.T) {
	t.Parallel()

	bad := "abc" + string([]byte{0xff, 0xfe}) // not valid UTF-8
	if _, err := Canonicalize(bad); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("Canonicalize(invalid) err = %v, want ErrInvalidUTF8", err)
	}
	r := NewUserRepo()
	if err := r.Create(User{Name: bad}); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("Create(invalid) err = %v, want ErrInvalidUTF8", err)
	}
}

func TestCanonicalizeIsIdempotent(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"caf" + string(rune(0x00E9)),
		"cafe" + string(rune(0x0301)),
		"plain ascii",
	}
	for _, in := range inputs {
		got, err := Canonicalize(in)
		if err != nil {
			t.Fatalf("Canonicalize(%q): %v", in, err)
		}
		if !norm.NFC.IsNormalString(got) {
			t.Fatalf("Canonicalize(%q) = %q, not in NFC", in, got)
		}
		if again, _ := Canonicalize(got); again != got {
			t.Fatalf("Canonicalize not idempotent: %q -> %q", got, again)
		}
	}
}

func ExampleCanonicalize() {
	a, _ := Canonicalize("caf" + string(rune(0x00E9)))  // precomposed café
	b, _ := Canonicalize("cafe" + string(rune(0x0301))) // decomposed café
	fmt.Println(a == b)
	// Output: true
}
```

## Review

The repository is correct when the store never holds a non-NFC key: `Canonicalize`
rejects invalid UTF-8 with a wrapped `ErrInvalidUTF8` and otherwise returns
`norm.NFC.String`, and both `Create` and `FindByName` route through it, so a row
written in NFD and queried in NFC lands on the same map entry. The mistakes to
avoid are the ones the tests guard: do not normalize at the query site only (the
write path would still admit two byte-different duplicates), do not reach for NFKC
here (it is lossy and wrong for round-trip storage), and do not skip the UTF-8
check (a malformed sequence would otherwise become a silent replacement-rune key).
Run `go test -race` to confirm the boundary holds under the table of accented pairs.

## Resources

- [`golang.org/x/text/unicode/norm`](https://pkg.go.dev/golang.org/x/text/unicode/norm) — `NFC.String`, `IsNormalString`.
- [`unicode/utf8`](https://pkg.go.dev/unicode/utf8#ValidString) — `ValidString`, the boundary check.
- [The Go Blog: Text normalization in Go](https://go.dev/blog/normalization) — normalize on input, compare with bytes.
- [UAX #15: Unicode Normalization Forms](https://unicode.org/reports/tr15/) — why NFC is the storage form.

---

Back to [02-nfc-nfd-locale-gap-probe.md](02-nfc-nfd-locale-gap-probe.md) | Next: [04-accent-folding-search-key.md](04-accent-folding-search-key.md)
