# Exercise 9: Empty structs, sets, and the trailing zero-size field trap

`struct{}` occupies zero bytes, which powers the idiomatic allocation-free set
`map[string]struct{}` — the right tool for dedup and idempotency-key sets. But
zero-size types have one sharp edge: a struct whose *last* field is zero-size is
padded so a pointer to that field cannot point past the allocation. This module
builds the set and demonstrates the trailing-field trap.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test.

## What you'll build

```text
zerosize/                  independent module: example.com/zerosize
  go.mod                   go 1.26
  set.go                   StringSet over map[string]struct{}; trailing zero-size types
  cmd/
    demo/
      main.go              dedups idempotency keys; prints zero-size sizes
  set_test.go              Sizeof(struct{})==0; trailing > leading; set ops table
```

- Files: `set.go`, `cmd/demo/main.go`, `set_test.go`.
- Implement: a `StringSet` backed by `map[string]struct{}` with `Add`/`Has`/`Delete`/`Len`, plus a `TrailingZero` and `LeadingZero` struct to demonstrate the padding rule.
- Test: assert `unsafe.Sizeof(struct{}{}) == 0`, that a struct ending in a zero-size field is strictly larger than the same struct with that field moved earlier, and a table test of the set operations.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/09-struct-memory-layout-and-padding/09-empty-and-trailing-zero-size-fields/cmd/demo
cd go-solutions/07-structs-and-methods/09-struct-memory-layout-and-padding/09-empty-and-trailing-zero-size-fields
```

### Zero-size types and why the last field is special

`struct{}` and `[0]byte` have size zero: they carry no data and cost no storage.
This is exactly what a set wants — a set is a map whose keys matter and whose
values do not, so `map[string]struct{}` stores the keys with a zero-byte value,
using no per-entry value storage at all. It is the idiomatic Go set, and it is the
right shape for deduplicating request ids, tracking seen idempotency keys, or
holding a membership set on a hot path. (`map[string]bool` works too, but spends a
byte per entry and invites the ambiguity of a `false` value; `struct{}` says "the
key's presence is the only fact".)

The sharp edge is the trailing zero-size field. Go guarantees that taking the
address of a struct field yields a pointer *inside* the allocation, and the
language forbids a pointer that points one-past-the-end of a distinct object from
being mistaken for a pointer into the next object. If a struct's last field has
size zero, then `&struct.lastField` would compute to the address just past the
struct's data — potentially the start of the next object in memory. To prevent
that, Go pads a struct whose final field is zero-size by one extra alignment unit,
so the zero-size field has a byte of its own to point at. The consequence is
counter-intuitive: adding a "free" zero-size field at the *end* makes the struct
*larger*. `TrailingZero{x int32; _ [0]byte}` is 8 bytes on a 64-bit platform,
while `LeadingZero{_ [0]byte; x int32}` — the same fields, zero-size one moved
first — is 4. Same data, different size, purely because of where the zero-size
field sits.

Create `set.go`:

```go
// Package zerosize demonstrates the zero-byte struct{} set and the trailing
// zero-size field padding rule.
package zerosize

// StringSet is an allocation-free set of strings backed by map[string]struct{}:
// the key carries all the information and the value costs no storage.
type StringSet struct {
	m map[string]struct{}
}

// NewStringSet returns an empty set.
func NewStringSet() *StringSet {
	return &StringSet{m: make(map[string]struct{})}
}

// Add inserts key; adding an existing key is a no-op.
func (s *StringSet) Add(key string) { s.m[key] = struct{}{} }

// Has reports whether key is present.
func (s *StringSet) Has(key string) bool {
	_, ok := s.m[key]
	return ok
}

// Delete removes key; deleting an absent key is a no-op.
func (s *StringSet) Delete(key string) { delete(s.m, key) }

// Len reports the number of distinct keys.
func (s *StringSet) Len() int { return len(s.m) }

// TrailingZero ends in a zero-size field. Go pads it so &_ does not point past
// the allocation, making it larger than LeadingZero despite identical data.
type TrailingZero struct {
	x int32
	_ [0]byte
}

