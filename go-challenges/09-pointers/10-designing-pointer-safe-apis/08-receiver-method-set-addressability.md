# Exercise 8: Pointer vs Value Receivers — Why Your Type Fails the Interface

You register a type under an interface, the code compiles in isolation, and then a
map of that interface silently holds the wrong thing — or refuses to compile in a
way whose error message points nowhere useful. The cause is method sets: a value
`T` does not carry pointer-receiver methods. This module builds a `Validator`
registry that only works when you store pointers, and pins the rule with a
compile-time assertion.

This module is fully self-contained: its own `go mod init`, its own types, its own
demo, and its own tests. Nothing here imports another exercise.

## What you'll build

```text
validator/                  independent module: example.com/validator
  go.mod                    go 1.25
  validator.go              Validator interface; EmailRule/RangeRule with pointer-receiver Validate; Registry
  cmd/
    demo/
      main.go               register two *rules, dispatch Validate polymorphically
  validator_test.go         compile-time assertions, polymorphic dispatch, mutation-through-interface tests
```

- Files: `validator.go`, `cmd/demo/main.go`, `validator_test.go`.
- Implement: a `Validator` interface with `Validate(string) error`, two rule types whose `Validate` (and a stateful counter method) are on the pointer receiver, and a `Registry` (`map[string]Validator`) that stores pointers.
- Test: a compile-time `var _ Validator = (*EmailRule)(nil)` succeeds (with a build note that `var _ Validator = EmailRule{}` would not); a runtime test that a registry of pointers dispatches `Validate` correctly; a test proving mutation through the interface persists (pointer) where a value copy would not; register two implementations and assert correct polymorphic dispatch.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### The method-set rule

A value of type `T` has, in its method set, only the methods declared with a value
receiver. A pointer `*T` has both value- and pointer-receiver methods. So if you
declare `func (r *EmailRule) Validate(s string) error`, then `*EmailRule`
satisfies a `Validator` interface but `EmailRule` (the value) does not. Write
`var v Validator = EmailRule{}` and it fails to compile:
`EmailRule does not implement Validator (Validate method has pointer receiver)`.

The usual reflex — "take the address, `&EmailRule{}`" — works for a local variable
because a local is addressable. It fails exactly where this bug bites: a value
stored in a map is *not addressable*, so `&registry["email"]` is a compile error,
and a value returned from a function is not addressable either. That is why the
registry must store `Validator` values that are already pointers (`*EmailRule`),
not `EmailRule` values you hope to promote later. Store pointers when the
interface's methods have pointer receivers.

There is a second reason to prefer the pointer here beyond satisfaction: these
rules carry mutable state (a count of how many strings they have checked). A
pointer receiver mutates the one rule the registry holds; a value receiver would
mutate a copy and lose the increment. The mutation-through-interface test pins
this.

### The compile-time assertion

`var _ Validator = (*EmailRule)(nil)` is a zero-cost, zero-runtime assertion that
`*EmailRule` satisfies `Validator`. If a future edit changes the receiver or the
method signature, this line stops compiling and names the problem at the type, not
at some distant call site. It is the idiomatic way to document "this pointer type
implements this interface." The commented-out `var _ Validator = EmailRule{}` in
the source records the negative half of the rule without breaking the build.

Create `validator.go`:

```go
package validator

import (
	"fmt"
	"strings"
)

// Validator checks a string and reports why it is invalid. Implementations here
// use pointer receivers because they carry mutable state.
type Validator interface {
	Validate(s string) error
	Checked() int
}

// Compile-time assertions: the POINTER types satisfy Validator.
var (
	_ Validator = (*EmailRule)(nil)
	_ Validator = (*RangeRule)(nil)
)

// Note: `var _ Validator = EmailRule{}` would NOT compile — a value type does
// not carry pointer-receiver methods in its method set.

// EmailRule requires an "@" and counts how many strings it has checked.
type EmailRule struct {
	checked int
}

func (r *EmailRule) Validate(s string) error {
	r.checked++
	if !strings.Contains(s, "@") {
		return fmt.Errorf("%q is not an email", s)
	}
	return nil
}

func (r *EmailRule) Checked() int { return r.checked }

// RangeRule requires a non-empty string no longer than Max.
type RangeRule struct {
	Max     int
	checked int
}

func (r *RangeRule) Validate(s string) error {
	r.checked++
	if len(s) == 0 || len(s) > r.Max {
		return fmt.Errorf("%q length %d out of range (1..%d)", s, len(s), r.Max)
	}
	return nil
}

func (r *RangeRule) Checked() int { return r.checked }

// Registry maps a field name to its Validator. It stores pointers, because the
// method set of the value types would not satisfy Validator.
type Registry struct {
	rules map[string]Validator
}

func NewRegistry() *Registry {
	return &Registry{rules: make(map[string]Validator)}
}

func (reg *Registry) Register(field string, v Validator) {
	reg.rules[field] = v
}

// Validate dispatches to the registered rule for field, if any.
func (reg *Registry) Validate(field, value string) error {
	v, ok := reg.rules[field]
	if !ok {
		return nil
	}
	return v.Validate(value)
}
```

