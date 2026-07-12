# Exercise 7: I/O Built-ins and the Injected Writer

The I/O built-ins — `print`, `readFile`, `writeFile` — are where the interpreter touches the outside world, and that makes them the ones a naive design leaves untestable. The fix is to inject the boundary: `print` writes to a package-level `io.Writer` that defaults to `os.Stdout` but can be swapped for a `bytes.Buffer` in a test, and the file built-ins run against a real temporary directory the test framework cleans up. The same discipline that made the collection built-ins pure makes the I/O built-ins assertable.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
object.go     the runtime Integer, String, Null, Error types
registry.go   Builtin, RegisterBuiltin, Dispatch, newError
io.go         Output writer, print, readFile, writeFile and registration
cmd/
  demo/
    main.go   print plus a writeFile/readFile round-trip in a temp dir
io_test.go    capture print into a buffer; round-trip and missing-file cases
```

- Files: `object.go`, `registry.go`, `io.go`, `cmd/demo/main.go`, `io_test.go`.
- Implement: the `Output` writer variable; `print`, `readFile`, `writeFile`; and the registry framework.
- Test: that `print` writes its space-separated arguments to a captured buffer, that a `writeFile`/`readFile` round-trip preserves content, and that `readFile` on a missing path returns an `*Error`.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

### Inject the boundary so the test can stand in for the world

`print` does not call `fmt.Println`. It writes to `Output`, a package-level `var Output io.Writer = os.Stdout`. In production that is standard output; in a test the line `Output = &buf` (restored afterward with `t.Cleanup`) redirects every `print` into a `bytes.Buffer` the test can assert on. This is the cheapest possible version of dependency injection — a single mutable package variable — and it turns an operation that would otherwise need a subprocess-capture harness into a three-line unit test. `print` joins its arguments' `Inspect()` forms with spaces and appends a newline, returning the `null` singleton because it is called for its effect, not its value.

`readFile` and `writeFile` go straight to `os.ReadFile` and `os.WriteFile`, translating any OS error into the language's `*Error` value rather than letting it escape as a Go `error`. They are tested against a real path under `t.TempDir()`, which the framework creates fresh per test and removes automatically — so the test exercises the actual syscalls without leaving artifacts behind. A `readFile` on a nonexistent path must return an `*Error`, not panic, which is the file-system analogue of the type guards elsewhere in the lesson.

Create `object.go`:

```go
package iobuiltins

import "fmt"

// ObjectType names the runtime kind of an Object.
type ObjectType string

const (
	INTEGER_OBJ ObjectType = "INTEGER"
	STRING_OBJ  ObjectType = "STRING"
	NULL_OBJ    ObjectType = "NULL"
	ERROR_OBJ   ObjectType = "ERROR"
)

// Object is the runtime value interface shared across the interpreter.
type Object interface {
	Type() ObjectType
	Inspect() string
}

// Integer holds an int64 value.
type Integer struct{ Value int64 }

func (i *Integer) Type() ObjectType { return INTEGER_OBJ }
func (i *Integer) Inspect() string  { return fmt.Sprintf("%d", i.Value) }

// String holds a Go string value.
type String struct{ Value string }

func (s *String) Type() ObjectType { return STRING_OBJ }
func (s *String) Inspect() string  { return s.Value }

// Null represents the absence of a value.
type Null struct{}

func (n *Null) Type() ObjectType { return NULL_OBJ }
func (n *Null) Inspect() string  { return "null" }

// Error is a runtime error value that propagates without panicking.
type Error struct{ Message string }

func (e *Error) Type() ObjectType { return ERROR_OBJ }
func (e *Error) Inspect() string  { return "ERROR: " + e.Message }

// NullVal is the shared null singleton returned by effectful built-ins.
var NullVal = &Null{}
```

### The registry framework

Create `registry.go`:

```go
package iobuiltins

import "fmt"

// BuiltinFunction is the signature every built-in satisfies.
type BuiltinFunction func(args ...Object) Object

