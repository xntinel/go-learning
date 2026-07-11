# Exercise 7: The Typed-Nil Interface Bug

An interface value is nil only when BOTH its type and its value are unset. A nil
`*RepoError` stored into an `error` is `(type=*RepoError, value=nil)`, which is
NOT nil — so `err != nil` fires on the success path and a healthy request returns
500. This module reproduces that production incident with a broken repository,
documents the trap in a test, and then fixes it by returning the interface type
and an explicit nil.

## What you'll build

```text
typednil/                   independent module: example.com/typednil
  go.mod                    go 1.26
  repo.go                   RepoError; BrokenFind (returns *RepoError); FixedFind (returns error)
  cmd/
    demo/
      main.go               shows err!=nil on the broken success path, ==nil on the fix
  repo_test.go              documents the trap; proves the fix; errors.As tuple semantics
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: `BrokenFind` returning a concrete `*RepoError` (nil on success), and `FixedFind` returning the `error` interface with an explicit `nil`.
- Test: reproduce the bug — assign the broken result to an `error` and assert (correctly) that `err != nil` even on success; assert the fixed version returns `err == nil`; use `errors.As` to show the `(type, value)` tuple on the failure path.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/typednil/cmd/demo
cd ~/go-exercises/typednil
go mod init example.com/typednil
```

### Why the guard fires when nothing failed

A Go interface value is a pair: a dynamic type and a dynamic value. `err == nil`
is true only when both halves are unset — the literal untyped `nil`. When you take
a nil pointer of a concrete type and store it into an interface, the type half is
now set (`*RepoError`) even though the value half is nil. The pair is
`(*RepoError, nil)`, which is not equal to the pure-nil pair, so `err != nil` is
true. The pointer is nil; the interface holding it is not.

`BrokenFind` has the classic shape: it returns the concrete type `*RepoError`,
returning a nil pointer on success. That is fine as long as the caller keeps it as
a `*RepoError` and compares the pointer. But callers do not — they store results
in an `error` variable, because that is the universal error type. The instant the
nil `*RepoError` is assigned to `error`, it becomes a non-nil interface, and the
caller's `if err != nil` guard fires on a request that succeeded. In an HTTP
handler that means a 200-worthy request returns 500. This is the single most
cited Go gotcha, and it costs every team one incident to learn.

The fix is a rule, not a workaround: functions that can fail must declare their
return type as the `error` interface, not a concrete error type, and must return
an explicit untyped `nil` on the success path. `FixedFind` returns `error`; on
success it returns the literal `nil`, which is the pure-nil pair, so the caller's
guard behaves. If you construct the concrete error conditionally, keep a variable
of interface type or return `nil` explicitly rather than a typed nil pointer.

Create `repo.go`:

```go
package typednil

import "fmt"

// RepoError is a concrete error type.
type RepoError struct {
	Code string
}

func (e *RepoError) Error() string {
	return fmt.Sprintf("repo error: %s", e.Code)
}

// BrokenFind returns the CONCRETE type *RepoError. On success it returns a nil
// *RepoError. A caller that assigns the result to an `error` variable gets a
// NON-nil interface holding a nil pointer -- the typed-nil trap. Do not do this.
func BrokenFind(id string) (string, *RepoError) {
	if id == "" {
		return "", &RepoError{Code: "EMPTY_ID"}
	}
	return "record:" + id, nil // nil *RepoError, not an untyped nil
}

// FixedFind returns the error INTERFACE and an explicit untyped nil on success,
// so a caller's `if err != nil` guard behaves correctly.
func FixedFind(id string) (string, error) {
	if id == "" {
		return "", &RepoError{Code: "EMPTY_ID"}
	}
	return "record:" + id, nil // untyped nil: the pure-nil interface
}
```

### The runnable demo

The demo shows both functions on the success path. The broken one's result, once
placed in an `error`, is non-nil; the fixed one's is nil.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/typednil"
)

