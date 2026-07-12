# 9. Go Assembly: Plan 9 Syntax

Go's assembler descends from Plan 9 and is unlike AT&T or Intel syntax. It runs on
every architecture the toolchain supports, using a set of pseudo-registers and
directives that abstract away hardware differences. Assembly appears in the Go
runtime (the scheduler, GC write barriers, system call stubs), in
`math/bits`, and in any hot path where the compiler cannot emit the
right instruction sequence on its own. This lesson teaches the mechanics you need
to read compiler output fluently and to write a small, correct assembly function.

```text
asmbasics/
  go.mod
  add.go          -- Go prototype (no body)
  add_amd64.s     -- implementation for amd64
  add_test.go     -- tests + Example
  cmd/demo/
    main.go       -- runnable demo: go run ./cmd/demo
```

The lesson targets amd64 (Linux/macOS/Windows). The concepts transfer directly to
arm64, but register names differ; see the ABI reference in Resources.

## Concepts

### The Plan 9 Lineage

Go's assembler is not a standalone tool; it is part of `go tool asm`. It accepts
Plan 9 assembly syntax, which differs from GNU AS in three ways that matter daily:

- Data flows **left to right**: `MOVQ AX, BX` copies AX into BX (opposite of
  Intel syntax, same direction as AT&T without the `%` sigils).
- Registers are written without prefixes: `AX`, `BX`, `CX`, not `%rax`.
- Size suffixes are part of the mnemonic, not the register: `MOVQ` (64-bit),
  `MOVL` (32-bit), `MOVW` (16-bit), `MOVB` (8-bit).

### Pseudo-Registers

The assembler defines four pseudo-registers that never map to a single hardware
register; the toolchain resolves them to real addresses at link time.

| Register | Meaning | Typical use |
|---|---|---|
| `FP` | frame pointer, base of caller's stack frame | addressing arguments and named returns |
| `SP` | virtual stack pointer, top of local frame | local variables (negative offsets) |
| `SB` | static base, start of the data segment | naming global symbols and functions |
| `PC` | program counter | conditional branches and jump targets |

`FP` is the canonical way to address function arguments in ABI0 (the old
stack-based calling convention). Every `FP`-relative reference must carry a
symbolic name: `a+0(FP)` is legal; `0(FP)` is rejected by `go vet`.

`SP` with a symbol name refers to the virtual (pseudo) stack pointer. Without a
symbol name, `SP` refers to the hardware stack register. Use the symbolic form
for local variables: `tmp-8(SP)`.

### The TEXT Directive

A TEXT directive opens a function body:

```asm
TEXT ·Add(SB), NOSPLIT, $0-24
```

Breaking this apart:

- `·` is a Unicode middle dot (U+00B7); it separates the package from the
  function name. In a file under package `asmbasics`, writing `·Add` names the
  function `asmbasics.Add`.
- `(SB)` anchors the symbol in the static data segment.
- `NOSPLIT` is a flag constant from `textflag.h` that suppresses the
  stack-growth preamble. Use it only for leaf functions (no calls to other Go
  functions) with small or zero local frames.
- `$0-24`: `$0` is the local frame size in bytes; `24` is the total size of
  arguments plus return values on the caller's stack (3 × 8 bytes on amd64).

The full list of TEXT flags is defined in
`src/cmd/internal/obj/textflag.go` and documented at go.dev/doc/asm.
The two you encounter most are `NOSPLIT` and `NOFRAME`.

### The Register-Based ABI (Go 1.17+)

Before Go 1.17 all arguments were passed on the stack (ABI0). From Go 1.17
onward, the compiler and runtime use ABIInternal: arguments and return values
are passed in registers when they fit.

On amd64, the integer argument registers in order are:
`AX`, `BX`, `CX`, `DI`, `SI`, `R8`, `R9`, `R10`, `R11`.

For a function `func Add(a, b int) int`, the compiler (when calling from Go)
places `a` in `AX`, `b` in `BX`, and expects the result back in `AX`. Hand-
written assembly that the compiler calls as ABIInternal must follow the same
convention.

However, hand-written assembly functions declared in `.s` files are called via
ABI0 wrappers by default. The linker generates a thin wrapper that shuffles
registers to/from the stack before calling your `.s` body. As a result, inside
your `.s` file you still use FP-relative offsets to read arguments, exactly as
you would in pre-1.17 code. This is the correct, supported way to write Go
assembly today.

