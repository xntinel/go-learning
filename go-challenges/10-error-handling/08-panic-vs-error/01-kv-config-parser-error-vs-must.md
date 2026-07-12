# Exercise 1: KV Config Parser — Error for Input, Panic for Programmer Fault

Every service parses `key=value` configuration from somewhere: an env file, a CLI
flag, a `--set k=v` override. The same parse logic has two callers with opposite
contracts — one fed untrusted runtime input, one fed a constant the developer
typed — and that difference is exactly the panic-vs-error decision. This module
builds all three entry points and proves each one behaves.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
kvconfig/                    independent module: example.com/kvconfig
  go.mod                     go 1.26
  kvconfig.go                Pair; sentinels; Parse, MustParse, SafeParse
  cmd/
    demo/
      main.go                runnable demo: Parse ok, Parse err, SafeParse recovers
  kvconfig_test.go           table tests: each sentinel via errors.Is; panic asserts; recover-to-error
```

Files: `kvconfig.go`, `cmd/demo/main.go`, `kvconfig_test.go`.
Implement: `Parse` (returns wrapped sentinels `ErrEmpty`/`ErrInvalidKind`/`ErrInvalidNum`), `MustParse` (panics with contextual messages), `SafeParse` (recovers `MustParse` at the boundary via a named-return defer).
Test: `Parse` success and each sentinel via `errors.Is`; `MustParse` success and panic; `SafeParse` recovers to a non-nil error and succeeds on valid input; the pinned message-context test.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### One parse, two contracts

`Parse(input string) (Pair, error)` is the runtime entry point. Its caller is a
config loader reading a file or a flag — untrusted, expected to contain typos.
Empty input, a line with no `=`, a value that is not a number: all of these are
routine, so `Parse` returns a wrapped sentinel error and the loader decides
whether to skip the line, warn, or abort. Wrapping with `%w` is what lets the
loader classify the failure with `errors.Is(err, ErrInvalidNum)` without string
matching.

`MustParse(input string) Pair` is the init-time entry point. Its caller passes a
constant the developer wrote — `MustParse("max_conns=64")` next to the code. Here
a failure is not bad input, it is a *misbuilt program*: someone typed a bad
constant. Returning an error would force dead `if err != nil` branches that can
never legitimately fire on a constant. So `MustParse` panics, with a message that
names what was wrong ("invalid number ..."), and the program crashes at startup
instead of silently mis-parsing its own configuration.

`SafeParse(input string) (Pair, error)` exists for the case where you want the
convenience of the panicking form but at a boundary that must not crash — a plugin
that calls `MustParse` internally, say. It wraps `MustParse` in the canonical
recover-to-error shape: a deferred closure that calls `recover()` and, if a panic
was in flight, assigns the *named* return `err`. This is the bridge between the
two worlds: a panic on one side becomes an error on the other, and the only thing
that makes it work is that `err` is a named return the deferred closure can write.

Create `kvconfig.go`:

```go
package kvconfig

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Sentinel errors for the expected, caller-recoverable failures of Parse.
// Wrapped with %w so callers classify them with errors.Is.
var (
	ErrEmpty       = errors.New("empty input")
	ErrInvalidKind = errors.New("missing '=' separator")
	ErrInvalidNum  = errors.New("invalid number")
)

// Pair is a parsed key=value entry with an integer value.
type Pair struct {
	Key string
	Val int
}

// Parse is the runtime entry point: it is handed untrusted input (a config file
// line, a CLI flag) and returns a wrapped sentinel error for every expected
// failure. A correct caller reaches these at runtime with valid usage, so they
// are errors, never panics.
func Parse(input string) (Pair, error) {
	if input == "" {
		return Pair{}, fmt.Errorf("parse %q: %w", input, ErrEmpty)
	}
	parts := strings.SplitN(input, "=", 2)
	if len(parts) != 2 {
		return Pair{}, fmt.Errorf("parse %q: %w", input, ErrInvalidKind)
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return Pair{}, fmt.Errorf("parse %q: %w", input, ErrInvalidNum)
	}
	return Pair{Key: parts[0], Val: n}, nil
}

// MustParse is the init-time entry point: its caller passes a compile-time
// constant it controls, so a failure means the program is misbuilt. It panics
// with a contextual message rather than returning an error nobody could handle.
// Use it only for trusted, constant input.
func MustParse(input string) Pair {
	if input == "" {
		panic("kvconfig.MustParse: empty input")
	}
	parts := strings.SplitN(input, "=", 2)
	if len(parts) != 2 {
		panic(fmt.Sprintf("kvconfig.MustParse: missing '=' in %q", input))
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		panic(fmt.Sprintf("kvconfig.MustParse: invalid number %q", parts[1]))
	}
	return Pair{Key: parts[0], Val: n}
}

