# Exercise 6: A nil-Safe Feature-Flag Evaluator (Pointer Receiver, nil Is Valid)

A pointer-receiver method does not automatically panic on a `nil` receiver — it
panics only if it dereferences the nil. That gap is a design tool: you can make
`nil` a legitimate, usable zero value. This module builds a `FlagSet` evaluator
where a `nil *FlagSet` means "no flags configured" and `IsEnabled` returns `false`
for everything instead of crashing — the same nil-safe-receiver pattern used for a
nil `*Tree` node whose methods treat nil as the empty tree.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
featureflags/              independent module: example.com/featureflags
  go.mod
  flags.go                 type FlagSet; New; IsEnabled/Enable guard the nil receiver
  cmd/
    demo/
      main.go              a nil FlagSet and a configured one, side by side
  flags_test.go            nil is false-for-all and never panics; enabled/unknown flags
```

Files: `flags.go`, `cmd/demo/main.go`, `flags_test.go`.
Implement: `FlagSet` over a `map[string]bool`, `New(flags map[string]bool) *FlagSet`, and `IsEnabled(name string) bool` / `Enable(name string)` that check `if fs == nil` before touching any field.
Test: a `nil *FlagSet` reports every flag disabled without panicking, an enabled flag reports true, an unknown flag reports false; a recover-based test pins that the nil path does not panic.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/featureflags/cmd/demo
cd ~/go-exercises/featureflags
go mod init example.com/featureflags
```

### Making nil a usable zero value

In a real service, feature flags are often optional: a code path asks "is flag X
on?" whether or not anyone configured a flag set. The clumsy version threads a
`*FlagSet` everywhere and nil-checks it at every call site
(`if flags != nil && flags.IsEnabled("x")`). The clean version pushes the nil
check *into the method* so a `nil *FlagSet` is a first-class value meaning "no
flags configured" — and every call site simplifies to `flags.IsEnabled("x")`.

This works because of the exact rule from the concepts: calling
`fs.IsEnabled("x")` when `fs` is nil does not panic by itself; it enters the
method with `fs == nil` and only panics if the body dereferences `fs` (reads
`fs.flags`). So the method guards first: `if fs == nil { return false }`. After
that guard, `fs` is known non-nil and the field access is safe. A disabled flag
and an unset flag are the same answer — `false` — which the comma-ok map read
delivers naturally: a missing key yields the zero `bool`, and an explicit `false`
value reads back `false` too.

Contrast this with the `Counter` of Exercise 1, which *forbids* nil by guaranteeing
a non-nil pointer from `New`. Both are valid; the choice is deliberate. `Counter`
has no meaningful "nil counter" so it rules nil out at the constructor; `FlagSet`
has a natural "no flags" meaning for nil so it embraces it. What you may not do is
leave a field access unguarded in a type you advertised as nil-safe — that
reintroduces the panic the pattern exists to avoid.

Create `flags.go`:

```go
package featureflags

// FlagSet evaluates named boolean feature flags. A nil *FlagSet is a valid value
// meaning "no flags configured": every flag reads as disabled.
type FlagSet struct {
	flags map[string]bool
}

// New builds a FlagSet from an initial map. The map may be nil or empty.
func New(flags map[string]bool) *FlagSet {
	if flags == nil {
		flags = make(map[string]bool)
	}
	return &FlagSet{flags: flags}
}

// IsEnabled reports whether the named flag is on. On a nil receiver it returns
// false without panicking; an unknown flag also returns false.
func (fs *FlagSet) IsEnabled(name string) bool {
	if fs == nil {
		return false
	}
	on, ok := fs.flags[name]
	return ok && on
}

// Enable turns a flag on. On a nil receiver it is a no-op (there is no map to
// write into), mirroring IsEnabled's nil tolerance.
func (fs *FlagSet) Enable(name string) {
	if fs == nil {
		return
	}
	fs.flags[name] = true
}
```

### The runnable demo