The practical consequence: you do not need to know the ABIInternal register
order to write correct assembly functions. You read arguments from
`arg+0(FP)`, `arg+8(FP)`, etc., and write results to `ret+N(FP)`.

### Declaring the Go Prototype

Every function implemented in assembly needs a matching Go declaration with no
body in a `.go` file in the same package:

```go
// add.go
package asmbasics

// Add returns a + b. Implemented in add_amd64.s.
func Add(a, b int) int
```

The declaration provides the type information the compiler needs for type
checking and GC pointer maps. Without it, the function is invisible to Go code.

### Inspecting Compiler Output

Two tools reveal what the compiler does:

```bash
# Show the SSA + assembly output for a single file
go tool compile -S add.go

# Disassemble a compiled binary
go tool objdump -s 'asmbasics\.Add' ./binary
```

`go tool compile -S` is the fastest way to check whether an intrinsic or
optimization fired, to compare hand-written and compiler-generated code, and
to learn the mnemonic names for instructions you want to use.

## Exercises

Set up the module (run once):

```bash
mkdir -p go-solutions/36-runtime-compiler-and-assembly/09-go-assembly-basics/09-go-assembly-basics/cmd/demo
cd go-solutions/36-runtime-compiler-and-assembly/09-go-assembly-basics/09-go-assembly-basics
```

This is a library, not a program. Verification is `go test`.

### Exercise 1: The Go Prototype

Create `add.go`:

```go
package asmbasics

// Add returns a + b.
// The implementation is in add_amd64.s (ABI0 convention).
func Add(a, b int) int

// Abs returns the absolute value of x.
func Abs(x int) int

// Max returns the larger of a and b.
func Max(a, b int) int
```

These are declarations with no body. The compiler accepts them only when the
corresponding `.s` file exists in the same package directory at build time.

### Exercise 2: The Assembly File

Create `add_amd64.s`. The `_amd64` suffix restricts this file to amd64 builds;
the Go build system applies it automatically.

```asm
// Copyright 2024 example.com/asmbasics authors. All rights reserved.
// Implemented in Plan 9 assembly for amd64.

#include "textflag.h"

// func Add(a, b int) int
// ABI0: a at 0(FP), b at 8(FP), result at 16(FP).
TEXT ·Add(SB), NOSPLIT, $0-24
	MOVQ	a+0(FP), AX
	MOVQ	b+8(FP), BX
	ADDQ	BX, AX
	MOVQ	AX, ret+16(FP)
	RET

// func Abs(x int) int
// Returns |x| using arithmetic shift to construct a mask.
// If x >= 0: mask = 0, result = x.
// If x < 0:  mask = -1 (all ones), result = (x ^ mask) - mask = -x.
TEXT ·Abs(SB), NOSPLIT, $0-16
	MOVQ	x+0(FP), AX
	MOVQ	AX, BX
	SARQ	$63, BX        // arithmetic right shift: 0 or -1 (all ones)
	XORQ	BX, AX         // AX ^ mask
	SUBQ	BX, AX         // subtract mask: completes two's complement negation
	MOVQ	AX, ret+8(FP)
	RET

// func Max(a, b int) int
// Returns the larger of a and b using CMOVQGT (conditional move).
TEXT ·Max(SB), NOSPLIT, $0-24
	MOVQ	a+0(FP), AX
	MOVQ	b+8(FP), BX
	CMPQ	AX, BX
	CMOVQGT	AX, BX       // if AX > BX, move AX into BX
	MOVQ	BX, ret+16(FP)
	RET
```

Key points:

- `#include "textflag.h"` makes `NOSPLIT` and other flag constants available.
- `SARQ $63, BX` fills BX with the sign bit of the original value: 0 for
  non-negative, -1 (0xFFFFFFFFFFFFFFFF) for negative. XOR and subtract then
  conditionally negate the value without a branch.
- `CMOVQGT` is a conditional move: execute only when the previous comparison
  found the left operand greater than the right. Branchless code avoids
  branch-misprediction penalties on modern CPUs.

### Exercise 3: Tests and an Example

Create `add_test.go`:

