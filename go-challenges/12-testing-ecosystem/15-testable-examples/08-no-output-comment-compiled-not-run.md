# Exercise 8: Non-Deterministic Example — Compiled as Docs, Not Executed

Not every usage snippet can be output-matched. A request-ID generator produces a
different value every call, so you cannot pin its output. Go has a precise mode
for this: an example with *no* output comment is compiled but not executed, so it
documents usage and guards against API drift without asserting on a flaky value.
This exercise builds an ID generator and documents it that way.

## What you'll build

```text
idgen/                      independent module: example.com/idgen
  go.mod                    go 1.26
  idgen.go                  NewID() string (128-bit hex request id)
  cmd/
    demo/
      main.go               runnable demo printing deterministic properties of an id
  idgen_test.go             table-driven Test (properties) + ExampleNewID with NO output comment
```

Files: `idgen.go`, `cmd/demo/main.go`, `idgen_test.go`.
Implement: `NewID()` returning a 32-character hex string from 16 random bytes.
Test: a table-driven/property `Test` on length, hex-validity, and uniqueness, plus `ExampleNewID` deliberately without an `// Output:` comment.
Verify: `go test -count=1 -race ./...`

## Compiled, not executed — and why that is exactly right

An example that carries no output comment is still compiled by the test build, so
it must reference the current API and cannot rot — but it is never executed, so
nothing is asserted about what it prints. That is the correct tool for a snippet
whose output is genuinely non-deterministic: `NewID` returns fresh random bytes
each call, so there is no stable string to pin. Omitting the comment lets you
show a reader *how to call* `NewID` while sidestepping the impossible assertion.

The semantics are worth being exact about. Under `go test -v`, an example with an
output comment shows a `RUN`/`PASS` line; `ExampleNewID` here shows none, because
it is not run. Yet `go build ./...` and `go vet ./...` still compile it, so if
`NewID`'s signature changed — say it grew an `error` return — the example would
fail to compile and the build would break. That is the entire value: API-drift
protection without a flaky assertion. The trap to respect is the flip side:
because it does not execute, a panic or a logic error *inside* the example is
never caught at test time. If you added a bogus `// Output:` line, the example
would suddenly run and fail on the random value — which is precisely the flake the
omission avoids.

Create `idgen.go`:

```go
package idgen

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID returns a random 128-bit request id as a 32-character hex string.
// It is non-deterministic by design, which is why its example omits // Output:.
func NewID() string {
	b := make([]byte, 16)
	// crypto/rand.Read fills b entirely and does not fail in modern Go.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
```

### The runnable demo

The demo cannot print the id itself and stay reproducible, so it prints
deterministic *properties* of the id — its length and whether it is valid hex.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/hex"
	"fmt"

	"example.com/idgen"
)

func main() {
	id := idgen.NewID()
	_, err := hex.DecodeString(id)
	fmt.Println("length:", len(id))
	fmt.Println("valid hex:", err == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
length: 32
valid hex: true
```

### Tests and the no-output example

The `Test` asserts the deterministic properties the demo prints, plus uniqueness
across many calls — the right way to test a non-deterministic generator.
`ExampleNewID` carries no output comment, so it is compiled but not executed.

Create `idgen_test.go`:

```go
package idgen

import (
	"encoding/hex"
	"fmt"
	"testing"
)

func TestNewID(t *testing.T) {
	t.Parallel()

	t.Run("length and hex", func(t *testing.T) {
		t.Parallel()
		id := NewID()
		if len(id) != 32 {
			t.Errorf("len(NewID()) = %d, want 32", len(id))
		}
		if _, err := hex.DecodeString(id); err != nil {
			t.Errorf("NewID() = %q is not valid hex: %v", id, err)
		}
	})

	t.Run("uniqueness", func(t *testing.T) {
		t.Parallel()
		seen := make(map[string]bool, 1000)
		for range 1000 {
			id := NewID()
			if seen[id] {
				t.Fatalf("duplicate id generated: %q", id)
			}
			seen[id] = true
		}
	})
}

// ExampleNewID documents how to call NewID. It has no trailing output comment on
// purpose: NewID is non-deterministic, so this example is compiled (guarding
// against API drift) but never executed and asserts nothing.
func ExampleNewID() {
	id := NewID()
	fmt.Println(id)
}
```

## Review

`ExampleNewID` is correct precisely because it makes no output claim: with no
`// Output:` comment it compiles against `NewID`'s current signature — so a
breaking API change fails the build — yet never runs, so it cannot flake on the
random value. Confirm the semantics with `go test -v -run Example`: there is no
`RUN` line for `ExampleNewID`, while `go build ./...` still compiles it. The
determinism lives in the `Test`, which asserts length, hex-validity, and
uniqueness — the properties that *are* stable. The mistake to avoid is adding an
`// Output:` line "to be safe": it would make the example execute and fail on the
non-deterministic value. Keep `gofmt -l` empty and `go vet ./...` clean.

## Resources

- [testing package — Examples](https://pkg.go.dev/testing#hdr-Examples) — "Example functions without output comments are compiled but not executed."
- [crypto/rand](https://pkg.go.dev/crypto/rand) — `Read` and its guarantees.
- [The Go Blog: Testable Examples in Go](https://go.dev/blog/examples) — when to omit the output comment.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-whole-file-package-example.md](07-whole-file-package-example.md) | Next: [09-multiple-scenario-suffix-examples.md](09-multiple-scenario-suffix-examples.md)
