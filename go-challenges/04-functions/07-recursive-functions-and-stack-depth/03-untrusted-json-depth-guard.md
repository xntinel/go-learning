# Exercise 3: Reject Deeply-Nested Untrusted JSON Before It Exhausts the Stack

A request-body guard that streams JSON tokens and tracks nesting depth, rejecting
a payload the instant it crosses a limit — without ever materializing the whole
structure. This is the standard middleware defense against nested-JSON denial of
service, where an attacker sends `[[[[[...]]]]]` a million deep to blow the stack
during unmarshal.

This module is fully self-contained: its own `go mod init`, the guard inline, its
own demo and tests.

## What you'll build

```text
jsondepth/                 independent module: example.com/jsondepth
  go.mod                   go 1.26
  jsondepth.go             ValidateDepth(io.Reader, maxDepth) error; ErrTooDeep
  jsondepth_test.go        table tests, malformed input, exact-boundary fuzz-style
  cmd/
    demo/
      main.go              accept a shallow config, reject a deep array
```

- Files: `jsondepth.go`, `cmd/demo/main.go`, `jsondepth_test.go`.
- Implement: `ValidateDepth(r io.Reader, maxDepth int) error` that streams tokens with a `json.Decoder`, incrementing depth on `{`/`[` delimiters and decrementing on `}`/`]`, returning the sentinel `ErrTooDeep` (wrapped) when depth exceeds `maxDepth`.
- Test: a flat object passing at `maxDepth=1`; a 10-level array failing at `maxDepth=5`; malformed JSON surfacing the decoder error; empty input accepted; a generated N-deep payload rejected at exactly `maxDepth+1`.
- Verify: `go test -count=1 -race ./...`

### Why you cannot measure first and reject later

The instinct is to `json.Unmarshal` into an `any` and then walk the result to
measure its depth. That is exactly wrong: the stack overflow or the memory blowup
happens *during* the unmarshal, before your measurement code ever runs. Go's
`encoding/json` decoder is itself recursive, so a payload nested past the goroutine
stack cap crashes the process inside `Unmarshal`. The guard has to work on the
token stream, so that a rejection reads only a constant prefix of the input.

`json.Decoder.Token` is the tool. It returns the input one token at a time: the
delimiters `{`, `}`, `[`, `]` (each a `json.Delim`, a distinct type), plus scalar
values (`string`, `float64`, `bool`, `nil`) and object keys. You never build the
tree; you only observe the shape as it streams past. Tracking depth is then a
counter: `{` and `[` increment it, `}` and `]` decrement it. The maximum value the
counter reaches is the nesting depth of the document, and you can reject the
instant it exceeds the limit — having materialized nothing but the current token.

Because objects and arrays open and close symmetrically, the counter is balanced:
every increment has a matching decrement in a well-formed document, so the depth
returns to zero at the end. A malformed document — an unclosed bracket, a stray
colon — surfaces as an error from `Token` itself, which the guard returns as-is so
the caller can distinguish "too deep" (a policy rejection) from "not valid JSON" (a
parse error). `io.EOF` from `Token` is the normal end of input and is not an error;
an empty body reaches EOF immediately at depth zero and is accepted.

The one detail to get right is what counts as depth. Here depth is the count of
currently-open containers. A flat object `{"a":1}` reaches depth 1 (one open
brace). An array of one object `[{"a":1}]` reaches depth 2. This matches the
intuitive "how many levels of nesting", and it means `maxDepth=1` admits any flat
object or array and rejects anything nested inside it.

Create `jsondepth.go`:

```go
package jsondepth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrTooDeep is returned when a JSON document nests deeper than the allowed limit.
var ErrTooDeep = errors.New("json nesting exceeds max depth")

// ValidateDepth streams tokens from r and returns ErrTooDeep if the JSON nests
// deeper than maxDepth open containers. It never materializes the document, so a
// deeply-nested adversarial payload is rejected after reading only a constant
// prefix. A malformed document returns the decoder's parse error unchanged.
func ValidateDepth(r io.Reader, maxDepth int) error {
	dec := json.NewDecoder(r)
	depth := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			continue
		}
		switch delim {
		case '{', '[':
			depth++
			if depth > maxDepth {
				return fmt.Errorf("depth %d exceeds limit %d: %w", depth, maxDepth, ErrTooDeep)
			}
		case '}', ']':
			depth--
		}
	}
}
```

### The runnable demo

The demo accepts a realistic shallow config object and rejects a deeply nested
array, showing the sentinel is detectable with `errors.Is`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"strings"

	"example.com/jsondepth"
)