// LeadingZero puts the zero-size field first, so no trailing padding is added.
type LeadingZero struct {
	_ [0]byte
	x int32
}
```

### The runnable demo

The demo uses the set to deduplicate a stream of idempotency keys — the same key
arriving twice is processed once — and prints the zero-size layout facts.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"unsafe"

	"example.com/zerosize"
)

func main() {
	seen := zerosize.NewStringSet()
	incoming := []string{"req-1", "req-2", "req-1", "req-3", "req-2", "req-1"}

	processed := 0
	for _, key := range incoming {
		if seen.Has(key) {
			continue // idempotent: already handled
		}
		seen.Add(key)
		processed++
	}
	fmt.Printf("received %d, processed %d distinct\n", len(incoming), processed)
	fmt.Printf("set size: %d\n", seen.Len())

	fmt.Printf("Sizeof(struct{}{}) = %d\n", unsafe.Sizeof(struct{}{}))
	fmt.Printf("TrailingZero = %d, LeadingZero = %d\n",
		unsafe.Sizeof(zerosize.TrailingZero{}), unsafe.Sizeof(zerosize.LeadingZero{}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (on a 64-bit platform):

```
received 6, processed 3 distinct
set size: 3
Sizeof(struct{}{}) = 0
TrailingZero = 8, LeadingZero = 4
```

### Tests

Create `set_test.go`:

```go
package zerosize

import (
	"testing"
	"unsafe"
)

func TestEmptyStructIsZeroSize(t *testing.T) {
	t.Parallel()

	if got := unsafe.Sizeof(struct{}{}); got != 0 {
		t.Errorf("Sizeof(struct{}{}) = %d, want 0", got)
	}
	if got := unsafe.Sizeof([0]byte{}); got != 0 {
		t.Errorf("Sizeof([0]byte{}) = %d, want 0", got)
	}
}

func TestTrailingZeroSizeFieldPadsStruct(t *testing.T) {
	t.Parallel()

	trailing := unsafe.Sizeof(TrailingZero{})
	leading := unsafe.Sizeof(LeadingZero{})
	if trailing <= leading {
		t.Errorf("TrailingZero = %d, LeadingZero = %d; a trailing zero-size field must enlarge the struct", trailing, leading)
	}
}

func TestSetOperations(t *testing.T) {
	t.Parallel()

	s := NewStringSet()
	if s.Len() != 0 || s.Has("x") {
		t.Fatal("new set should be empty")
	}

	steps := []struct {
		op      string
		key     string
		wantLen int
		wantHas bool
	}{
		{"add", "a", 1, true},
		{"add", "a", 1, true}, // idempotent
		{"add", "b", 2, true},
		{"delete", "a", 1, false},
		{"delete", "z", 1, false}, // absent
		{"add", "c", 2, true},
	}
	for _, st := range steps {
		switch st.op {
		case "add":
			s.Add(st.key)
		case "delete":
			s.Delete(st.key)
		}
		if s.Len() != st.wantLen {
			t.Errorf("after %s %q: Len = %d, want %d", st.op, st.key, s.Len(), st.wantLen)
		}
		if got := s.Has(st.key); got != st.wantHas {
			t.Errorf("after %s %q: Has = %v, want %v", st.op, st.key, got, st.wantHas)
		}
	}
}
```

## Review

The set is correct because presence is the only fact it stores: `map[string]
struct{}` keeps the keys and spends no bytes on values, which is why it is the
idiomatic set for dedup and idempotency. The subtlety the exercise pins is that a
zero-size type is free only when it is not the last field — Go pads a struct whose
final field is zero-size so `&lastField` stays inside the allocation, which makes
`TrailingZero` larger than `LeadingZero` despite identical data. If you ever add a
zero-size sentinel field to a struct (a common trick for the `structs.HostLayout`
marker or a `noCopy`), put it first, not last.

## Resources

- [The empty struct](https://dave.cheney.net/2014/03/25/the-empty-struct) — Dave Cheney on `struct{}` and the zero-byte set.
- [Go spec: Size and alignment guarantees](https://go.dev/ref/spec#Size_and_alignment_guarantees) — the rule that a struct with a final zero-size field is padded.
- [unsafe.Sizeof](https://pkg.go.dev/unsafe#Sizeof) — measuring the trailing-field padding.

---

Back to [08-wire-header-padding-trap.md](08-wire-header-padding-trap.md) | Next: [10-slice-of-structs-vs-struct-of-slices.md](10-slice-of-structs-vs-struct-of-slices.md)
