# Exercise 7: Plugin registry dedup — nil vs typed-nil interface equality

A plugin registry that dedups handlers by identity exposes a subtle rule: two
interface values are equal only when both dynamic type and dynamic value are
equal. So a nil interface and a typed nil are not equal, two typed nils of
different types are not equal, and — the hazard — comparing an interface whose
dynamic type is not comparable panics at runtime. This module builds a `Set` and
pins all of it.

## What you'll build

```text
pluginset/                 independent module: example.com/pluginset
  go.mod                   go 1.26
  set.go                   Handler; Set (map-backed); Add; Contains; Len
  cmd/
    demo/
      main.go              dedup two handlers; show nil vs typed-nil equality
  set_test.go              equality table; incomparable-key panic (recover-guarded)
```

- Files: `set.go`, `cmd/demo/main.go`, `set_test.go`.
- Implement: a `Handler` interface, a `Set` backed by `map[Handler]struct{}` with `Add`/`Contains`/`Len`.
- Test: a table asserting `nil==nil`, `nil==typedNil` (false), `typedNil(*A)==typedNil(*B)` (false), `sameTypedNil==sameTypedNil` (true); a recover-guarded subtest proving a slice-backed dynamic type panics on comparison.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/pluginset/cmd/demo
cd ~/go-exercises/pluginset
go mod init example.com/pluginset
```

### Why interface equality is subtle and sometimes fatal

An interface value is a (dynamic type, dynamic value) pair, and `==` compares
them in that order. It first compares the dynamic types; if they differ, the
result is false and the values are never compared. Only if the types match does
it compare the dynamic values. This explains the table:

- `nil == nil` is true: both are the zero interface, both words nil.
- a nil interface `== ` a typed nil is false: the typed nil has dynamic type
  `*handlerA`, the nil interface has none, so the types differ.
- `typedNil(*handlerA) == typedNil(*handlerB)` is false: both dynamic values are
  nil, but the dynamic types differ, so the comparison short-circuits to false.
- `typedNil(*handlerA) == typedNil(*handlerA)` is true: same dynamic type, and
  both dynamic values are the nil pointer, which compares equal.

The hazard is the last step — comparing dynamic values — when the dynamic type
is *not comparable*. If the dynamic type's underlying kind is a slice, map, or
function, comparing two such values panics with `comparing uncomparable type`.
This is not a compile error, because the static type is an interface, which is
comparable at compile time; the panic only appears when a slice-backed
implementation actually shows up at runtime. A `Set` backed by
`map[Handler]struct{}` inherits the hazard directly: inserting a slice-backed
`Handler` as a map key panics, because map keys must be comparable and the
runtime checks the *dynamic* type on insert. That is why a dedup set keyed on an
interface can take a random production panic the first time someone registers a
handler whose concrete type is slice-backed.

Create `set.go`:

```go
package pluginset

// Handler is the plugin contract. Implementations registered in a Set are
// deduplicated by interface identity.
type Handler interface {
	Handle(msg string) error
}

// Set is a dedup set of Handlers keyed by interface identity. Adding a Handler
// whose dynamic type is not comparable panics, the same as any map with an
// incomparable key.
type Set struct {
	items map[Handler]struct{}
}

func NewSet() *Set {
	return &Set{items: make(map[Handler]struct{})}
}

// Add inserts h. Re-adding an equal Handler is a no-op (the dedup property).
func (s *Set) Add(h Handler) {
	s.items[h] = struct{}{}
}

func (s *Set) Contains(h Handler) bool {
	_, ok := s.items[h]
	return ok
}

func (s *Set) Len() int {
	return len(s.items)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pluginset"
)

type auth struct{ name string }

func (a *auth) Handle(msg string) error { return nil }

func main() {
	set := pluginset.NewSet()

	// Two distinct handler identities plus a duplicate of the first.
	a := &auth{name: "jwt"}
	b := &auth{name: "apikey"}
	set.Add(a)
	set.Add(b)
	set.Add(a) // duplicate: no-op
	fmt.Println("distinct handlers:", set.Len())
	fmt.Println("contains a:", set.Contains(a))

	// The two-word equality rule.
	var nilIface pluginset.Handler
	var typedNil pluginset.Handler = (*auth)(nil)
	fmt.Println("nil == nil:", nilIface == nil)
	fmt.Println("nil == typedNil:", nilIface == typedNil)
	fmt.Println("typedNil == nil:", typedNil == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
distinct handlers: 2
contains a: true
nil == nil: true
nil == typedNil: false
typedNil == nil: false
```

### Tests

Create `set_test.go`:

```go
package pluginset

import "testing"

type handlerA struct{}

func (*handlerA) Handle(string) error { return nil }

type handlerB struct{}

func (*handlerB) Handle(string) error { return nil }

// sliceHandler has a slice underlying type, so it is NOT comparable: comparing
// two of them as interface values panics.
type sliceHandler []string

func (sliceHandler) Handle(string) error { return nil }

func TestInterfaceEquality(t *testing.T) {
	t.Parallel()

	var nilIface Handler
	var typedNilA Handler = (*handlerA)(nil)
	var typedNilA2 Handler = (*handlerA)(nil)
	var typedNilB Handler = (*handlerB)(nil)

	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{"nil == nil", nilIface == nil, true},
		{"nil == typedNil(*A)", nilIface == typedNilA, false},
		{"typedNil(*A) == typedNil(*B)", typedNilA == typedNilB, false},
		{"typedNil(*A) == typedNil(*A)", typedNilA == typedNilA2, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.got != tc.want {
				t.Fatalf("%s = %v; want %v", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestSetDedups(t *testing.T) {
	t.Parallel()

	set := NewSet()
	a := &handlerA{}
	b := &handlerB{}
	set.Add(a)
	set.Add(b)
	set.Add(a) // duplicate

	if set.Len() != 2 {
		t.Fatalf("Len() = %d; want 2", set.Len())
	}
	if !set.Contains(a) {
		t.Fatal("set should contain a")
	}
}

func TestComparingIncomparableTypePanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("comparing slice-backed interface values should panic")
		}
	}()

	var x Handler = sliceHandler{"a"}
	var y Handler = sliceHandler{"b"}
	_ = x == y // panics: comparing uncomparable type pluginset.sliceHandler
}

func TestAddIncomparableKeyPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("adding a slice-backed key to the set should panic")
		}
	}()

	set := NewSet()
	set.Add(sliceHandler{"a"}) // panics: map key must be comparable
}
```

## Review

The equality rules are correct because `==` on interfaces compares dynamic type
first, then dynamic value: `TestInterfaceEquality` pins that a nil interface and
a typed nil differ (different types), two typed nils of different types differ,
and two typed nils of the same type are equal. `TestSetDedups` confirms the
map-backed set collapses a re-added handler. The two recover-guarded tests teach
the failure mode: comparing two slice-backed `Handler` values panics with
`comparing uncomparable type`, and adding one as a map key panics the same way,
because the runtime checks the dynamic type's comparability at comparison and at
insert. The lesson for a real dedup/cache path is to key on a stable comparable
identity — a name or an ID — when any implementation might be slice-, map-, or
func-backed.

## Resources

- [Go spec: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — when interface `==` is defined and when it panics.
- [Go spec: Interface types](https://go.dev/ref/spec#Interface_types) — the dynamic type/value model.
- [Go blog: The Laws of Reflection](https://go.dev/blog/laws-of-reflection) — the (type, value) pair behind every interface.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-errors-as-typed-nil-sentinel.md](08-errors-as-typed-nil-sentinel.md)
