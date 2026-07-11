# Exercise 6: Hash Built-ins

Hashes are the language's associative containers, and like arrays they are immutable at the language level: `set`, `delete`, and `merge` all return a new hash and never touch the original. These six built-ins — `keys`, `values`, `hasKey`, `merge`, `delete`, `set` — also surface the hashability rule, since only Integer, String, and Boolean can be keys, and a `set` or `delete` with an unhashable key must error rather than silently drop the operation.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
object.go     Hash, HashKey, HashPair, HashKeyOf, Array, and the value types
registry.go   Builtin, RegisterBuiltin, Dispatch, newError
hash.go       keys, values, hasKey, merge, delete, set and registration
cmd/
  demo/
    main.go   immutable set / delete / merge, asserted by lookups
hash_test.go  immutability, override-on-merge, unhashable-key error
```

- Files: `object.go`, `registry.go`, `hash.go`, `cmd/demo/main.go`, `hash_test.go`.
- Implement: `keys`, `values`, `hasKey`, `merge`, `delete`, `set`, and the registry framework. The `Hash` is keyed by a canonical `HashKey` computed by `HashKeyOf`.
- Test: that `set` and `delete` return new hashes without mutating the original, that `merge` lets the second hash override the first, and that an unhashable key errors.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p hashbuiltins/cmd/demo && cd hashbuiltins
go mod init example.com/hashbuiltins
```

### Canonical keys and immutable rebuilds

A hash cannot key directly on an `Object` pointer — two distinct `*String{Value: "name"}` allocations would be different keys despite being equal values. `HashKeyOf` solves this by reducing a hashable object to a `HashKey` struct of `{Type, Value}` where `Value` is a canonical string form; two equal strings produce the same `HashKey`, so the Go map behaves as a value-keyed map. Only Integer, String, and Boolean are reducible this way, and `HashKeyOf` returns `false` for anything else — that boolean is what `set`, `delete`, and `hasKey` check before touching the map.

The immutability discipline is identical to the array case, applied to maps. `set` allocates a fresh `map[HashKey]HashPair`, copies every existing pair, then writes the new one — so the argument hash is unchanged and any binding that held it still sees the old contents. `delete` copies every pair except the target. `merge` copies the first hash, then overlays the second, so the second hash's entries win on a key collision. `keys` and `values` walk the pairs into a new array; their order is whatever Go's map iteration yields, which is randomized, so callers must never depend on it.

Create `object.go`:

```go
package hashbuiltins

import (
	"fmt"
	"strings"
)

// ObjectType names the runtime kind of an Object.
type ObjectType string

const (
	INTEGER_OBJ ObjectType = "INTEGER"
	STRING_OBJ  ObjectType = "STRING"
	BOOLEAN_OBJ ObjectType = "BOOLEAN"
	NULL_OBJ    ObjectType = "NULL"
	ARRAY_OBJ   ObjectType = "ARRAY"
	HASH_OBJ    ObjectType = "HASH"
	ERROR_OBJ   ObjectType = "ERROR"
)

// Object is the runtime value interface shared across the interpreter.
type Object interface {
	Type() ObjectType
	Inspect() string
}

// Integer holds an int64 value.
type Integer struct{ Value int64 }

func (i *Integer) Type() ObjectType { return INTEGER_OBJ }
func (i *Integer) Inspect() string  { return fmt.Sprintf("%d", i.Value) }

// String holds a Go string value.
type String struct{ Value string }

func (s *String) Type() ObjectType { return STRING_OBJ }
func (s *String) Inspect() string  { return s.Value }

// Boolean holds a bool value.
type Boolean struct{ Value bool }

func (b *Boolean) Type() ObjectType { return BOOLEAN_OBJ }
func (b *Boolean) Inspect() string  { return fmt.Sprintf("%t", b.Value) }

// Null represents the absence of a value.
type Null struct{}

func (n *Null) Type() ObjectType { return NULL_OBJ }
func (n *Null) Inspect() string  { return "null" }

// Array holds an ordered list of Objects.
type Array struct{ Elements []Object }

func (a *Array) Type() ObjectType { return ARRAY_OBJ }
func (a *Array) Inspect() string {
	parts := make([]string, len(a.Elements))
	for i, e := range a.Elements {
		parts[i] = e.Inspect()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// HashKey is the canonical map key for Hash entries.
type HashKey struct {
	Type  ObjectType
	Value string
}

// HashPair holds one key-value pair in a Hash.
type HashPair struct {
	Key   Object
	Value Object
}

// Hash maps hashable keys to objects.
type Hash struct{ Pairs map[HashKey]HashPair }

func (h *Hash) Type() ObjectType { return HASH_OBJ }
func (h *Hash) Inspect() string {
	parts := make([]string, 0, len(h.Pairs))
	for _, p := range h.Pairs {
		parts = append(parts, p.Key.Inspect()+": "+p.Value.Inspect())
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Error is a runtime error value that propagates without panicking.
type Error struct{ Message string }

func (e *Error) Type() ObjectType { return ERROR_OBJ }
func (e *Error) Inspect() string  { return "ERROR: " + e.Message }

// Singletons avoid allocating new booleans and nulls on every built-in call.
var (
	NullVal  = &Null{}
	TrueVal  = &Boolean{Value: true}
	FalseVal = &Boolean{Value: false}
)

// BoolObj returns the singleton Boolean for v.
func BoolObj(v bool) *Boolean {
	if v {
		return TrueVal
	}
	return FalseVal
}

// HashKeyOf returns the canonical HashKey for o and reports whether o is
// hashable. Only Integer, String, and Boolean are hashable.
func HashKeyOf(o Object) (HashKey, bool) {
	switch v := o.(type) {
	case *Integer:
		return HashKey{Type: INTEGER_OBJ, Value: fmt.Sprintf("%d", v.Value)}, true
	case *String:
		return HashKey{Type: STRING_OBJ, Value: v.Value}, true
	case *Boolean:
		return HashKey{Type: BOOLEAN_OBJ, Value: fmt.Sprintf("%t", v.Value)}, true
	}
	return HashKey{}, false
}
```

