# Exercise 6: Legitimate Internal Panic: Unwinding a Recursive Validator to an Error

There is one place inside a package where `panic` is idiomatic: bailing out of
deep recursion. A recursive-descent validator that hits a violation ten frames
down should not thread an error back through every return; it panics with a
*private* sentinel type and recovers once, at its exported boundary, converting
that sentinel to a returned error. The non-negotiable rule that makes this safe:
on recover, if the value is not the package's own sentinel, re-panic — never let a
real runtime bug hide behind the parser's boundary. This module builds that
validator.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
configvalidator/           independent module: example.com/configvalidator
  go.mod                   go 1.26
  validator.go             Node, parseError (private), Validate, validator.walk
  cmd/
    demo/
      main.go              runnable demo: one valid config, one invalid
  validator_test.go        valid/invalid/duplicate/deep + re-panic-on-bug case
```

Files: `validator.go`, `cmd/demo/main.go`, `validator_test.go`.
Implement: `Validate(root Node) error` that recovers a private `*parseError` at the boundary (wrapping it with `%w`) and re-panics anything else; a recursive `walk` that panics `*parseError` on violations.
Test: valid input returns nil; deeply nested / duplicate / empty-key input returns the sentinel-derived error (`errors.As` `*parseError`); a white-box case injects a real runtime bug inside recursion and asserts the boundary RE-PANICS instead of returning it.
Verify: `go test -count=1 -race ./...`

### Why panic here, and why the re-panic rule is sacred

A config tree is recursive, and validation rules apply at arbitrary depth: an empty
key, nesting past a depth cap, a leaf with no value, a duplicate reference. Writing
that with error returns means every recursive call checks and propagates an error
through the whole tree — a lot of `if err != nil { return err }` noise for what is
really "stop everything, this tree is invalid." Panicking with a private sentinel
type (`*parseError`) collapses that: `walk` panics the moment it finds a violation,
and the exported `Validate` recovers once and turns it into a normal error. The
sentinel is *private* precisely so no other package can panic with the same type
and no caller can depend on the panic escaping — the panic is an implementation
detail that never crosses the package boundary.

The discipline that keeps this from becoming the "recover in a library" anti-pattern
is the classification in the recover: `rec.(*parseError)` succeeds only for the
package's own control-flow panic. If the recovered value is anything else — a nil
map write, a nil deref, some genuine bug in `walk` — the type assertion fails and
`Validate` re-panics it. That is the line between a legitimate internal panic and a
swallowed bug. Without the re-panic, a real defect inside the parser would be
reported to the caller as "your config is invalid," sending them chasing a
non-existent config problem while your actual bug hides. The validator here carries
a small, real feature that can expose such a bug: it dedupes `ref` nodes through a
`seen` map, and a construction path that forgot to initialize that map would make a
`ref` node trigger a nil-map-write panic deep in recursion. The test proves that
panic re-raises rather than masquerading as a validation error.

`Validate` uses a named return `err` so the deferred recover can set the result,
and wraps the sentinel with `%w` so callers can `errors.As` it back to `*parseError`
for structured handling (the path and message).

Create `validator.go`:

```go
package configvalidator

import "fmt"

// maxDepth caps nesting so a pathological config cannot blow the stack.
const maxDepth = 32

// Node is one node of a recursive config tree.
type Node struct {
	Key      string
	Value    string
	Children []Node
}

// parseError is the package's PRIVATE control-flow panic. It never escapes the
// package: Validate recovers it and returns it as a normal error.
type parseError struct {
	path string
	msg  string
}

func (e *parseError) Error() string {
	return fmt.Sprintf("at %s: %s", e.path, e.msg)
}

type validator struct {
	seen map[string]bool // ref targets already encountered, for dedup
}

// Validate checks a config tree, converting the internal *parseError panic into a
// returned error. Any other recovered value is re-panicked: it is a real bug the
// parser must not swallow behind its boundary.
func Validate(root Node) (err error) {
	v := &validator{seen: make(map[string]bool)}
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		if pe, ok := rec.(*parseError); ok {
			err = fmt.Errorf("config invalid: %w", pe)
			return
		}
		panic(rec) // not our sentinel: a real bug, let it crash
	}()
	v.walk(root, "", 0)
	return nil
}

