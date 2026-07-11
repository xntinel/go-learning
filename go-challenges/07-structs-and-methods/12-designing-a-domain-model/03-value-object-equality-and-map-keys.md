# Exercise 3: Comparable Value Objects as Map Keys and Dedup Sets

A value object earns its keep when it becomes a map key: a normalizing
constructor makes equality *meaningful*, and comparability makes the key work at
all. This module builds an `EmailAddress` that lowercases and trims on
construction, then uses it as the key of a de-duplicating index so two spellings
of the same address collapse to one entry.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
email/                      independent module: example.com/email
  go.mod                    go 1.26
  email.go                  type EmailAddress (normalized); type Set keyed by it
  cmd/
    demo/
      main.go               runnable demo: dedup two spellings of one address
  email_test.go             tests: normalization, map-key collision, dedup, equality contract
```

- Files: `email.go`, `cmd/demo/main.go`, `email_test.go`.
- Implement: `EmailAddress` with a single unexported normalized `value`, `NewEmailAddress` that trims and lowercases, `String`, and a `Set` (`map[EmailAddress]struct{}`) with `Add`/`Has`/`Len`.
- Test: two addresses differing only in case/whitespace normalize equal and collide as one map key; a raw unnormalized string does not; the `Set` dedups; equal values compare `==`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/email/cmd/demo
cd ~/go-exercises/email
go mod init example.com/email
```

### Why a value object must be comparable, and why normalization makes it mean something

A value object should be usable as a map key. In Go a struct is a valid map key
exactly when it is *comparable*, which holds only if every field is comparable —
so no slice, map, or function fields. `EmailAddress` has a single `string` field,
so it is comparable, and `==` compares that string. If you were to add a
`[]string` field (say, a list of aliases), the type would stop compiling as a map
key and comparing two of them with `==` would fail to build; that is the
compile-time enforcement of the "value objects stay comparable" rule. Keep the
field set comparable and the type stays usable everywhere a key is needed.

Comparability alone is not enough; equality has to *mean* the right thing.
`"Alice@Example.com"` and `" alice@example.com "` are the same address to a human,
but as raw strings they are three different values and would occupy three
different map slots. The fix is a *normalizing constructor*: `NewEmailAddress`
trims surrounding whitespace and lowercases, so every spelling of the same address
produces the identical stored `value`. After that, `==` on `EmailAddress` is the
domain's notion of equality, and using the type as a map key deduplicates
correctly. This is why the field is unexported and there is no way to build an
`EmailAddress` except through the constructor — an un-normalized instance must not
exist.

When is `==` not enough, and you reach for an explicit `Equal` method? When
equality must ignore a field, or when a field is itself non-comparable. Neither
applies here — a single normalized string is precisely comparable — so `==` is the
right tool and adding an `Equal` method would be noise. The `reflect.DeepEqual`
you will see in the test is there only to *contrast*: it compares structurally and
would treat two un-normalized values as different, which is exactly the trap
normalization avoids.

Create `email.go`:

```go
package email

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrEmpty   = errors.New("email: address is required")
	ErrNoAtSep = errors.New("email: address must contain '@'")
)

// EmailAddress is a value object with a single comparable field. The constructor
// normalizes (trim + lowercase) so equal addresses have the identical value and
// compare == and collide as one map key.
type EmailAddress struct {
	value string
}

// NewEmailAddress normalizes and validates. It is the only door into the type,
// so an EmailAddress that exists is always normalized.
func NewEmailAddress(raw string) (EmailAddress, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return EmailAddress{}, ErrEmpty
	}
	if !strings.Contains(v, "@") {
		return EmailAddress{}, fmt.Errorf("%w: %q", ErrNoAtSep, raw)
	}
	return EmailAddress{value: v}, nil
}

// String returns the normalized address.
func (e EmailAddress) String() string { return e.value }

// Set is a de-duplicating collection keyed by the value object itself. Because
// EmailAddress is comparable, it is a valid map key.
type Set struct {
	m map[EmailAddress]struct{}
}

func NewSet() *Set { return &Set{m: make(map[EmailAddress]struct{})} }

// Add inserts addr; a second Add of an equal address is a no-op.
func (s *Set) Add(addr EmailAddress) { s.m[addr] = struct{}{} }

// Has reports whether an equal address is present.
func (s *Set) Has(addr EmailAddress) bool {
	_, ok := s.m[addr]
	return ok
}

// Len reports the number of distinct addresses.
func (s *Set) Len() int { return len(s.m) }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/email"
)

func main() {
	set := email.NewSet()
	for _, raw := range []string{"Alice@Example.com", " alice@example.com ", "bob@example.com"} {
		addr, err := email.NewEmailAddress(raw)
		if err != nil {
			fmt.Printf("rejected %q: %v\n", raw, err)
			continue
		}
		set.Add(addr)
	}
	fmt.Printf("distinct addresses: %d\n", set.Len())

	alice, _ := email.NewEmailAddress("ALICE@EXAMPLE.COM")
	fmt.Printf("has alice (any case): %v\n", set.Has(alice))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
distinct addresses: 2
has alice (any case): true
```

