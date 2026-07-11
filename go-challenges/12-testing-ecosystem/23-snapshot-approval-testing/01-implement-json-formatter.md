# Exercise 1: Implement the JSON formatter under test

Every snapshot needs a deterministic producer. Before you can pin serialized
output to a golden file, you need output that is the same every run. This module
builds that producer — a `Format(User)` function that emits stable indented JSON
with a sentinel-guarded error path — and tests it with plain assertions. No
snapshot yet; the point is to establish the deterministic system-under-test that
every later module pins.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
jsonfmt/                   independent module: example.com/jsonfmt
  go.mod                   go 1.26
  format.go                User, ErrEmpty, Format(User) (string, error)
  cmd/
    demo/
      main.go              formats a sample user and prints the JSON
  format_test.go           happy-path shape + errors.Is(ErrEmpty) + Example
```

Files: `format.go`, `cmd/demo/main.go`, `format_test.go`.
Implement: `Format(User) (string, error)` producing indented JSON, rejecting an empty `ID` with `fmt.Errorf("format: %w", ErrEmpty)`.
Test: assert the happy-path JSON shape, assert `errors.Is(err, ErrEmpty)` on a missing ID, and an `Example` with `// Output:`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/jsonfmt/cmd/demo
cd ~/go-exercises/jsonfmt
go mod init example.com/jsonfmt
```

### Why determinism starts here

`json.MarshalIndent(v, "", "  ")` is the workhorse. It serializes a struct's
exported fields, in struct-declaration order, with two-space indentation. That
declaration-order guarantee is what makes the output snapshot-able: unlike a map,
a struct's field order is fixed at compile time, so the same `User` always
produces the same bytes. The JSON tags on `User` pin the wire names (`id`, `name`,
`email`) independent of the Go field names, which is exactly the contract a client
depends on — and exactly the thing a later snapshot test will catch if someone
renames a tag by accident.

The error path is a sentinel wrapped with `%w`. `ErrEmpty` is a package-level
`error` value; `Format` returns `fmt.Errorf("format: %w", ErrEmpty)` when `ID` is
blank, so a caller can match it with `errors.Is(err, ErrEmpty)` regardless of the
`"format: "` prefix. Wrapping with `%w` (not `%v`) is what preserves that match
through the wrap. Guarding the empty ID is a real validation concern: an empty
identifier is almost always a programming error upstream, and failing loudly here
beats emitting a record with a blank primary key.

`MarshalIndent` on a plain struct of strings cannot actually fail, but the
signature returns an error and idiomatic code checks it — a future change that
adds an unmarshalable field type (a channel, a function) would surface here rather
than panic.

Create `format.go`:

```go
package jsonfmt

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrEmpty is returned when a required identifier is missing. Callers match it
// with errors.Is regardless of any wrapping prefix.
var ErrEmpty = errors.New("empty value")

// User is the record whose serialized form later modules pin as a snapshot. The
// json tags are the wire contract; changing one is a contract change.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Format renders u as stable, indented JSON. A blank ID is rejected with a
// wrapped ErrEmpty so callers can match it with errors.Is.
func Format(u User) (string, error) {
	if u.ID == "" {
		return "", fmt.Errorf("format: %w", ErrEmpty)
	}
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return "", fmt.Errorf("format: marshal: %w", err)
	}
	return string(data), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/jsonfmt"
)

func main() {
	u := jsonfmt.User{ID: "u1", Name: "Alice", Email: "alice@example.com"}
	out, err := jsonfmt.Format(u)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "id": "u1",
  "name": "Alice",
  "email": "alice@example.com"
}
```

### Tests

`TestFormatShape` asserts the happy-path output exactly, so a regression in the
serialized shape is caught. `TestFormatRejectsEmptyID` asserts the sentinel path
with `errors.Is`, proving the wrap preserves the match. The `Example` doubles as a
documentation example and is verified by `go test` against its `// Output:` block.

Create `format_test.go`:

```go
package jsonfmt

import (
	"errors"
	"fmt"
	"testing"
)

func TestFormatShape(t *testing.T) {
	t.Parallel()

	got, err := Format(User{ID: "u1", Name: "Alice", Email: "alice@example.com"})
	if err != nil {
		t.Fatalf("Format returned error: %v", err)
	}

	want := `{
  "id": "u1",
  "name": "Alice",
  "email": "alice@example.com"
}`
	if got != want {
		t.Fatalf("Format shape mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatRejectsEmptyID(t *testing.T) {
	t.Parallel()

	_, err := Format(User{Name: "Alice"})
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want errors.Is(err, ErrEmpty)", err)
	}
}

func ExampleFormat() {
	out, _ := Format(User{ID: "u1", Name: "Alice", Email: "alice@example.com"})
	fmt.Println(out)
	// Output:
	// {
	//   "id": "u1",
	//   "name": "Alice",
	//   "email": "alice@example.com"
	// }
}
```

## Review

The formatter is correct when the output is a pure function of the input: the same
`User` always yields the same bytes, driven by `MarshalIndent`'s
struct-declaration order and the JSON tags. That determinism is the whole reason
the later snapshot modules can pin this output at all — if `Format` reached for a
map or a clock, its bytes would vary and no golden could hold. The error path is
correct when `errors.Is(err, ErrEmpty)` matches through the `%w` wrap; a common
slip is wrapping with `%v`, which flattens the error to a string and breaks the
match. Run `go test -race` to confirm both paths, and note that at this stage the
"snapshot" is just an inline literal in `TestFormatShape` — Exercise 2 makes the
snapshot framing explicit.

## Resources

- [encoding/json: MarshalIndent](https://pkg.go.dev/encoding/json#MarshalIndent) — the indented serializer and its struct-field-order guarantee.
- [errors: Is](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel error.
- [fmt: Errorf and the %w verb](https://pkg.go.dev/fmt#Errorf) — wrapping an error so `errors.Is` can unwrap it.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-inline-snapshot-test.md](02-inline-snapshot-test.md)