func main() {
	// Broken: success path, but the error variable is non-nil.
	_, repoErr := typednil.BrokenFind("u1")
	var err error = repoErr // assign concrete *RepoError to error interface
	fmt.Printf("broken success: concrete pointer nil? %v; interface err != nil? %v\n",
		repoErr == nil, err != nil)

	// Fixed: success path, error is truly nil.
	_, err = typednil.FixedFind("u1")
	fmt.Printf("fixed success:  err != nil? %v\n", err != nil)

	// Fixed: failure path, error is non-nil and carries the code.
	_, err = typednil.FixedFind("")
	fmt.Printf("fixed failure:  err = %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
broken success: concrete pointer nil? true; interface err != nil? true
fixed success:  err != nil? false
fixed failure:  err = repo error: EMPTY_ID
```

The first line is the whole bug in one sentence: the pointer is nil, yet the
interface holding it is not.

### Tests

`TestTypedNilTrap` documents the trap: after assigning the broken success result
to an `error`, the guard `err != nil` is (wrongly, from the user's view)
satisfied, and the test asserts exactly that to pin the behavior. `TestFixedFind`
asserts the fix returns `err == nil` on success. `TestErrorsAsRecoversConcrete`
uses `errors.As` on the failure path to extract the concrete `*RepoError` and read
its `Code`, showing the `(type, value)` tuple that `errors.As` inspects.
`ExampleFixedFind` pins the success-path output so `go test` verifies the snippet too.

Create `repo_test.go`:

```go
package typednil

import (
	"errors"
	"fmt"
	"testing"
)

// TestTypedNilTrap documents the bug in code: the concrete pointer is nil, but
// once stored in an error interface the value is NOT nil.
func TestTypedNilTrap(t *testing.T) {
	t.Parallel()

	_, repoErr := BrokenFind("u1") // success path
	if repoErr != nil {
		t.Fatalf("concrete *RepoError should be nil on success, got %v", repoErr)
	}

	var err error = repoErr // the fatal assignment
	if err == nil {
		t.Fatal("EXPECTED the typed-nil trap: err should be non-nil here")
	}
	// This is the incident: a success returns a non-nil error, so a handler that
	// does `if err != nil { http.Error(w, ..., 500) }` returns 500 on success.
}

func TestFixedFindSuccessIsNil(t *testing.T) {
	t.Parallel()

	got, err := FixedFind("u1")
	if err != nil {
		t.Fatalf("FixedFind success err = %v, want nil", err)
	}
	if got != "record:u1" {
		t.Fatalf("FixedFind = %q, want record:u1", got)
	}
}

func TestFixedFindFailureIsNonNil(t *testing.T) {
	t.Parallel()

	_, err := FixedFind("")
	if err == nil {
		t.Fatal("FixedFind(\"\") err = nil, want non-nil")
	}
}

// TestErrorsAsRecoversConcrete shows the interface tuple: errors.As matches on
// the dynamic type (*RepoError) and binds the dynamic value into target.
func TestErrorsAsRecoversConcrete(t *testing.T) {
	t.Parallel()

	_, err := FixedFind("")
	var target *RepoError
	if !errors.As(err, &target) {
		t.Fatalf("errors.As did not match *RepoError in %v", err)
	}
	if target.Code != "EMPTY_ID" {
		t.Fatalf("target.Code = %q, want EMPTY_ID", target.Code)
	}
}

// ExampleFixedFind shows the fixed function returning a truly nil error on the
// success path; the // Output line is auto-verified by `go test`.
func ExampleFixedFind() {
	v, err := FixedFind("u1")
	fmt.Println(v, err)
	// Output: record:u1 <nil>
}
```

## Review

The bug is not about pointers being unsafe; it is about the interface pair. The
`TestTypedNilTrap` test asserts `err != nil` on a success path on purpose — it
encodes the trap so a future edit that "fixes" `BrokenFind` by changing its return
type will make the test's expectation visibly stale, which is the point. The rule
to carry away is narrow and absolute: a function that can fail returns `error`,
not a concrete `*T`, and returns an explicit `nil` on success. Never assign a
concrete nil error pointer to an `error` variable. When you must inspect the
concrete error, use `errors.As`, which matches on the dynamic type — that is the
same `(type, value)` tuple that made the trap possible, used deliberately this
time.

## Resources

- [Go FAQ — Why is my nil error value not equal to nil?](https://go.dev/doc/faq#nil_error) — the canonical explanation of the interface `(type, value)` pair.
- [Go blog — Working with Errors (errors.As)](https://go.dev/blog/go1.13-errors) — matching the concrete error type out of an error chain.
- [Effective Go — Interfaces](https://go.dev/doc/effective_go#interfaces) — interface values and dynamic type.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-prefer-stdlib-io-interface.md](06-prefer-stdlib-io-interface.md) | Next: [08-single-caller-premature-abstraction.md](08-single-caller-premature-abstraction.md)
