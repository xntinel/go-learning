# Exercise 5: A Typed Log-Level Enum: iota, Defined Types, And Inference

A structured logger's `Level` is a textbook defined type: a small integer under the
hood, but a distinct type the compiler keeps separate from plain `int`. You will
build `type Level int8` with an `iota` const block, a `String` method, and
`ParseLevel`, and see why `l := Info` infers `Level` (not `int`) and why a `Level`
will not compare to an `int8` without a conversion.

## What you'll build

```text
loglevel/                   independent module: example.com/loglevel
  go.mod                    go 1.26
  level.go                  type Level int8; iota consts; String; ParseLevel
  cmd/
    demo/
      main.go               prints each level and a round-trip parse
  level_test.go             round-trip, unknown-level rejection, type pins
```

Files: `level.go`, `cmd/demo/main.go`, `level_test.go`.
Implement: `type Level int8`, `const (Debug Level = iota; Info; Warn; Error)`,
`func (l Level) String() string`, `func ParseLevel(s string) (Level, error)`.
Test: `ParseLevel(l.String()) == l` for every level, unknown-level rejection, a
`var _ Level = Info` pin.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/loglevel/cmd/demo
cd ~/go-exercises/loglevel
go mod init example.com/loglevel
go mod edit -go=1.26
```

## iota, the const block's type, and inference

`iota` is the constant generator: it is `0` in the first `ConstSpec` of a block and
increments by one per line. Declaring the first constant as `Debug Level = iota`
fixes the *type* of the whole block to `Level`, so `Info`, `Warn`, and `Error` are
all `Level` too, with values `1`, `2`, `3`. They are typed constants of type
`Level`, not untyped `int` constants.

That typing flows into inference. `l := Info` infers `Level`, because `Info` is a
`Level` constant — not `int`. This is what makes the enum safe: you cannot
accidentally pass a `Level` where an `int` is expected, or add two levels as if they
were plain numbers, without the compiler noticing. The flip side is the friction the
type is *supposed* to create: a `Level` (underlying `int8`) will not compare to a
plain `int8` value, and will not slot into an `int8` field, without an explicit
`int8(l)` conversion. Untyped constants remain the exception — `Info == 1` compiles
because `1` is untyped and conforms to `Level`, but `Info == int8(1)` does not,
because `int8(1)` is a *typed* value of a different type.

`String` gives the type a human name for logs and satisfies `fmt.Stringer`, so
`%s`/`%v` and `slog` render `"INFO"` instead of `1`. It handles an out-of-range
`Level` by formatting the underlying number with `strconv`, so a corrupt value is
visible rather than silently blank. `ParseLevel` is the inverse, uppercasing input
so `"info"`, `"INFO"`, and `"Info"` all resolve, and returning a wrapped sentinel
error for anything it does not recognize.

Create `level.go`:

```go
package loglevel

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Level is a defined type over int8: distinct from int8 in the type system, so
// it cannot mix with plain integers without an explicit conversion.
type Level int8

// The iota block fixes the type of every constant to Level. Debug is 0, Info 1,
// Warn 2, Error 3 -- all typed as Level, not untyped int.
const (
	Debug Level = iota
	Info
	Warn
	Error
)

// ErrUnknownLevel is returned by ParseLevel for an unrecognized name.
var ErrUnknownLevel = errors.New("unknown log level")

// String satisfies fmt.Stringer. An out-of-range Level is rendered with its
// numeric value so a corrupt level is visible, not blank.
func (l Level) String() string {
	switch l {
	case Debug:
		return "DEBUG"
	case Info:
		return "INFO"
	case Warn:
		return "WARN"
	case Error:
		return "ERROR"
	default:
		return "Level(" + strconv.Itoa(int(l)) + ")"
	}
}

// ParseLevel maps a case-insensitive name to a Level, wrapping ErrUnknownLevel
// for anything unrecognized.
func ParseLevel(s string) (Level, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return Debug, nil
	case "INFO":
		return Info, nil
	case "WARN":
		return Warn, nil
	case "ERROR":
		return Error, nil
	default:
		return Debug, fmt.Errorf("%w: %q", ErrUnknownLevel, s)
	}
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/loglevel"
)