### The registry framework

Create `registry.go`:

```go
package hashbuiltins

import "fmt"

// BuiltinFunction is the signature every built-in satisfies.
type BuiltinFunction func(args ...Object) Object

// Builtin holds a function with its arity contract and documentation.
type Builtin struct {
	Name    string
	MinArgs int // -1 means no lower bound
	MaxArgs int // -1 means no upper bound (variadic)
	Doc     string
	Fn      BuiltinFunction
}

// BuiltinOption modifies a Builtin at registration time.
type BuiltinOption func(*Builtin)

// WithArity sets the inclusive [min, max] argument-count bounds.
func WithArity(min, max int) BuiltinOption {
	return func(b *Builtin) { b.MinArgs = min; b.MaxArgs = max }
}

// WithDoc attaches a one-line documentation string.
func WithDoc(doc string) BuiltinOption {
	return func(b *Builtin) { b.Doc = doc }
}

// Registry maps built-in names to their Builtin descriptors.
var Registry = make(map[string]*Builtin)

// RegisterBuiltin adds fn to the Registry under name.
func RegisterBuiltin(name string, fn BuiltinFunction, opts ...BuiltinOption) {
	b := &Builtin{Name: name, MinArgs: -1, MaxArgs: -1, Fn: fn}
	for _, opt := range opts {
		opt(b)
	}
	Registry[name] = b
}

// Lookup returns the Builtin for name, or nil if not registered.
func Lookup(name string) *Builtin { return Registry[name] }

// Dispatch validates arity, then calls the named built-in.
func Dispatch(name string, args ...Object) Object {
	b, ok := Registry[name]
	if !ok {
		return newError("undefined builtin: %q", name)
	}
	if err := checkArgs(b, args); err != nil {
		return err
	}
	return b.Fn(args...)
}

// checkArgs validates len(args) against b's arity contract.
func checkArgs(b *Builtin, args []Object) *Error {
	n := len(args)
	if b.MinArgs >= 0 && n < b.MinArgs {
		return newError("%s: want at least %d arg(s), got %d", b.Name, b.MinArgs, n)
	}
	if b.MaxArgs >= 0 && n > b.MaxArgs {
		return newError("%s: want at most %d arg(s), got %d", b.Name, b.MaxArgs, n)
	}
	return nil
}

func newError(format string, a ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, a...)}
}
```

### The hash built-ins

Create `hash.go`:

```go
package hashbuiltins

func init() {
	RegisterBuiltin("keys", builtinKeys, WithArity(1, 1),
		WithDoc("keys(hash) – return array of keys"))
	RegisterBuiltin("values", builtinValues, WithArity(1, 1),
		WithDoc("values(hash) – return array of values"))
	RegisterBuiltin("hasKey", builtinHasKey, WithArity(2, 2),
		WithDoc("hasKey(hash, key) – return boolean"))
	RegisterBuiltin("merge", builtinMerge, WithArity(2, 2),
		WithDoc("merge(h1, h2) – return new hash; h2 entries override h1"))
	RegisterBuiltin("delete", builtinDelete, WithArity(2, 2),
		WithDoc("delete(hash, key) – return new hash without key"))
	RegisterBuiltin("set", builtinSet, WithArity(3, 3),
		WithDoc("set(hash, key, value) – return new hash with key set to value"))
}

func builtinKeys(args ...Object) Object {
	hash, ok := args[0].(*Hash)
	if !ok {
		return newError("keys: arg 1: want HASH, got %s", args[0].Type())
	}
	keys := make([]Object, 0, len(hash.Pairs))
	for _, pair := range hash.Pairs {
		keys = append(keys, pair.Key)
	}
	return &Array{Elements: keys}
}

func builtinValues(args ...Object) Object {
	hash, ok := args[0].(*Hash)
	if !ok {
		return newError("values: arg 1: want HASH, got %s", args[0].Type())
	}
	vals := make([]Object, 0, len(hash.Pairs))
	for _, pair := range hash.Pairs {
		vals = append(vals, pair.Value)
	}
	return &Array{Elements: vals}
}

func builtinHasKey(args ...Object) Object {
	hash, ok := args[0].(*Hash)
	if !ok {
		return newError("hasKey: arg 1: want HASH, got %s", args[0].Type())
	}
	hk, hashable := HashKeyOf(args[1])
	if !hashable {
		return newError("hasKey: arg 2: type %s is not hashable", args[1].Type())
	}
	_, exists := hash.Pairs[hk]
	return BoolObj(exists)
}

func builtinMerge(args ...Object) Object {
	h1, ok1 := args[0].(*Hash)
	h2, ok2 := args[1].(*Hash)
	if !ok1 {
		return newError("merge: arg 1: want HASH, got %s", args[0].Type())
	}
	if !ok2 {
		return newError("merge: arg 2: want HASH, got %s", args[1].Type())
	}
	newPairs := make(map[HashKey]HashPair, len(h1.Pairs)+len(h2.Pairs))
	for k, v := range h1.Pairs {
		newPairs[k] = v
	}
	for k, v := range h2.Pairs {
		newPairs[k] = v // h2 overrides h1
	}
	return &Hash{Pairs: newPairs}
}

func builtinDelete(args ...Object) Object {
	hash, ok := args[0].(*Hash)
	if !ok {
		return newError("delete: arg 1: want HASH, got %s", args[0].Type())
	}
	hk, hashable := HashKeyOf(args[1])
	if !hashable {
		return newError("delete: arg 2: type %s is not hashable", args[1].Type())
	}
	newPairs := make(map[HashKey]HashPair, len(hash.Pairs))
	for k, v := range hash.Pairs {
		if k != hk {
			newPairs[k] = v
		}
	}
	return &Hash{Pairs: newPairs}
}

func builtinSet(args ...Object) Object {
	hash, ok := args[0].(*Hash)
	if !ok {
		return newError("set: arg 1: want HASH, got %s", args[0].Type())
	}
	hk, hashable := HashKeyOf(args[1])
	if !hashable {
		return newError("set: arg 2: type %s is not hashable", args[1].Type())
	}
	newPairs := make(map[HashKey]HashPair, len(hash.Pairs)+1)
	for k, v := range hash.Pairs {
		newPairs[k] = v
	}
	newPairs[hk] = HashPair{Key: args[1], Value: args[2]}
	return &Hash{Pairs: newPairs}
}
```

### The runnable demo

Because map iteration order is randomized, the demo never prints `keys` or `values` directly — it asserts behavior through `hasKey` and targeted lookups, which are deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/hashbuiltins"
)

func main() {
	empty := &hashbuiltins.Hash{Pairs: make(map[hashbuiltins.HashKey]hashbuiltins.HashPair)}
	h1 := hashbuiltins.Dispatch("set", empty, &hashbuiltins.String{Value: "name"}, &hashbuiltins.String{Value: "Alice"}).(*hashbuiltins.Hash)
	h2 := hashbuiltins.Dispatch("set", h1, &hashbuiltins.String{Value: "age"}, &hashbuiltins.Integer{Value: 30}).(*hashbuiltins.Hash)

	fmt.Println("hasKey name:    ", hashbuiltins.Dispatch("hasKey", h2, &hashbuiltins.String{Value: "name"}).Inspect())
	fmt.Println("hasKey missing: ", hashbuiltins.Dispatch("hasKey", h2, &hashbuiltins.String{Value: "missing"}).Inspect())

	h3 := hashbuiltins.Dispatch("delete", h2, &hashbuiltins.String{Value: "age"}).(*hashbuiltins.Hash)
	fmt.Println("after delete, hasKey age:", hashbuiltins.Dispatch("hasKey", h3, &hashbuiltins.String{Value: "age"}).Inspect())
	fmt.Println("original still has age:  ", hashbuiltins.Dispatch("hasKey", h2, &hashbuiltins.String{Value: "age"}).Inspect())

	override := &hashbuiltins.Hash{Pairs: make(map[hashbuiltins.HashKey]hashbuiltins.HashPair)}
	override = hashbuiltins.Dispatch("set", override, &hashbuiltins.String{Value: "name"}, &hashbuiltins.String{Value: "Bob"}).(*hashbuiltins.Hash)
	merged := hashbuiltins.Dispatch("merge", h3, override).(*hashbuiltins.Hash)
	nameKey, _ := hashbuiltins.HashKeyOf(&hashbuiltins.String{Value: "name"})
	fmt.Println("merged name:             ", merged.Pairs[nameKey].Value.Inspect())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hasKey name:     true
hasKey missing:  false
after delete, hasKey age: false
original still has age:   true
merged name:              Bob
```

### Tests

The tests assert membership rather than iteration order, and they verify immutability by checking the original hash still has a key after a `delete` returns a hash without it.

Create `hash_test.go`:

```go
package hashbuiltins

import (
	"fmt"
	"testing"
)

func TestRegistryPopulated(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"keys", "values", "hasKey", "merge", "delete", "set"} {
		if Lookup(name) == nil {
			t.Errorf("built-in %q not registered", name)
		}
	}
}