```go
package asmbasics

import (
	"fmt"
	"math"
	"testing"
)

func TestAdd(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b, want int
	}{
		{0, 0, 0},
		{1, 2, 3},
		{-5, 5, 0},
		{-3, -4, -7},
		{math.MaxInt / 2, math.MaxInt / 2, math.MaxInt - 1},
	}
	for _, tc := range cases {
		got := Add(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("Add(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestAbs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		x, want int
	}{
		{0, 0},
		{1, 1},
		{-1, 1},
		{42, 42},
		{-42, 42},
		{math.MinInt + 1, math.MaxInt}, // -MaxInt -> MaxInt
	}
	for _, tc := range cases {
		got := Abs(tc.x)
		if got != tc.want {
			t.Errorf("Abs(%d) = %d, want %d", tc.x, got, tc.want)
		}
	}
}

func TestMax(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b, want int
	}{
		{0, 0, 0},
		{3, 7, 7},
		{7, 3, 7},
		{-1, 1, 1},
		{-5, -3, -3},
	}
	for _, tc := range cases {
		got := Max(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("Max(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// ExampleAdd is auto-verified by go test: the // Output: comment must match.
func ExampleAdd() {
	fmt.Println(Add(10, 32))
	// Output:
	// 42
}

// ExampleAbs demonstrates the branchless absolute value.
func ExampleAbs() {
	fmt.Println(Abs(-7))
	fmt.Println(Abs(7))
	// Output:
	// 7
	// 7
}

// BenchmarkAdd measures the overhead of the ABI0 wrapper call.
// A pure-Go add is inlined to zero cost; the assembly call has non-zero overhead
// because it crosses the ABI boundary.
func BenchmarkAdd(b *testing.B) {
	x, y := 3, 4
	var sink int
	for range b.N {
		sink = Add(x, y)
	}
	_ = sink
}
```

Your turn: add `TestAbsOfMinInt` that calls `Abs(math.MinInt)` and checks that
the result is `math.MinInt` (the expected overflow behavior for two's complement
absolute value of the minimum integer). This case is a known trap.

### Exercise 4: The Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/asmbasics"
)

func main() {
	pairs := [][2]int{{3, 4}, {-5, 5}, {0, 0}}
	for _, p := range pairs {
		a, b := p[0], p[1]
		fmt.Printf("Add(%2d, %2d) = %d\n", a, b, asmbasics.Add(a, b))
	}

	xs := []int{-9, 0, 7, -1}
	for _, x := range xs {
		fmt.Printf("Abs(%3d) = %d\n", x, asmbasics.Abs(x))
	}

	fmt.Printf("Max(3, 9) = %d\n", asmbasics.Max(3, 9))
	fmt.Printf("Max(9, 3) = %d\n", asmbasics.Max(9, 3))
}
```

Run it with:

```bash
go run ./cmd/demo
```

### Exercise 5: Comparing Compiler Output

After the module compiles, compare your hand-written `Add` to what the compiler
generates for a pure-Go equivalent:

```bash
# View assembly for the whole package
go tool compile -S add.go 2>/dev/null | grep -A 10 'asmbasics\.Add'

# Build the binary, then disassemble
go build -o /tmp/asmbasics ./cmd/demo
go tool objdump -s 'asmbasics\.Add' /tmp/asmbasics
```

You will see the ABI0 wrapper the linker generated alongside your TEXT body.
The wrapper shuffles registers to the stack before calling your function, then
restores them on return. This is why hand-written assembly function calls have
slightly more overhead than inlined Go: the wrapper is never inlined.

## Common Mistakes

### Missing the symbolic name on FP references

Wrong: `MOVQ 0(FP), AX` — the assembler rejects bare FP offsets.

What happens: `go vet` reports `use of unnamed argument 0(FP); offset 0 is
a+0(FP)` and the build fails.

Fix: always prefix FP references with a name: `MOVQ a+0(FP), AX`. The name is
arbitrary but must be present; it documents the argument and is checked by
`go vet` against the Go prototype.

### Omitting `#include "textflag.h"` and using a numeric flag

Wrong: `TEXT ·Add(SB), 4, $0-24` — the magic number 4 equals `NOSPLIT`, but it
is opaque and fragile.

What happens: the code works but is unreadable and will break if the constant
ever changes.