### The runnable demo

The demo registers two pointer rules, runs a few validations through the registry,
and prints each rule's `Checked()` count — which is non-zero only because the
pointer receiver mutated the one stored rule rather than a copy.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/validator"
)

func main() {
	reg := validator.NewRegistry()
	email := &validator.EmailRule{}
	name := &validator.RangeRule{Max: 8}
	reg.Register("email", email)
	reg.Register("name", name)

	fmt.Println("valid email:", reg.Validate("email", "a@b.com") == nil)
	fmt.Println("bad email:  ", reg.Validate("email", "nope") == nil)
	fmt.Println("valid name: ", reg.Validate("name", "alice") == nil)
	fmt.Println("long name:  ", reg.Validate("name", "verylongname") == nil)

	fmt.Printf("email checked=%d name checked=%d\n", email.Checked(), name.Checked())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid email: true
bad email:   false
valid name:  true
long name:   false
email checked=2 name checked=2
```

### Tests

`TestPolymorphicDispatch` registers both rule types and asserts each dispatches to
the right `Validate`. `TestMutationThroughInterfacePersists` calls `Validate`
several times through the interface and asserts the stored rule's `Checked()`
counter advanced — proving the pointer receiver mutated the one instance, not a
copy. The compile-time assertions in `validator.go` are themselves a test: if the
receivers changed, the package would not compile.

Create `validator_test.go`:

```go
package validator

import (
	"fmt"
	"testing"
)

func TestPolymorphicDispatch(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Register("email", &EmailRule{})
	reg.Register("name", &RangeRule{Max: 8})

	tests := []struct {
		field   string
		value   string
		wantErr bool
	}{
		{field: "email", value: "a@b.com", wantErr: false},
		{field: "email", value: "nope", wantErr: true},
		{field: "name", value: "alice", wantErr: false},
		{field: "name", value: "verylongname", wantErr: true},
		{field: "unknown", value: "x", wantErr: false},
	}
	// Subtests are not parallel: they share the same registered rules, whose
	// pointer receivers mutate a Checked counter.
	for _, tc := range tests {
		t.Run(tc.field+"/"+tc.value, func(t *testing.T) {
			err := reg.Validate(tc.field, tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate(%q,%q) err = %v, wantErr = %t", tc.field, tc.value, err, tc.wantErr)
			}
		})
	}
}

func TestMutationThroughInterfacePersists(t *testing.T) {
	t.Parallel()
	email := &EmailRule{}
	var v Validator = email // interface holds the pointer

	_ = v.Validate("a@b.com")
	_ = v.Validate("c@d.com")
	_ = v.Validate("bad")

	if got := v.Checked(); got != 3 {
		t.Fatalf("Checked() = %d, want 3 (pointer receiver must accumulate)", got)
	}
	if email.Checked() != 3 {
		t.Fatal("mutation through the interface did not reach the underlying pointer")
	}
}

func ExampleRegistry() {
	reg := NewRegistry()
	reg.Register("email", &EmailRule{})
	fmt.Println(reg.Validate("email", "a@b.com") == nil)
	fmt.Println(reg.Validate("email", "nope") == nil)
	// Output:
	// true
	// false
}
```

## Review

The registry is correct when it stores `*EmailRule`/`*RangeRule` (which satisfy
`Validator`) and dispatches polymorphically, and when mutation through the
interface reaches the one stored instance — proven by the `Checked()` counter
reaching 3. The compile-time `var _ Validator = (*EmailRule)(nil)` assertions
document and enforce which form implements the interface. The mistake to avoid is
storing value types under the interface: the code either fails to compile with a
"method has pointer receiver" error, or — if you switched the receivers to values
to make it compile — silently loses every mutation because each call operates on a
copy. And you cannot rescue a map-stored value with `&m["k"]`, because map elements
are not addressable. Store pointers when the methods have pointer receivers.

## Resources

- [Go spec: method sets](https://go.dev/ref/spec#Method_sets) — which methods belong to `T` versus `*T`.
- [Effective Go: pointers vs. values](https://go.dev/doc/effective_go#pointers_vs_values) — choosing the receiver type.
- [Go spec: address operators](https://go.dev/ref/spec#Address_operators) — why a map element is not addressable.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-atomic-pointer-config-hot-reload.md](07-atomic-pointer-config-hot-reload.md) | Next: [09-sync-pool-buffer-reuse.md](09-sync-pool-buffer-reuse.md)
