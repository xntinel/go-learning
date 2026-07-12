# Exercise 9: Parser — Multiple Suffix Examples Documenting Each Branch

A public parser has a behavior surface: it accepts valid input, rejects garbage,
and handles the empty edge. The canonical way to document all of it is one example
per scenario, each attached to the same function via a distinct lower-case suffix.
This exercise builds a duration parser and documents its valid, invalid, and empty
branches as separate labeled examples.

## What you'll build

```text
durparse/                   independent module: example.com/durparse
  go.mod                    go 1.26
  parse.go                  ErrEmpty, ErrInvalid; Parse(string) (time.Duration, error)
  cmd/
    demo/
      main.go               runnable demo parsing valid and invalid input
  parse_test.go             table-driven Test + ExampleParse, ExampleParse_valid, _invalid, _empty
```

Files: `parse.go`, `cmd/demo/main.go`, `parse_test.go`.
Implement: `Parse` wrapping `time.ParseDuration`, returning `ErrEmpty` (wrapped) for `""` and `ErrInvalid` (wrapped) for garbage.
Test: a table-driven `Test` asserting values and error identities, plus `ExampleParse`, `ExampleParse_valid`, `ExampleParse_invalid`, `ExampleParse_empty`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/15-testable-examples/09-multiple-scenario-suffix-examples/cmd/demo
cd go-solutions/12-testing-ecosystem/15-testable-examples/09-multiple-scenario-suffix-examples
```

## One symbol, several scenarios

`ExampleParse` (no further suffix) documents the function `Parse` and renders as
its primary example. Appending a lower-case, underscore-separated suffix —
`ExampleParse_valid`, `ExampleParse_invalid`, `ExampleParse_empty` — attaches
*additional* scenarios to the same `Parse` symbol, each rendered as its own
labeled example block in `go doc -all`. This is how you document a function's full
behavior surface without cramming every case into one example: the happy path, the
error path, and the edge each get a self-contained, runnable block.

The rule the toolchain enforces is that the suffix must start with a **lower-case**
letter. `ExampleParse_valid` is a scenario; `ExampleParse_Valid` (upper-case V) is
*not* treated as an example variant at all, because an upper-case letter after the
underscore reads as an exported identifier name (as if documenting a symbol
`Parse.Valid`), not a scenario label. So `ExampleParse_Valid` would be ignored as
a `Parse` example. Keep every scenario suffix lower-case.

`Parse` wraps `time.ParseDuration` and layers two sentinel errors so callers can
distinguish an empty input from a malformed one via `errors.Is`. The examples pin
both the parsed values (for valid input) and the exact error strings (for the
error and empty branches) as public, executed documentation.

Create `parse.go`:

```go
package durparse

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors distinguish the empty-input case from a malformed one.
var (
	ErrEmpty   = errors.New("empty input")
	ErrInvalid = errors.New("invalid duration")
)

// Parse converts a duration string such as "1m30s" into a time.Duration. It
// returns an error wrapping ErrEmpty for "" and ErrInvalid for malformed input.
func Parse(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("parse: %w", ErrEmpty)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, ErrInvalid)
	}
	return d, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/durparse"
)

func main() {
	if d, err := durparse.Parse("2h45m"); err == nil {
		fmt.Println("valid:", d)
	}
	if _, err := durparse.Parse("nope"); err != nil {
		fmt.Println("invalid:", err)
	}
	if _, err := durparse.Parse(""); err != nil {
		fmt.Println("empty:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid: 2h45m0s
invalid: parse "nope": invalid duration
empty: parse: empty input
```

### Tests and the four examples

The `Test` is table-driven over valid, invalid, and empty inputs, asserting parsed
values and error identity with `errors.Is`. The four examples document each
branch of `Parse`.

Create `parse_test.go`:

```go
package durparse

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr error
	}{
		{"valid compound", "2h45m", 2*time.Hour + 45*time.Minute, nil},
		{"valid millis", "500ms", 500 * time.Millisecond, nil},
		{"invalid", "nope", 0, ErrInvalid},
		{"empty", "", 0, ErrEmpty},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tt.in)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Parse(%q) err = %v, want Is %v", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) unexpected err: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("Parse(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func ExampleParse() {
	d, _ := Parse("2h45m")
	fmt.Println(d)
	// Output: 2h45m0s
}

func ExampleParse_valid() {
	d, _ := Parse("500ms")
	fmt.Println(d)
	// Output: 500ms
}

func ExampleParse_invalid() {
	_, err := Parse("nope")
	fmt.Println(err)
	// Output: parse "nope": invalid duration
}

func ExampleParse_empty() {
	_, err := Parse("")
	fmt.Println(err)
	// Output: parse: empty input
}
```

## Review

The four examples document `Parse`'s full behavior surface, and `go doc -all`
shows each suffix as its own labeled block under the function. The rule to
internalize is the lower-case suffix requirement: rename `ExampleParse_valid` to
`ExampleParse_Valid` and the toolchain no longer treats it as a `Parse` example
variant, because the upper-case letter reads as an identifier, not a scenario.
The `Test` and the examples agree on the contract — valid input returns the parsed
`Duration`, empty and malformed inputs wrap `ErrEmpty`/`ErrInvalid` — so a change
to either sentinel's message fails the matching example. Keep `gofmt -l` empty and
`go vet ./...` clean.

## Resources

- [testing package — Examples](https://pkg.go.dev/testing#hdr-Examples) — the lower-case suffix rule for scenario variants.
- [time.ParseDuration](https://pkg.go.dev/time#ParseDuration) — the parsing and the `Duration.String` format the examples pin.
- [errors.Is](https://pkg.go.dev/errors#Is) — distinguishing the two sentinel errors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-no-output-comment-compiled-not-run.md](08-no-output-comment-compiled-not-run.md) | Next: [10-example-with-setup-deterministic-output.md](10-example-with-setup-deterministic-output.md)
