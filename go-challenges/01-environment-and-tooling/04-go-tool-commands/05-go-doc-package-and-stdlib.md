# Exercise 5: go doc — Reading the Package Graph and the Stdlib

`go doc` prints the documentation comments attached to declarations, resolved
through the package graph rather than by opening files. This module writes a
package with real doc comments and queries it every way `go doc` allows —
package, symbol, source, unexported — then reads the standard library with no
module present and serves browsable docs with the Go 1.25 `-http` flag.

## What you'll build

```text
doc-tour/                      module example.com/doc-tour
  go.mod
  internal/
    circle/
      circle.go                documented Area, ErrNegativeRadius, unexported tau
      circle_test.go           minimal test so the module gates
  cmd/
    demo/
      main.go                  prints Area(5)
```

- Files: `internal/circle/circle.go`, `internal/circle/circle_test.go`, `cmd/demo/main.go`.
- Implement: a documented `circle` package with an exported function, an exported sentinel, and an unexported constant.
- Test: a minimal `TestArea` so the module builds and gates.
- Verify: `go doc ./internal/circle` renders the package header and comments; `go doc fmt.Println` works with no module.

### Docs live in comments, not a separate file

The package comment — the comment immediately above `package circle` with no
blank line between them — becomes the package overview. Every exported
declaration's preceding comment becomes that symbol's documentation. There is no
separate docs artifact to keep in sync, which is the point: `go doc` reads what
travels with the code, so the documentation is exactly as current as the binary
you ship. Start each comment with the name it documents ("Area returns…",
"ErrNegativeRadius is returned…") — that convention is what `go doc` and pkgsite
render as the summary line.

The flags map onto how much you want to see. No flag, a package pattern: the
overview plus a one-line index of exported symbols. One argument
`package.Symbol`: that symbol's signature and comment. `-src`: the full source of
the declaration. `-u`: include unexported identifiers (the "u" is for
unexported), useful when reading a dependency's internals. `-all`: every
declaration expanded rather than indexed.

Create `internal/circle/circle.go`:

```go
// Package circle computes properties of circles. It exists to demonstrate how
// go doc renders package, symbol, and unexported documentation.
package circle

import (
	"errors"
	"math"
)

// ErrNegativeRadius is returned when the radius is negative. Zero is allowed;
// a zero-radius circle has area zero.
var ErrNegativeRadius = errors.New("radius must not be negative")

// Area returns the area of a circle with the given radius. It returns
// ErrNegativeRadius when radius is below zero.
func Area(radius float64) (float64, error) {
	if radius < 0 {
		return 0, ErrNegativeRadius
	}
	return math.Pi * radius * radius, nil
}

// tau is an unexported helper only visible with go doc -u.
const tau = 2 * math.Pi
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/doc-tour/internal/circle"
)

func main() {
	a, _ := circle.Area(5)
	fmt.Printf("%.5f\n", a)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
78.53982
```

Create `internal/circle/circle_test.go`:

```go
package circle

import (
	"errors"
	"math"
	"testing"
)

func TestArea(t *testing.T) {
	t.Parallel()
	got, err := Area(2)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if math.Abs(got-4*math.Pi) > 1e-9 {
		t.Fatalf("Area(2) = %v", got)
	}
	if _, err := Area(-1); !errors.Is(err, ErrNegativeRadius) {
		t.Fatalf("Area(-1) err = %v", err)
	}
}
```

### Querying your own package

The package overview and its exported index:

```bash
go doc ./internal/circle
```

```text
package circle // import "example.com/doc-tour/internal/circle"

Package circle computes properties of circles. It exists to demonstrate how go
doc renders package, symbol, and unexported documentation.

var ErrNegativeRadius = errors.New("radius must not be negative")
func Area(radius float64) (float64, error)
```

A single symbol, signature plus comment:

```bash
go doc ./internal/circle.Area
```

```text
package circle // import "example.com/doc-tour/internal/circle"

func Area(radius float64) (float64, error)
    Area returns the area of a circle with the given radius. It returns
    ErrNegativeRadius when radius is below zero.
```

The source of that declaration with `-src`:

```bash
go doc -src ./internal/circle.Area
```

```text
package circle // import "example.com/doc-tour/internal/circle"

// Area returns the area of a circle with the given radius. It returns
// ErrNegativeRadius when radius is below zero.
func Area(radius float64) (float64, error) {
	if radius < 0 {
		return 0, ErrNegativeRadius
	}
	return math.Pi * radius * radius, nil
}
```

The unexported `tau` appears only with `-u`:

```bash
go doc -u ./internal/circle
```

```text
package circle // import "example.com/doc-tour/internal/circle"

Package circle computes properties of circles. It exists to demonstrate how go
doc renders package, symbol, and unexported documentation.

const tau = 2 * math.Pi
var ErrNegativeRadius = errors.New("radius must not be negative")
func Area(radius float64) (float64, error)
```

### Reading the standard library with no module

`go doc` reads the stdlib straight from `$GOROOT`, so these work from any
directory, module or not:

```bash
go doc fmt.Println
```

```text
package fmt // import "fmt"

func Println(a ...any) (n int, err error)
    Println formats using the default formats for its operands and writes to
    ...
```

`go doc -all sync.WaitGroup` expands every method of a stdlib type the same way.

### Browsing locally with go doc -http (Go 1.25)

Go 1.25 added a built-in documentation server. It renders the same content as
pkgsite, in a browser, entirely offline:

```bash
go doc -http=:6060
```

This starts a server on `localhost:6060` and opens the requested object in a
browser. Point another terminal at it to confirm it is live, then stop it:

```bash
curl -sS localhost:6060/ | head -1
# stop the server with Ctrl-C, or kill the background job
```

It indexes your workspace and the stdlib, so you can browse
`example.com/doc-tour/internal/circle` and `sync.WaitGroup` side by side without
uploading anything.

## Review

The docs are correct when `go doc ./internal/circle | head -3` prints the package
header line `package circle // import "example.com/doc-tour/internal/circle"`
followed by the package comment, and `go doc ./internal/circle.Area` prints the
signature and its comment. The traps: forgetting the blank-line rule (a blank
line between the comment and `package circle` detaches it, and the overview goes
missing), and reaching for a wiki when `go doc -http` already serves browsable
HTML from the code itself. Because `go doc` reads comments from the graph, its
output is always as accurate as the source — there is nothing to regenerate.

## Resources

- [Command doc](https://pkg.go.dev/cmd/doc) — every `go doc` flag: `-all`, `-src`, `-u`, `-http`.
- [Go Doc Comments](https://go.dev/doc/comment) — the comment conventions `go doc` and pkgsite render.
- [Go 1.25 release notes](https://go.dev/doc/go1.25) — the new `go doc -http` documentation server.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-go-vet-catches-real-bugs.md](04-go-vet-catches-real-bugs.md) | Next: [06-gofmt-canonical-gate.md](06-gofmt-canonical-gate.md)