// SafeParse runs MustParse behind a recovery boundary: the deferred closure
// recovers any panic and assigns it to the named return err. It is the bridge
// from the panicking contract to the error-returning one.
func SafeParse(input string) (p Pair, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("kvconfig.SafeParse: %v", r)
		}
	}()
	p = MustParse(input)
	return p, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/kvconfig"
)

func main() {
	// Runtime input: Parse returns errors we classify with errors.Is.
	if p, err := kvconfig.Parse("max_conns=64"); err == nil {
		fmt.Printf("parsed: %s=%d\n", p.Key, p.Val)
	}
	if _, err := kvconfig.Parse("max_conns=lots"); errors.Is(err, kvconfig.ErrInvalidNum) {
		fmt.Println("rejected bad number as ErrInvalidNum")
	}

	// Trusted init input: MustParse would crash on a bad constant. Here it is good.
	def := kvconfig.MustParse("timeout=30")
	fmt.Printf("default: %s=%d\n", def.Key, def.Val)

	// SafeParse turns the MustParse panic into an error at the boundary.
	if _, err := kvconfig.SafeParse("broken"); err != nil {
		fmt.Println("SafeParse recovered:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
parsed: max_conns=64
rejected bad number as ErrInvalidNum
default: timeout=30
SafeParse recovered: kvconfig.SafeParse: kvconfig.MustParse: missing '=' in "broken"
```

### Tests

The tests assert each contract independently: `Parse` classification through
`errors.Is`, `MustParse` panicking (caught by a deferred `recover`), `SafeParse`
converting that panic to a non-nil error, and the pinned
`TestMustParsePanicMessageIncludesContext` that proves the panic value carries the
offending detail rather than a bare "invalid input".

Create `kvconfig_test.go`:

```go
package kvconfig

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    Pair
		wantErr error
	}{
		{"ok", "x=42", Pair{Key: "x", Val: 42}, nil},
		{"empty", "", Pair{}, ErrEmpty},
		{"no equals", "nokey", Pair{}, ErrInvalidKind},
		{"bad number", "x=notanumber", Pair{}, ErrInvalidNum},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tt.input)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Parse(%q) err = %v, want %v", tt.input, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) unexpected err: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("Parse(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestMustParseSuccess(t *testing.T) {
	t.Parallel()
	p := MustParse("x=42")
	if p != (Pair{Key: "x", Val: 42}) {
		t.Fatalf("MustParse = %+v, want {x 42}", p)
	}
}

func TestMustParsePanicsOnInvalid(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", "nokey", "x=bad"} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("MustParse(%q) did not panic", input)
				}
			}()
			MustParse(input)
		}()
	}
}

// TestMustParsePanicMessageIncludesContext pins the contract that the panic value
// names what was wrong, so a crash log is actionable.
func TestMustParsePanicMessageIncludesContext(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "invalid number") {
			t.Fatalf("panic message %q does not contain %q", msg, "invalid number")
		}
	}()
	MustParse("x=bad")
}

func TestSafeParse(t *testing.T) {
	t.Parallel()
	// Recovers a panic into a non-nil error.
	if _, err := SafeParse(""); err == nil {
		t.Fatal("SafeParse(\"\") err = nil, want non-nil")
	}
	// Succeeds on valid input with no error.
	p, err := SafeParse("x=42")
	if err != nil {
		t.Fatalf("SafeParse valid err = %v, want nil", err)
	}
	if p != (Pair{Key: "x", Val: 42}) {
		t.Fatalf("SafeParse = %+v, want {x 42}", p)
	}
}

func ExampleParse() {
	p, _ := Parse("port=8080")
	fmt.Printf("%s=%d\n", p.Key, p.Val)
	// Output: port=8080
}
```

## Review

The design is correct when each entry point matches its caller's contract. `Parse`
never panics on any string input — that is the whole point of the runtime path, and
the table test proves each expected failure surfaces as a classifiable sentinel via
`errors.Is`. `MustParse` panics on exactly the same failures because its caller is a
developer passing a constant, and a bad constant is a build defect that should crash
at startup, not limp along. `SafeParse` is the bridge, and it works only because
`err` is a *named* return the deferred `recover` closure can assign — drop the name
and the recovery has nowhere to put its result. The pinned message test guards
against the common regression of panicking with a generic string that gives on-call
nothing to act on. Run `go test -race` and `go vet ./...` to confirm.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the recover-to-error pattern with a named return.
- [Effective Go: Panic and Recover](https://go.dev/doc/effective_go#panic) — when Must-style panics are appropriate.
- [`errors.Is` and `%w` wrapping](https://pkg.go.dev/errors#Is) — sentinel classification through wrapped errors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-panic-recovery-http-middleware.md](02-panic-recovery-http-middleware.md)