### Tests

The tests prove the two halves of the contract. Normalization: two raw spellings
that differ only in case and whitespace produce equal `EmailAddress` values that
compare `==` and land on the same map slot, while a genuinely different address
does not. Dedup: adding both spellings to a `Set` leaves one entry. The
`reflect.DeepEqual` case contrasts structural equality on raw strings with the
normalized value-object equality.

Create `email_test.go`:

```go
package email

import (
	"errors"
	"reflect"
	"testing"
)

func TestNormalization(t *testing.T) {
	t.Parallel()
	a, err := NewEmailAddress("Alice@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewEmailAddress("  alice@example.com  ")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("normalized addresses compare unequal: %q vs %q", a, b)
	}
	if a.String() != "alice@example.com" {
		t.Fatalf("String = %q, want alice@example.com", a.String())
	}
}

func TestMapKeyCollision(t *testing.T) {
	t.Parallel()
	a, _ := NewEmailAddress("Alice@Example.com")
	b, _ := NewEmailAddress("alice@example.com")
	c, _ := NewEmailAddress("carol@example.com")

	seen := map[EmailAddress]int{}
	seen[a]++
	seen[b]++
	seen[c]++

	if seen[a] != 2 {
		t.Fatalf("equal addresses did not collide: count = %d, want 2", seen[a])
	}
	if len(seen) != 2 {
		t.Fatalf("distinct keys = %d, want 2", len(seen))
	}
}

func TestSetDedups(t *testing.T) {
	t.Parallel()
	s := NewSet()
	a, _ := NewEmailAddress("Alice@Example.com")
	b, _ := NewEmailAddress(" alice@example.com ")
	s.Add(a)
	s.Add(b)
	if s.Len() != 1 {
		t.Fatalf("Set.Len = %d, want 1", s.Len())
	}
	if !s.Has(a) || !s.Has(b) {
		t.Fatal("Set.Has failed for an equal address")
	}
}

func TestRejects(t *testing.T) {
	t.Parallel()
	if _, err := NewEmailAddress("   "); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
	if _, err := NewEmailAddress("no-at-sign"); !errors.Is(err, ErrNoAtSep) {
		t.Fatalf("err = %v, want ErrNoAtSep", err)
	}
}

// TestDeepEqualContrast shows why normalization matters: structural equality on
// raw strings would treat two spellings as different; the value object does not.
func TestDeepEqualContrast(t *testing.T) {
	t.Parallel()
	rawA := struct{ v string }{v: "Alice@Example.com"}
	rawB := struct{ v string }{v: "alice@example.com"}
	if reflect.DeepEqual(rawA, rawB) {
		t.Fatal("raw structs unexpectedly equal")
	}
	a, _ := NewEmailAddress("Alice@Example.com")
	b, _ := NewEmailAddress("alice@example.com")
	if a != b {
		t.Fatal("normalized value objects should be equal")
	}
}
```

## Review

The `EmailAddress` is correct when construction is the only way to obtain one and
construction always normalizes, so `==` is the domain's equality and the type
deduplicates as a map key. The failure this design prevents is a "set" of emails
that silently holds `Alice@` and `alice@` as two members because equality was
computed on un-normalized input. The mistakes to avoid: giving the value object a
slice or map field, which breaks `==` and map-key use at compile time; and
skipping normalization, which makes equality meaningless. Reach for an explicit
`Equal` method only when a field is non-comparable or must be ignored — not here,
where a single normalized string is exactly right.

## Resources

- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — when structs are comparable and usable as map keys.
- [`strings` package](https://pkg.go.dev/strings) — `strings.ToLower`, `strings.TrimSpace`, `strings.Contains`.
- [`reflect.DeepEqual`](https://pkg.go.dev/reflect#DeepEqual) — structural equality, contrasted with value-object `==`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-user-entity-invariants.md](02-user-entity-invariants.md) | Next: [04-money-currency-safe-arithmetic.md](04-money-currency-safe-arithmetic.md)