The demo puts a `nil *FlagSet` next to a configured one and evaluates the same
flag against both, so the "no flags configured" behavior is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/featureflags"
)

func main() {
	var unconfigured *featureflags.FlagSet // nil: no flags configured

	configured := featureflags.New(map[string]bool{
		"new-checkout": true,
		"beta-search":  false,
	})

	fmt.Printf("nil/new-checkout:        %v\n", unconfigured.IsEnabled("new-checkout"))
	fmt.Printf("configured/new-checkout: %v\n", configured.IsEnabled("new-checkout"))
	fmt.Printf("configured/beta-search:  %v\n", configured.IsEnabled("beta-search"))
	fmt.Printf("configured/unknown:      %v\n", configured.IsEnabled("unknown"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
nil/new-checkout:        false
configured/new-checkout: true
configured/beta-search:  false
configured/unknown:      false
```

### Tests

`TestNilFlagSetIsEnabledReturnsFalse` calls the method on a nil receiver directly.
`TestNilPathDoesNotPanic` wraps the call in a `defer recover()` and fails if
anything was recovered — turning "must not panic" into an assertion rather than a
hope. The remaining cases cover an enabled flag, an explicitly-disabled flag, and
an unknown one.

Create `flags_test.go`:

```go
package featureflags

import "testing"

func TestNilFlagSetIsEnabledReturnsFalse(t *testing.T) {
	t.Parallel()

	var fs *FlagSet // nil
	if fs.IsEnabled("anything") {
		t.Fatal("nil FlagSet reported a flag enabled; want false")
	}
}

func TestNilPathDoesNotPanic(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil receiver panicked: %v", r)
		}
	}()

	var fs *FlagSet
	_ = fs.IsEnabled("x")
	fs.Enable("x") // also a no-op on nil, must not panic
}

func TestEnabledFlagReturnsTrue(t *testing.T) {
	t.Parallel()

	fs := New(map[string]bool{"x": true})
	if !fs.IsEnabled("x") {
		t.Fatal("IsEnabled(x) = false, want true")
	}
}

func TestDisabledFlagReturnsFalse(t *testing.T) {
	t.Parallel()

	fs := New(map[string]bool{"x": false})
	if fs.IsEnabled("x") {
		t.Fatal("IsEnabled(x) = true for an explicitly-disabled flag, want false")
	}
}

func TestUnknownFlagReturnsFalse(t *testing.T) {
	t.Parallel()

	fs := New(map[string]bool{"x": true})
	if fs.IsEnabled("y") {
		t.Fatal("IsEnabled(unknown) = true, want false")
	}
}

func TestEnableThenIsEnabled(t *testing.T) {
	t.Parallel()

	fs := New(nil)
	fs.Enable("z")
	if !fs.IsEnabled("z") {
		t.Fatal("Enable(z) did not persist")
	}
}
```

## Review

The evaluator is correct when a `nil *FlagSet` answers `false` for every flag
without panicking, and a configured one distinguishes enabled, disabled, and
unknown flags. The load-bearing detail is the `if fs == nil` guard at the top of
every method: it is what converts "nil receiver" from a panic into a designed,
usable zero value. `TestNilPathDoesNotPanic` is not ceremony — it is the contract,
because a single unguarded field access on the nil path would reintroduce the
crash this whole pattern removes. Weigh the choice deliberately: embrace nil (as
here) when the type has a natural empty meaning, or forbid it in the constructor
(as `Counter` does) when it does not; never leave nil half-handled.

## Resources

- [Go Spec: Method values](https://go.dev/ref/spec#Method_values) — receiver evaluation, including a nil pointer receiver.
- [Go FAQ: Should I define methods on values or pointers?](https://go.dev/doc/faq#methods_on_values_or_pointers) — the receiver-kind decision this pattern rests on.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — pointer-receiver method dispatch, the mechanism behind a nil-safe method.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-silent-mutation-loss-bug.md](05-silent-mutation-loss-bug.md) | Next: [07-method-value-capture-bug.md](07-method-value-capture-bug.md)