func main() {
	for _, l := range []loglevel.Level{
		loglevel.Debug, loglevel.Info, loglevel.Warn, loglevel.Error,
	} {
		fmt.Printf("%d -> %s\n", int(l), l)
	}

	parsed, err := loglevel.ParseLevel("warn")
	if err != nil {
		panic(err)
	}
	fmt.Printf("ParseLevel(\"warn\") = %s\n", parsed)

	if _, err := loglevel.ParseLevel("trace"); err != nil {
		fmt.Printf("ParseLevel(\"trace\") error: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
0 -> DEBUG
1 -> INFO
2 -> WARN
3 -> ERROR
ParseLevel("warn") = WARN
ParseLevel("trace") error: unknown log level: "trace"
```

## Tests

The round-trip test proves `ParseLevel(l.String())` returns the same `Level` for
every defined level. `var _ Level = Info` pins the inferred type. The comparison
comment documents the compile-time contract that a `Level` and a plain `int8` do
not mix.

Create `level_test.go`:

```go
package loglevel

import (
	"errors"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	for _, l := range []Level{Debug, Info, Warn, Error} {
		got, err := ParseLevel(l.String())
		if err != nil {
			t.Fatalf("ParseLevel(%q) error: %v", l.String(), err)
		}
		if got != l {
			t.Fatalf("ParseLevel(%q) = %v, want %v", l.String(), got, l)
		}
	}
}

func TestInferredTypeIsLevel(t *testing.T) {
	t.Parallel()

	l := Info          // inferred type is Level, not int
	var _ Level = l    // compile-time pin
	var _ Level = Info // the constant is a Level too

	// Level and int8 are distinct types. The following would NOT compile,
	// which is the contract the defined type provides:
	//
	//	var x int8 = 1
	//	if l == x { ... } // invalid: mismatched types Level and int8
	//
	// Crossing the boundary requires an explicit conversion:
	if int8(l) != 1 {
		t.Fatalf("int8(Info) = %d, want 1", int8(l))
	}
}

func TestUnknownLevelRejected(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"trace", "", "verbose", "12"} {
		_, err := ParseLevel(s)
		if !errors.Is(err, ErrUnknownLevel) {
			t.Fatalf("ParseLevel(%q) err = %v, want ErrUnknownLevel", s, err)
		}
	}
}

func TestStringOutOfRange(t *testing.T) {
	t.Parallel()

	if got := Level(9).String(); got != "Level(9)" {
		t.Fatalf("Level(9).String() = %q, want Level(9)", got)
	}
}

func TestParseLevelCaseInsensitive(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"info", "INFO", "Info", " info "} {
		got, err := ParseLevel(s)
		if err != nil || got != Info {
			t.Fatalf("ParseLevel(%q) = %v, %v; want Info, nil", s, got, err)
		}
	}
}
```

## Review

The enum is correct when `iota` gives the four levels values `0..3`, when every
level round-trips through `String`/`ParseLevel`, and when an out-of-range `Level`
still renders visibly. The type-system payoff is what the commented block in
`TestInferredTypeIsLevel` documents: `l := Info` is a `Level`, and the compiler
refuses to compare or mix it with a plain `int8` — so a stray integer can never
masquerade as a level. When you genuinely need the underlying number (a metrics
label, a wire encoding), convert explicitly with `int8(l)`; that conversion is the
one visible seam where the defined type touches raw integers.

## Resources

- [Go Specification: Iota](https://go.dev/ref/spec#Iota) — how `iota` and the const block assign values and types.
- [Effective Go: The blank identifier and constants](https://go.dev/doc/effective_go#constants) — iota-based enumerations.
- [fmt.Stringer](https://pkg.go.dev/fmt#Stringer) — the interface `String` satisfies for `%s`/`%v`.

---

Back to [04-byte-size-constants-overflow.md](04-byte-size-constants-overflow.md) | Next: [06-rps-rate-metrics-truncation.md](06-rps-rate-metrics-truncation.md)