func (v *validator) walk(n Node, path string, depth int) {
	here := path + "/" + n.Key
	if n.Key == "" {
		panic(&parseError{path: path, msg: "empty key"})
	}
	if depth > maxDepth {
		panic(&parseError{path: here, msg: "nesting exceeds max depth"})
	}
	if n.Key == "ref" {
		if v.seen[n.Value] {
			panic(&parseError{path: here, msg: "duplicate ref " + n.Value})
		}
		v.seen[n.Value] = true // panics if seen was never initialized (a bug)
	}
	if len(n.Children) == 0 && n.Value == "" {
		panic(&parseError{path: here, msg: "leaf has no value"})
	}
	for _, c := range n.Children {
		v.walk(c, here, depth+1)
	}
}
```

### The runnable demo

The demo validates one well-formed config and one with a duplicate `ref`, printing
the returned error for each. No panic escapes either call.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configvalidator"
)

func main() {
	good := configvalidator.Node{
		Key: "service",
		Children: []configvalidator.Node{
			{Key: "name", Value: "billing"},
			{Key: "ref", Value: "db-primary"},
			{Key: "ref", Value: "cache"},
		},
	}
	bad := configvalidator.Node{
		Key: "service",
		Children: []configvalidator.Node{
			{Key: "ref", Value: "db-primary"},
			{Key: "ref", Value: "db-primary"},
		},
	}

	fmt.Printf("good: %v\n", configvalidator.Validate(good))
	fmt.Printf("bad:  %v\n", configvalidator.Validate(bad))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
good: <nil>
bad:  config invalid: at /service/ref: duplicate ref db-primary
```

### Tests

The tests cover the four validation failures and the critical re-panic case.
`TestValid` asserts a well-formed tree returns `nil`. `TestValidationErrors` is a
table over empty key, duplicate ref, missing leaf value, and over-deep nesting,
each asserting a non-nil error that `errors.As` back to `*parseError`.
`TestRePanicsOnRuntimeBug` is white-box: it builds a `validator` with a nil `seen`
map and drives `walk` through a `ref` node so the nil-map write panics; it asserts
`Validate`'s boundary logic re-panics a `runtime.Error` rather than returning it.

Create `validator_test.go`:

```go
package configvalidator

import (
	"errors"
	"fmt"
	"runtime"
	"testing"
)

func deepChain(depth int) Node {
	root := Node{Key: "n", Value: "leaf"}
	for range depth {
		root = Node{Key: "n", Children: []Node{root}}
	}
	return root
}

func TestValid(t *testing.T) {
	t.Parallel()

	root := Node{
		Key: "service",
		Children: []Node{
			{Key: "name", Value: "billing"},
			{Key: "ref", Value: "db"},
		},
	}
	if err := Validate(root); err != nil {
		t.Fatalf("Validate = %v, want nil", err)
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		root Node
	}{
		{"empty key", Node{Key: "root", Children: []Node{{Key: "", Value: "x"}}}},
		{"duplicate ref", Node{Key: "root", Children: []Node{
			{Key: "ref", Value: "db"}, {Key: "ref", Value: "db"},
		}}},
		{"missing leaf value", Node{Key: "root", Children: []Node{{Key: "leaf"}}}},
		{"too deep", deepChain(maxDepth + 5)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tc.root)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			var pe *parseError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v, want it to unwrap to *parseError", err)
			}
		})
	}
}

func TestRePanicsOnRuntimeBug(t *testing.T) {
	t.Parallel()

	// A validator whose seen map was never initialized: a ref node will trigger
	// a nil-map-write runtime panic deep in walk. The boundary must RE-PANIC it,
	// not report it as a validation error.
	bug := &validator{} // seen is nil
	root := Node{Key: "root", Children: []Node{{Key: "ref", Value: "db"}}}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if _, ok := rec.(*parseError); ok {
				t.Errorf("boundary swallowed a runtime bug as a parseError")
				return
			}
			panic(rec) // mirror Validate's discipline
		}()
		bug.walk(root, "", 0)
	}()

	if recovered == nil {
		t.Fatal("expected the runtime bug to propagate past the boundary")
	}
	err, ok := recovered.(error)
	if !ok {
		t.Fatalf("recovered %T, want an error", recovered)
	}
	var re runtime.Error
	if !errors.As(err, &re) {
		t.Fatalf("recovered %v, want a runtime.Error (nil map write)", err)
	}
}

func ExampleValidate() {
	err := Validate(Node{Key: "root", Value: "leaf"})
	fmt.Println(err)
	// Output: <nil>
}
```

## Review

The validator is correct when a well-formed tree returns `nil`, every validation
failure returns an error that `errors.As` back to `*parseError`, and — the point of
the module — an unexpected runtime panic inside `walk` re-panics past the boundary
instead of being reported as a config error. The private sentinel type is what
makes the internal panic safe: no other package can forge it, and the boundary's
`rec.(*parseError)` check is the exact discriminator between "my own control-flow
unwind" and "a bug I must not own." The trap this closes is the tempting
`defer func(){ recover() }()` that catches *everything* at a package edge; that
turns your parser into a bug-hiding machine. Recover your sentinel, re-panic the
rest.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the "panic to unwind, recover at the boundary" pattern (see the JSON scanner example).
- [Go Language Specification: Handling panics](https://go.dev/ref/spec#Handling_panics) — the exact semantics of recover in a deferred function.
- [errors.As](https://pkg.go.dev/errors#As) — recovering the concrete *parseError from the wrapped return.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-cross-goroutine-recover-trap.md](07-cross-goroutine-recover-trap.md)