// Builtin holds a function with its arity contract and documentation.
type Builtin struct {
	Name    string
	MinArgs int // -1 means no lower bound
	MaxArgs int // -1 means no upper bound (variadic)
	Doc     string
	Fn      BuiltinFunction
}

// BuiltinOption modifies a Builtin at registration time.
type BuiltinOption func(*Builtin)

// WithArity sets the inclusive [min, max] argument-count bounds.
func WithArity(min, max int) BuiltinOption {
	return func(b *Builtin) { b.MinArgs = min; b.MaxArgs = max }
}

// WithDoc attaches a one-line documentation string.
func WithDoc(doc string) BuiltinOption {
	return func(b *Builtin) { b.Doc = doc }
}

// Registry maps built-in names to their Builtin descriptors.
var Registry = make(map[string]*Builtin)

// RegisterBuiltin adds fn to the Registry under name.
func RegisterBuiltin(name string, fn BuiltinFunction, opts ...BuiltinOption) {
	b := &Builtin{Name: name, MinArgs: -1, MaxArgs: -1, Fn: fn}
	for _, opt := range opts {
		opt(b)
	}
	Registry[name] = b
}

// Lookup returns the Builtin for name, or nil if not registered.
func Lookup(name string) *Builtin { return Registry[name] }

// Dispatch validates arity, then calls the named built-in.
func Dispatch(name string, args ...Object) Object {
	b, ok := Registry[name]
	if !ok {
		return newError("undefined builtin: %q", name)
	}
	if err := checkArgs(b, args); err != nil {
		return err
	}
	return b.Fn(args...)
}

// checkArgs validates len(args) against b's arity contract.
func checkArgs(b *Builtin, args []Object) *Error {
	n := len(args)
	if b.MinArgs >= 0 && n < b.MinArgs {
		return newError("%s: want at least %d arg(s), got %d", b.Name, b.MinArgs, n)
	}
	if b.MaxArgs >= 0 && n > b.MaxArgs {
		return newError("%s: want at most %d arg(s), got %d", b.Name, b.MaxArgs, n)
	}
	return nil
}

func newError(format string, a ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, a...)}
}
```

### The I/O built-ins

Create `io.go`:

```go
package iobuiltins

import (
	"io"
	"os"
	"strings"
)

// Output is the writer used by print. Replace it in tests to capture output.
var Output io.Writer = os.Stdout

func init() {
	RegisterBuiltin("print", builtinPrint, WithArity(0, -1),
		WithDoc("print(args...) – print space-separated args with a newline"))
	RegisterBuiltin("readFile", builtinReadFile, WithArity(1, 1),
		WithDoc("readFile(path) – read file contents as a string"))
	RegisterBuiltin("writeFile", builtinWriteFile, WithArity(2, 2),
		WithDoc("writeFile(path, content) – write content to file"))
}

func builtinPrint(args ...Object) Object {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = a.Inspect()
	}
	_, _ = io.WriteString(Output, strings.Join(parts, " ")+"\n")
	return NullVal
}

func builtinReadFile(args ...Object) Object {
	path, ok := args[0].(*String)
	if !ok {
		return newError("readFile: arg 1: want STRING, got %s", args[0].Type())
	}
	data, err := os.ReadFile(path.Value)
	if err != nil {
		return newError("readFile: %s", err)
	}
	return &String{Value: string(data)}
}

func builtinWriteFile(args ...Object) Object {
	path, ok := args[0].(*String)
	if !ok {
		return newError("writeFile: arg 1: want STRING, got %s", args[0].Type())
	}
	content, ok := args[1].(*String)
	if !ok {
		return newError("writeFile: arg 2: want STRING, got %s", args[1].Type())
	}
	if err := os.WriteFile(path.Value, []byte(content.Value), 0o644); err != nil {
		return newError("writeFile: %s", err)
	}
	return NullVal
}
```

### The runnable demo

The demo prints to standard output through the built-in, then round-trips a string through a temporary file so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"example.com/iobuiltins"
)

func main() {
	iobuiltins.Dispatch("print", &iobuiltins.String{Value: "hello"}, &iobuiltins.Integer{Value: 42})

	dir, err := os.MkdirTemp("", "io-demo-*")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "note.txt")

	iobuiltins.Dispatch("writeFile", &iobuiltins.String{Value: path}, &iobuiltins.String{Value: "monkey"})
	got := iobuiltins.Dispatch("readFile", &iobuiltins.String{Value: path})
	fmt.Println("round-trip:", got.Inspect())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hello 42
round-trip: monkey
```

