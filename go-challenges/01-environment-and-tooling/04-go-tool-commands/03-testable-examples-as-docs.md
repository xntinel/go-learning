# Exercise 3: Executable Documentation with Example Functions

An `Example` function with an `// Output:` comment is documentation the compiler
runs and the test suite verifies. This module writes three of them — an ordered
example, a negative-path example, and an unordered-output example over a map — so
the package's docs cannot drift from its behavior.

## What you'll build

```text
examples-doc/                  module example.com/examples-doc
  go.mod
  internal/
    circle/
      circle.go                Area(radius) (float64, error); ErrNegativeRadius
      example_test.go          ExampleArea, ExampleArea_negative, ExampleArea_table
  cmd/
    demo/
      main.go                  prints circle.Area for a couple of radii
```

- Files: `internal/circle/circle.go`, `internal/circle/example_test.go`, `cmd/demo/main.go`.
- Implement: the `circle.Area` library.
- Test: three example functions — one `// Output:`, one negative-path, one `// Unordered output:` over a map.
- Verify: `go run ./cmd/demo` prints the areas; `go test ./...` executes the examples and compares stdout; `go doc` renders them.

### How examples are checked and named

`go test` collects every `func ExampleXxx()` and runs it. If the function ends in
an `// Output:` comment, the harness captures its stdout and compares it, line for
line, against the comment; a mismatch fails the suite exactly like a failed
assertion. That is what makes an example *executable documentation*: the moment
`Area` changes so that `Area(5)` no longer prints `78.5398`, the example fails and
the docs are forced back into sync.

The naming convention doubles as placement in the rendered docs. `ExampleArea`
attaches to the `Area` function. A suffix after an underscore attaches a second
example to the same symbol and labels it: `ExampleArea_negative` renders as an
additional example titled "negative". This is why the Go 1.24 `tests` analyzer
(part of `go vet`) reports an `ExampleXxx` whose `Xxx` names no identifier in the
package — a rename that leaves `ExampleOldName` behind is a dangling doc, and vet
catches it.

When the output order is not deterministic — ranging a map, for instance — use
`// Unordered output:` instead of `// Output:`. The harness then sorts both the
produced lines and the expected lines before comparing, so a correct example does
not flake on map iteration order. Using plain `// Output:` there would fail
intermittently; using `// Unordered output:` states the intent precisely.

Create `internal/circle/circle.go`:

```go
package circle

import (
	"errors"
	"math"
)

// ErrNegativeRadius is returned when the radius is negative.
var ErrNegativeRadius = errors.New("radius must not be negative")

// Area returns the area of a circle with the given radius.
func Area(radius float64) (float64, error) {
	if radius < 0 {
		return 0, ErrNegativeRadius
	}
	return math.Pi * radius * radius, nil
}
```

### A demo alongside the examples

The example functions are compiled and run by `go test`, but they never run under
`go run`. A small `cmd/demo` gives the package the same runnable entry point as
every other module and lets you see `circle.Area` produce the values the examples
assert.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/examples-doc/internal/circle"
)

func main() {
	for _, r := range []float64{1, 2} {
		a, _ := circle.Area(r)
		fmt.Printf("Area(%.0f) = %.4f\n", r, a)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
Area(1) = 3.1416
Area(2) = 12.5664
```

Create `internal/circle/example_test.go`. It lives in the external test package
`circle_test`, so it exercises only the exported API — exactly what a reader of
the docs can call:

```go
package circle_test

import (
	"fmt"

	"example.com/examples-doc/internal/circle"
)

func ExampleArea() {
	a, err := circle.Area(5)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%.4f\n", a)
	// Output: 78.5398
}

func ExampleArea_negative() {
	_, err := circle.Area(-1)
	fmt.Println(err)
	// Output: radius must not be negative
}

func ExampleArea_table() {
	radii := map[string]float64{"unit": 1, "double": 2}
	for name, r := range radii {
		a, _ := circle.Area(r)
		fmt.Printf("%s=%.4f\n", name, a)
	}
	// Unordered output:
	// unit=3.1416
	// double=12.5664
}
```

Run the suite; the examples execute and their stdout is checked:

```bash
go test ./...
```

```text
ok  	example.com/examples-doc/internal/circle	0.245s
```

### Proving the check bites

Change the `ExampleArea` output line to a wrong value and the suite fails,
printing the produced output against the expected:

```text
--- FAIL: ExampleArea (0.00s)
got:
78.5398
want:
78.9999
FAIL
```

The example is not decoration; it is a test with a comment for an assertion.

### The tests analyzer catches a dangling example

An example named after a symbol that does not exist is a documentation bug that
compiles cleanly. The Go 1.24 `tests` analyzer, run as part of `go vet`, reports
it — an illustrative form of the diagnostic:

```text
# example.com/examples-doc/internal/circle
./example_test.go:NN: ExampleBogusSymbol refers to unknown identifier: BogusSymbol
```

Do not add such a function to the module; the point is that vet would refuse it.
The three real examples above all name existing identifiers, so `go vet ./...` is
silent.

## Review

The examples are correct when `go test ./...` passes with each example's stdout
matching its `// Output:` (or, for the map case, its `// Unordered output:`), and
when `go vet ./...` is silent. The trap the unordered case avoids is using plain
`// Output:` over a map and getting an intermittently failing test; the trap the
`tests` analyzer catches is a renamed symbol leaving a `ExampleOldName` behind.
Break one output line on purpose and watch the suite fail — that failure is the
guarantee that these docs can never silently drift from the code.

## Resources

- [testing package — examples](https://pkg.go.dev/testing#hdr-Examples) — naming rules, `// Output:`, and `// Unordered output:`.
- [Go blog — testable examples](https://go.dev/blog/examples) — how examples become both docs and tests.
- [Command vet](https://pkg.go.dev/cmd/vet) — the `tests` analyzer that flags malformed example declarations.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-table-driven-parallel-tests.md](02-table-driven-parallel-tests.md) | Next: [04-go-vet-catches-real-bugs.md](04-go-vet-catches-real-bugs.md)