func TestBuiltinHashOperations(t *testing.T) {
	t.Parallel()

	empty := &Hash{Pairs: make(map[HashKey]HashPair)}
	h1 := Dispatch("set", empty, &String{Value: "name"}, &String{Value: "Alice"}).(*Hash)
	h2 := Dispatch("set", h1, &String{Value: "age"}, &Integer{Value: 30}).(*Hash)

	if Dispatch("hasKey", h2, &String{Value: "name"}).(*Boolean).Value != true {
		t.Fatal("hasKey 'name' should be true")
	}
	if Dispatch("hasKey", h2, &String{Value: "missing"}).(*Boolean).Value != false {
		t.Fatal("hasKey 'missing' should be false")
	}

	h3 := Dispatch("delete", h2, &String{Value: "age"}).(*Hash)
	if Dispatch("hasKey", h3, &String{Value: "age"}).(*Boolean).Value != false {
		t.Fatal("delete should remove key")
	}
	if Dispatch("hasKey", h2, &String{Value: "age"}).(*Boolean).Value != true {
		t.Fatal("delete mutated the original hash")
	}

	h4 := &Hash{Pairs: make(map[HashKey]HashPair)}
	h4 = Dispatch("set", h4, &String{Value: "name"}, &String{Value: "Bob"}).(*Hash)
	merged := Dispatch("merge", h3, h4).(*Hash)

	nameKey, _ := HashKeyOf(&String{Value: "name"})
	if merged.Pairs[nameKey].Value.(*String).Value != "Bob" {
		t.Fatal("merge: h4 should override h3 value for 'name'")
	}
}

func TestBuiltinHashUnhashableKeyErrors(t *testing.T) {
	t.Parallel()

	h := &Hash{Pairs: make(map[HashKey]HashPair)}
	result := Dispatch("set", h, &Array{Elements: nil}, &Integer{Value: 1})
	if result.Type() != ERROR_OBJ {
		t.Fatal("set with unhashable key (array) should return error")
	}
}

// ExampleDispatch sets a key and reads it back through hasKey.
func ExampleDispatch() {
	h := &Hash{Pairs: make(map[HashKey]HashPair)}
	h = Dispatch("set", h, &String{Value: "lang"}, &String{Value: "Monkey"}).(*Hash)
	fmt.Println(Dispatch("hasKey", h, &String{Value: "lang"}).Inspect())
	// Output: true
}
```

## Review

The module is correct when every mutating operation returns a new hash and unhashable keys are rejected. Confirm that `set` and `delete` leave the argument hash unchanged — the immutability test checks the original still has a key after `delete` returns a hash without it — that `merge` lets the second hash override the first on a key collision, and that `set`/`delete`/`hasKey` return an `*Error` when handed an unhashable key like an array. Never assert on the order `keys` or `values` returns.

Common mistakes for this feature. Calling Go's built-in `delete(map, k)` on the argument's own `Pairs` map mutates the original; allocate a new map and copy the survivors. Copying `h2` first and then `h1` in `merge` makes the first hash win instead of the second. Asserting on `keys` output order produces a test that fails intermittently because Go randomizes map iteration. And using the `Object` pointer as the map key instead of the canonical `HashKey` makes two equal strings miss each other.

## Resources

- [Go maps in action](https://go.dev/blog/maps) — why map iteration order is unspecified and randomized.
- [Go spec: deletion](https://go.dev/ref/spec#Deletion_of_map_elements) — the built-in `delete`, which mutates and must not be used on the argument map directly.
- [Writing An Interpreter In Go (Thorsten Ball)](https://interpreterbook.com/) — the `Hashable`/`HashKey` design this module's `HashKeyOf` mirrors.

---

Back to [05-math-builtins.md](05-math-builtins.md) | Next: [07-io-builtins.md](07-io-builtins.md)