Fix: `#include "textflag.h"` at the top of every `.s` file, then use
`NOSPLIT`, `NOFRAME`, `DUPOK` by name.

### Calling a Go function from NOSPLIT assembly

Wrong: calling `runtime.convT64` or any other Go function from a `NOSPLIT`
function with a zero-byte frame.

What happens: the runtime's stack-growth check is skipped; if the goroutine's
stack is full, the call corrupts memory. The linker catches some of these cases
and reports "nosplit stack overflow".

Fix: `NOSPLIT` is only safe for true leaf functions (no calls, no local frame
that might exhaust the stack). If the function calls anything, remove `NOSPLIT`
and provide a real frame size, or use `go:nosplit` in a thin Go wrapper and
keep the heavy work in a non-NOSPLIT callee.

### Wrong argsize in the TEXT directive

Wrong: `TEXT ·Add(SB), NOSPLIT, $0-16` for `func Add(a, b int) int` — the
argsize 16 covers only two arguments but omits the return value slot.

What happens: `go vet` reports an argsize mismatch. On some architectures the
return value is written past the frame boundary, corrupting the caller.

Fix: argsize = sizeof(all args) + sizeof(all return values). For
`func Add(a, b int) int` on amd64: 8 + 8 + 8 = 24, so `$0-24`.

### Forgetting the Go prototype

Wrong: implementing `·Add` in the `.s` file but not declaring `func Add(a, b
int) int` in any `.go` file.

What happens: the package compiles, but `Add` is invisible to all callers
including the package's own tests. Any reference from Go code produces "undefined:
Add".

Fix: every assembly function must have a matching Go declaration in the same
package, with no function body.

## Verification

From `~/go-exercises/asmbasics`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass on amd64 (Linux, macOS, or Windows). On arm64 or other
architectures the `.s` file must be replaced or supplemented with an
`add_arm64.s` variant; the Go prototypes in `add.go` are architecture-neutral
and need no change.

To see the benchmark results:

```bash
go test -bench=. -benchtime=3s ./...
```

## Summary

- Go assembly uses Plan 9 syntax: left-to-right data flow, no register prefixes,
  size-suffix mnemonics (`MOVQ`, `ADDQ`).
- Four pseudo-registers: `FP` (arguments), `SP` (locals), `SB` (globals and
  function names), `PC` (branches).
- The TEXT directive opens a function: `TEXT ·Name(SB), flags, $framesize-argsize`.
- `NOSPLIT` suppresses the stack-growth check; use it only for leaf functions with
  small frames and no calls to other Go functions.
- Every assembly function needs a matching Go declaration (no body) in the same
  package so the compiler knows the type.
- The argsize field in TEXT must equal the sum of all argument sizes plus all
  return value sizes; `go vet` enforces this.
- Hand-written `.s` functions are called via an ABI0 wrapper the linker generates;
  read arguments from `arg+0(FP)`, write results to `ret+N(FP)`.
- `go tool compile -S` and `go tool objdump` are the primary tools for reading
  what the compiler and linker produce.

## What's Next

Next: [Writing Assembly Functions](../10-writing-assembly-functions/10-writing-assembly-functions.md).

## Resources

- [A Quick Guide to Go's Assembler](https://go.dev/doc/asm) -- the official
  reference for pseudo-registers, TEXT/DATA/GLOBL directives, and textflag
  constants; updated for each Go release.
- [Go Internal ABI Specification](https://github.com/golang/go/blob/master/src/cmd/compile/abi-internal.md)
  -- documents the register-based ABIInternal calling convention and the ABI0
  interoperability wrapper mechanism.
- [textflag.h constants](https://github.com/golang/go/blob/master/src/cmd/internal/obj/textflag.go)
  -- canonical source for NOSPLIT, NOFRAME, DUPOK, and the other TEXT flags.
- [Go runtime assembly: asm_amd64.s](https://github.com/golang/go/blob/master/src/runtime/asm_amd64.s)
  -- real-world examples of TEXT directives, NOSPLIT usage, and FP-relative
  argument addressing in the Go runtime itself.
- [Plan 9 Assembler Manual](https://9p.io/sys/doc/asm.html) -- historical
  reference for the syntax origin; useful for understanding why certain
  conventions exist.