### Tests

`TestBuiltinPrintToWriter` is the one that demonstrates the injected writer: it swaps `Output` for a buffer, restores it with `t.Cleanup`, and asserts the captured text. The file tests run against `t.TempDir()`, so they exercise the real syscalls without leaving anything behind.

Create `io_test.go`:

```go
package iobuiltins

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryPopulated(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"print", "readFile", "writeFile"} {
		if Lookup(name) == nil {
			t.Errorf("built-in %q not registered", name)
		}
	}
}

func TestBuiltinPrintToWriter(t *testing.T) {
	var buf bytes.Buffer
	prev := Output
	Output = &buf
	t.Cleanup(func() { Output = prev })

	Dispatch("print", &String{Value: "hello"}, &Integer{Value: 42})
	if !strings.Contains(buf.String(), "hello") || !strings.Contains(buf.String(), "42") {
		t.Fatalf("print output = %q, want it to contain 'hello' and '42'", buf.String())
	}
}

func TestBuiltinReadWriteFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "builtin-test.txt")

	content := "line1\nline2\n"
	writeResult := Dispatch("writeFile", &String{Value: path}, &String{Value: content})
	if writeResult.Type() == ERROR_OBJ {
		t.Fatalf("writeFile error: %s", writeResult.Inspect())
	}

	readResult := Dispatch("readFile", &String{Value: path})
	if readResult.Type() == ERROR_OBJ {
		t.Fatalf("readFile error: %s", readResult.Inspect())
	}
	if readResult.(*String).Value != content {
		t.Fatalf("readFile = %q, want %q", readResult.(*String).Value, content)
	}
}

func TestBuiltinReadFileNotFound(t *testing.T) {
	t.Parallel()

	result := Dispatch("readFile", &String{Value: "/nonexistent/path/file.txt"})
	if result.Type() != ERROR_OBJ {
		t.Fatal("readFile on missing file should return error")
	}
}

// ExampleDispatch round-trips a string through a temporary file.
func ExampleDispatch() {
	dir, _ := os.MkdirTemp("", "io-example-*")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "x.txt")
	Dispatch("writeFile", &String{Value: path}, &String{Value: "monkey"})
	fmt.Println(Dispatch("readFile", &String{Value: path}).Inspect())
	// Output: monkey
}
```

## Review

The module is correct when `print` is capturable and the file operations fail gracefully. Confirm that `print` writes its space-separated arguments plus a newline to `Output`, that swapping `Output` for a buffer captures the text exactly, that a `writeFile` followed by `readFile` round-trips content byte for byte, and that `readFile` on a missing path returns an `*Error` rather than panicking. The print test must not call `t.Parallel()`, because it mutates the shared `Output` variable; making it parallel would race other tests that read `Output`.

Common mistakes for this feature. Calling `fmt.Println` directly inside `print` instead of writing to `Output` makes the built-in impossible to capture in a unit test. Forgetting to restore `Output` in `t.Cleanup` leaks the buffer into later tests. Letting an `os.ReadFile` error escape as a Go `error` instead of wrapping it in an `*Error` breaks the value-propagation contract the evaluator depends on. And writing files outside `t.TempDir()` leaves artifacts and can fail on a read-only working tree.

## Resources

- [`os.ReadFile` / `os.WriteFile`](https://pkg.go.dev/os#ReadFile) — the whole-file helpers, available since Go 1.16.
- [`io.Writer`](https://pkg.go.dev/io#Writer) — the one-method interface that makes `print` redirectable.
- [`testing.T.TempDir`](https://pkg.go.dev/testing#T.TempDir) — a per-test directory removed automatically when the test ends.

---

Back to [06-hash-builtins.md](06-hash-builtins.md)