func main() {
	config := `{"service":"api","limits":{"rps":100,"burst":150}}`
	err := jsondepth.ValidateDepth(strings.NewReader(config), 8)
	fmt.Printf("config accepted: %v\n", err == nil)

	deep := strings.Repeat("[", 12) + strings.Repeat("]", 12)
	err = jsondepth.ValidateDepth(strings.NewReader(deep), 5)
	fmt.Printf("deep rejected: %v\n", errors.Is(err, jsondepth.ErrTooDeep))
	fmt.Printf("reason: %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
config accepted: true
deep rejected: true
reason: depth 6 exceeds limit 5: json nesting exceeds max depth
```

### Tests

The table drives the core cases: a flat object at `maxDepth=1`, a nested object
that needs `maxDepth>=2`, a deep array rejected, malformed input, and empty input.
`TestMalformedSurfacesDecoderError` asserts a parse error is returned *and* is not
`ErrTooDeep`, so the caller can tell them apart. `TestRejectsAtExactBoundary` is
the fuzz-style check: for a range of depths it builds an N-deep array and asserts
the guard accepts at `maxDepth==N` and rejects at `maxDepth==N-1`, pinning the
boundary exactly.

Create `jsondepth_test.go`:

```go
package jsondepth

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func nestedArray(n int) string {
	return strings.Repeat("[", n) + strings.Repeat("]", n)
}

func TestValidateDepth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		maxDepth int
		wantErr  bool
	}{
		{"flat object at limit 1", `{"a":1}`, 1, false},
		{"flat array at limit 1", `[1,2,3]`, 1, false},
		{"nested object needs 2", `{"a":{"b":1}}`, 1, true},
		{"nested object ok at 2", `{"a":{"b":1}}`, 2, false},
		{"array of objects needs 2", `[{"a":1}]`, 1, true},
		{"deep array rejected", nestedArray(10), 5, true},
		{"deep array ok at 10", nestedArray(10), 10, false},
		{"empty input accepted", ``, 1, false},
		{"scalar accepted", `42`, 1, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateDepth(strings.NewReader(tt.input), tt.maxDepth)
			if tt.wantErr {
				if !errors.Is(err, ErrTooDeep) {
					t.Fatalf("err = %v, want ErrTooDeep", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected err = %v", err)
			}
		})
	}
}

func TestMalformedSurfacesDecoderError(t *testing.T) {
	t.Parallel()

	err := ValidateDepth(strings.NewReader(`{"a":}`), 10)
	if err == nil {
		t.Fatal("expected a decoder error for malformed JSON")
	}
	if errors.Is(err, ErrTooDeep) {
		t.Fatalf("malformed input misreported as too deep: %v", err)
	}
}

func TestRejectsAtExactBoundary(t *testing.T) {
	t.Parallel()

	for n := 1; n <= 20; n++ {
		payload := nestedArray(n)
		if err := ValidateDepth(strings.NewReader(payload), n); err != nil {
			t.Fatalf("depth %d should pass at maxDepth=%d, got %v", n, n, err)
		}
		if err := ValidateDepth(strings.NewReader(payload), n-1); !errors.Is(err, ErrTooDeep) {
			t.Fatalf("depth %d at maxDepth=%d: err = %v, want ErrTooDeep", n, n-1, err)
		}
	}
}

func Example() {
	err := ValidateDepth(strings.NewReader(`{"a":{"b":{"c":1}}}`), 2)
	fmt.Println(errors.Is(err, ErrTooDeep))
	// Output: true
}
```

## Review

The guard is correct when depth is measured as open-container count and the
rejection fires the instant that count exceeds the limit, having read only a
constant prefix — never after materializing the document.
`TestRejectsAtExactBoundary` is the honest proof: across twenty depths it pins the
accept/reject line at exactly `maxDepth`, which only holds if the counter is
symmetric and the check is on the increment. `TestMalformedSurfacesDecoderError`
guards the other failure mode: a parse error must be returned as itself, not
misclassified as too-deep, so middleware can respond `400 Bad Request` versus
`413`-style rejection appropriately. The mistake this exercise exists to prevent is
`json.Unmarshal`-then-measure: the crash happens inside the unmarshal, so the only
real defense is to stream tokens and bound depth before the structure exists.

## Resources

- [encoding/json: Decoder.Token](https://pkg.go.dev/encoding/json#Decoder.Token)
- [encoding/json: the Delim type](https://pkg.go.dev/encoding/json#Delim)
- [encoding/json: Decoder.More](https://pkg.go.dev/encoding/json#Decoder.More)
- [errors package (errors.Is)](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-depth-bounded-iterative-walker.md](02-depth-bounded-iterative-walker.md) | Next: [04-recursive-descent-filter-parser.md](04-recursive-descent-filter-parser.md)
