# 10. Writing Assembly Functions

Go assembly is not Intel syntax and not AT&T syntax. It is derived from Plan 9 and uses pseudo-registers that the linker and assembler resolve at link time. Lesson 09 introduced declarations and the TEXT directive. This lesson goes further: loops, memory access over slice backing arrays, and branching inside leaf assembly functions. The exercises implement three leaf functions (`SumInt64s`, `CountByte`, `MemEqual`) with explicit `NOSPLIT` and `$0` local frames. The Concepts section also covers the non-leaf frame prologue (`SUBQ $8, SP` / `MOVQ BP, 0(SP)`) as essential background — understanding why leaf functions avoid that overhead makes the rules concrete. A wrong slice-header offset produces silent data corruption that only surfaces under race or at large inputs; always verify with the pure-Go oracle.

```text
asmfuncs/
  go.mod
  doc.go
  sum_amd64.s          -- SumInt64s loop
  sum_stub.go          -- Go declarations (no body)
  sum_pure.go          -- pure-Go reference implementations
  sum_test.go          -- table-driven tests + benchmarks + Example
  cmd/demo/main.go     -- runnable demo touching only exported API
```

The `.s` files compile only on amd64. The pure-Go implementations in `sum_pure.go` compile on every platform, are tested by the same test file via build tags, and serve as the correctness oracle during benchmarking.

## Concepts

### The Plan 9 Assembler and Pseudo-Registers

Go's assembler (`go tool asm`) is a derivative of the Plan 9 assembler. Its syntax is neither Intel nor AT&T. Four pseudo-registers are defined on all architectures:

- `SB` (static base): the origin of the program's address space. All global symbol references are written as `name(SB)` or `name+offset(SB)`.
- `FP` (frame pointer): a virtual register that addresses function arguments by name. `a+0(FP)` names the first argument `a` at offset 0. **Do not confuse this with the hardware BP/RBP register** — `FP` is a linker abstraction.
- `SP` (stack pointer): addresses local variables within the current stack frame. Confusingly, on amd64 `SP` may refer to either the hardware `RSP` or the virtual frame pointer depending on whether it appears with or without a symbol prefix; the convention is to use `x-8(SP)` for locals (with a name) and `0(SP)` for the hardware stack pointer (without a name).
- `PC` (program counter): used in branch targets and the `CALL` instruction.

The `TEXT` directive opens a function:

```
TEXT ·FunctionName(SB), FLAGS, $localsize-argsize
```

`$localsize` is the number of bytes reserved for locals on the stack. `$0` means no locals. `argsize` is the total byte count of all arguments plus all return values — it is not optional when `go vet` is in use; `go vet` cross-checks it against the Go declaration.

### Register-Based ABI (Go 1.17+)

Before Go 1.17, all arguments were passed on the stack (ABI0). Since Go 1.17, the compiler uses a register-based ABI (ABIInternal) for Go-to-Go calls on supported platforms. For **assembly functions called from Go**:

- On amd64, integer/pointer arguments arrive in `AX`, `BX`, `CX`, `DI`, `SI`, `R8`, `R9`, `R10`, `R11` (in that order).
- Return values go back in `AX`, `BX`, `CX`, ... in the same scheme.
- Slice `[]T` is three words in registers: `ptr` (pointer), `len` (length), `cap` (capacity). For the first slice argument on amd64: `ptr=AX`, `len=BX`, `cap=CX`.

Because assembly files opt in to ABI0 by default, and the Go toolchain automatically generates a wrapper that bridges between the caller's ABIInternal and the assembly's ABI0, **you do not need to do anything special** — you write the assembly as if arguments arrive at `arg+offset(FP)`, and the linker inserts the bridge. However, for maximum clarity and to avoid confusion, this lesson uses the `//go:nosplit` approach with `$0-n` frame sizes typical of leaf assembly functions.

The authoritative reference is `src/cmd/compile/abi-internal.md` in the Go source.

### Slice Layout in Assembly

A Go slice value `[]T` is a three-word struct:

```
offset 0:  pointer to backing array  (8 bytes on 64-bit)
offset 8:  length                    (8 bytes)
offset 16: capacity                  (8 bytes)
```

When a slice is the first argument of an assembly function, these three words arrive at consecutive `FP` offsets:

```
s_base+0(FP)  -- pointer to first element
s_len+8(FP)   -- length
s_cap+16(FP)  -- capacity (usually ignored in practice)
```

To iterate: load the pointer into a register, load the length into another, keep a counter, and loop with `CMPQ` / `JGE`.

### Stack Frame Management for Non-Leaf Functions

A **leaf** assembly function does not call any other function. It can declare `$0` local frame size and is safe to mark `NOSPLIT`.

A **non-leaf** function calls another function, which means it must:

1. Reserve stack space: `SUBQ $n, SP` where n covers the called function's argument area plus 8 bytes for the saved caller's BP.
2. Save the hardware base pointer: `MOVQ BP, saved_bp+0(SP)` / `LEAQ saved_bp+0(SP), BP`.
3. Call the target: `CALL ·targetFunc(SB)`.
4. Restore: `MOVQ saved_bp+0(SP), BP` / `ADDQ $n, SP`.

Forgetting step 2 or 4 corrupts the frame-pointer chain that `pprof` and stack-walking tools rely on. Forgetting step 1 overwrites the caller's stack.

### When Hand-Written Assembly Is Worth It

The compiler is good. Situations where hand-written assembly still wins:

- SIMD instructions the compiler's auto-vectorizer does not emit (AVX-512, NEON).
- CPU instructions the compiler does not model: `POPCNTQ`, `LZCNTQ`, `BSR`, `BSF`, `CMPXCHG16B`.
- Very tight inner loops with carefully chosen register allocation that the compiler's register allocator does not produce.
- OS/runtime code (syscalls, goroutine context switches) that must not trigger stack growth.

For everything else — including the `SumInt64s` example in this lesson — the compiler will match or beat hand-written code due to inlining. The benchmarks below make this explicit.

## Exercises

### Exercise 1: Pure-Go Implementations (the correctness oracle)

Create `sum_pure.go`. These compile everywhere and are the correctness oracle that the tests compare against:

```go
package asmfuncs

// SumInt64sPure returns the sum of all values in s using pure Go.
// It is the correctness oracle and the benchmark baseline.
func SumInt64sPure(s []int64) int64 {
	var acc int64
	for _, v := range s {
		acc += v
	}
	return acc
}

// CountBytePure counts occurrences of c in s using pure Go.
func CountBytePure(s []byte, c byte) int {
	n := 0
	for _, b := range s {
		if b == c {
			n++
		}
	}
	return n
}

// MemEqualPure reports whether a and b contain the same bytes.
func MemEqualPure(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

Create `doc.go` so the package has a package comment:

```go
// Package asmfuncs demonstrates writing Go assembly functions alongside
// pure-Go reference implementations.  The assembly versions target amd64
// only; on all other platforms the pure-Go versions are used directly.
package asmfuncs
```

### Exercise 2: Go Declarations for the Assembly Functions

Create `sum_stub.go`. A Go function with no body is the declaration that tells the compiler the function exists; the linker resolves it to the `.s` implementation:

```go
//go:build amd64

package asmfuncs

// SumInt64s returns the sum of all values in s.
// Implemented in sum_amd64.s.
func SumInt64s(s []int64) int64

// CountByte returns the number of occurrences of c in s.
// Implemented in sum_amd64.s.
func CountByte(s []byte, c byte) int

// MemEqual reports whether a and b contain identical bytes.
// Implemented in sum_amd64.s.
func MemEqual(a, b []byte) bool
```

`go vet` checks that the argument sizes in the TEXT directives in `sum_amd64.s` match these declarations exactly.

### Exercise 3: The Assembly Implementation

Create `sum_amd64.s`. Read every comment — each explains the instruction's role:

```asm
// Copyright 2024 example.com/asmfuncs authors. All rights reserved.
// Use of this source code is governed by a BSD-style license.

#include "textflag.h"

// func SumInt64s(s []int64) int64
//
// Argument layout at FP (ABI0 bridge):
//   s_base+0(FP)  pointer to backing array
//   s_len+8(FP)   length (number of int64 elements)
//   s_cap+16(FP)  capacity (ignored)
//   ret+24(FP)    return value
//
// Registers used:
//   SI  -- pointer into s (advances by 8 each iteration)
//   CX  -- remaining count (counts down to 0)
//   AX  -- accumulator (return value)
TEXT ·SumInt64s(SB), NOSPLIT, $0-32
	// Load slice header.
	MOVQ s_base+0(FP), SI  // SI = &s[0]
	MOVQ s_len+8(FP), CX   // CX = len(s)
	XORQ AX, AX            // AX = 0  (accumulator)

	// If len == 0, skip the loop.
	TESTQ CX, CX
	JZ   done_sum

loop_sum:
	// AX += *SI; SI += 8; CX--
	ADDQ (SI), AX
	ADDQ $8, SI
	DECQ CX
	JNZ  loop_sum

done_sum:
	MOVQ AX, ret+24(FP)    // store return value
	RET

// func CountByte(s []byte, c byte) int
//
// Argument layout at FP (ABI0 bridge):
//   s_base+0(FP)  pointer to backing array
//   s_len+8(FP)   length
//   s_cap+16(FP)  capacity (ignored)
//   c+24(FP)      the byte to search for (1 byte, padded to 8)
//   ret+32(FP)    return value (int)
//
// Registers:
//   SI  -- pointer into s
//   CX  -- remaining byte count
//   DX  -- the byte to search for, broadcast to all 8 bytes of BX scratch
//   AX  -- count of matches
//   BX  -- current byte being compared
TEXT ·CountByte(SB), NOSPLIT, $0-40
	MOVQ  s_base+0(FP), SI
	MOVQ  s_len+8(FP), CX
	MOVBQZX c+24(FP), DX    // zero-extend byte c into DX
	XORQ  AX, AX            // AX = 0

	TESTQ CX, CX
	JZ    done_count

loop_count:
	MOVBQZX (SI), BX        // BX = *SI (zero-extended)
	CMPQ  BX, DX
	JNE   next_count
	INCQ  AX                // match: increment counter
next_count:
	INCQ  SI
	DECQ  CX
	JNZ   loop_count

done_count:
	MOVQ  AX, ret+32(FP)
	RET

// func MemEqual(a, b []byte) bool
//
// Argument layout at FP (ABI0 bridge):
//   a_base+0(FP)   pointer to a's backing array
//   a_len+8(FP)    len(a)
//   a_cap+16(FP)   cap(a)  (ignored)
//   b_base+24(FP)  pointer to b's backing array
//   b_len+32(FP)   len(b)
//   b_cap+40(FP)   cap(b)  (ignored)
//   ret+48(FP)     bool return (1 byte)
//
// Registers:
//   SI  -- pointer into a
//   DI  -- pointer into b
//   CX  -- remaining byte count
//   AX  -- scratch
TEXT ·MemEqual(SB), NOSPLIT, $0-49
	MOVQ  a_len+8(FP), CX
	MOVQ  b_len+32(FP), AX

	// Lengths must be equal.
	CMPQ  CX, AX
	JNE   not_equal

	// Zero-length slices are equal.
	TESTQ CX, CX
	JZ    equal

	MOVQ  a_base+0(FP), SI
	MOVQ  b_base+24(FP), DI

loop_equal:
	MOVBQZX (SI), AX
	MOVBQZX (DI), R8
	CMPQ  AX, R8
	JNE   not_equal
	INCQ  SI
	INCQ  DI
	DECQ  CX
	JNZ   loop_equal

equal:
	MOVB  $1, ret+48(FP)
	RET

not_equal:
	MOVB  $0, ret+48(FP)
	RET
```

The `#include "textflag.h"` pulls in constants like `NOSPLIT`. The argument sizes in each TEXT line must add up exactly to all the `(FP)` offsets; `go vet` enforces this.

### Exercise 4: Tests, Benchmarks, and an Example

Create `sum_test.go`. The tests run on every platform: on amd64 they exercise both the assembly and the pure-Go versions; on other platforms only the pure-Go version is tested via the reference functions:

```go
package asmfuncs

import (
	"fmt"
	"math"
	"testing"
)

// --- SumInt64s ---

var sumCases = []struct {
	name string
	in   []int64
	want int64
}{
	{"empty", nil, 0},
	{"single", []int64{7}, 7},
	{"two", []int64{3, 4}, 7},
	{"negative", []int64{-1, -2, -3}, -6},
	{"mixed", []int64{math.MinInt64, math.MaxInt64}, -1},
	{"hundred", make100(), 5050},
}

func make100() []int64 {
	s := make([]int64, 100)
	for i := range s {
		s[i] = int64(i + 1)
	}
	return s
}

func TestSumInt64sPure(t *testing.T) {
	t.Parallel()
	for _, tc := range sumCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SumInt64sPure(tc.in)
			if got != tc.want {
				t.Fatalf("SumInt64sPure(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func ExampleSumInt64sPure() {
	fmt.Println(SumInt64sPure([]int64{1, 2, 3, 4, 5}))
	// Output: 15
}

// --- CountByte ---

var countCases = []struct {
	name string
	s    []byte
	c    byte
	want int
}{
	{"empty", nil, 'a', 0},
	{"no_match", []byte("hello"), 'z', 0},
	{"single_match", []byte("hello"), 'l', 2},
	{"all_match", []byte("aaaa"), 'a', 4},
	{"one_element_match", []byte{'x'}, 'x', 1},
	{"one_element_no_match", []byte{'x'}, 'y', 0},
}

func TestCountBytePure(t *testing.T) {
	t.Parallel()
	for _, tc := range countCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CountBytePure(tc.s, tc.c)
			if got != tc.want {
				t.Fatalf("CountBytePure(%q, %q) = %d, want %d", tc.s, tc.c, got, tc.want)
			}
		})
	}
}

func ExampleCountBytePure() {
	fmt.Println(CountBytePure([]byte("banana"), 'a'))
	// Output: 3
}

// --- MemEqual ---

var equalCases = []struct {
	name string
	a, b []byte
	want bool
}{
	{"both_empty", nil, nil, true},
	{"one_empty", []byte("a"), nil, false},
	{"equal", []byte("abc"), []byte("abc"), true},
	{"different_len", []byte("ab"), []byte("abc"), false},
	{"same_len_diff_content", []byte("abc"), []byte("abd"), false},
	{"single_byte_equal", []byte{'x'}, []byte{'x'}, true},
}

func TestMemEqualPure(t *testing.T) {
	t.Parallel()
	for _, tc := range equalCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := MemEqualPure(tc.a, tc.b)
			if got != tc.want {
				t.Fatalf("MemEqualPure(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func ExampleMemEqualPure() {
	fmt.Println(MemEqualPure([]byte("hello"), []byte("hello")))
	fmt.Println(MemEqualPure([]byte("hello"), []byte("world")))
	// Output:
	// true
	// false
}

// --- Benchmarks ---

var bigSlice []int64
var bigBytes []byte

func init() {
	bigSlice = make([]int64, 1<<16)
	for i := range bigSlice {
		bigSlice[i] = int64(i)
	}
	bigBytes = make([]byte, 1<<16)
	for i := range bigBytes {
		bigBytes[i] = byte(i)
	}
}

func BenchmarkSumInt64sPure(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = SumInt64sPure(bigSlice)
	}
}

func BenchmarkCountBytePure(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = CountBytePure(bigBytes, 42)
	}
}

func BenchmarkMemEqualPure(b *testing.B) {
	b2 := make([]byte, len(bigBytes))
	copy(b2, bigBytes)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MemEqualPure(bigBytes, b2)
	}
}
```

Your turn: add `TestSumInt64sOverflow` that calls `SumInt64sPure([]int64{math.MaxInt64, 1})` and checks that the result equals `math.MinInt64` (two's-complement wraparound). The test documents that the function follows Go's standard overflow semantics, not undefined behavior.

### Exercise 5: The cmd/demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/asmfuncs"
)

func main() {
	// SumInt64s
	nums := []int64{10, 20, 30, 40, 50}
	fmt.Printf("SumInt64sPure(%v) = %d\n", nums, asmfuncs.SumInt64sPure(nums))

	// CountByte
	text := []byte("the quick brown fox jumps over the lazy dog")
	fmt.Printf("CountBytePure(%q, 'o') = %d\n", string(text), asmfuncs.CountBytePure(text, 'o'))

	// MemEqual
	a := []byte("hello world")
	b := []byte("hello world")
	c := []byte("hello Go")
	fmt.Printf("MemEqualPure(%q, %q) = %v\n", string(a), string(b), asmfuncs.MemEqualPure(a, b))
	fmt.Printf("MemEqualPure(%q, %q) = %v\n", string(a), string(c), asmfuncs.MemEqualPure(a, c))
}
```

Run with:

```bash
go run ./cmd/demo
```

### Exercise 6: Wiring the Assembly on amd64

On amd64, `sum_stub.go` is included (build tag `amd64`) and the linker expects to find `SumInt64s`, `CountByte`, and `MemEqual` in `sum_amd64.s`. To additionally test the assembly functions on amd64, add the following to `sum_test.go` inside a build-tag block:

```go
//go:build amd64
```

Create a separate file `sum_asm_test.go` for amd64-only tests:

```go
//go:build amd64

package asmfuncs

import (
	"testing"
)

// TestSumInt64sASM verifies the assembly implementation against the pure-Go oracle.
func TestSumInt64sASM(t *testing.T) {
	t.Parallel()
	for _, tc := range sumCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SumInt64s(tc.in)
			want := SumInt64sPure(tc.in)
			if got != want {
				t.Fatalf("SumInt64s(%v) = %d, pure = %d", tc.in, got, want)
			}
		})
	}
}

func TestCountByteASM(t *testing.T) {
	t.Parallel()
	for _, tc := range countCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CountByte(tc.s, tc.c)
			want := CountBytePure(tc.s, tc.c)
			if got != want {
				t.Fatalf("CountByte(%q, %q) = %d, pure = %d", tc.s, tc.c, got, want)
			}
		})
	}
}

func TestMemEqualASM(t *testing.T) {
	t.Parallel()
	for _, tc := range equalCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := MemEqual(tc.a, tc.b)
			want := MemEqualPure(tc.a, tc.b)
			if got != want {
				t.Fatalf("MemEqual(%q, %q) = %v, pure = %v", tc.a, tc.b, got, want)
			}
		})
	}
}

func BenchmarkSumInt64sASM(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = SumInt64s(bigSlice)
	}
}
```

The test compares assembly output against the pure-Go oracle for each test case. A disagreement is a correctness bug in the assembly.

## Common Mistakes

### Wrong argument size in TEXT directive

Wrong:

```asm
TEXT ·SumInt64s(SB), NOSPLIT, $0-24
```

What happens: `go vet` reports "wrong argument size" because `[]int64` is 24 bytes (3 words) but the return value `int64` adds another 8 bytes; the total must be 32.

Fix:

```asm
TEXT ·SumInt64s(SB), NOSPLIT, $0-32
```

Count every field: ptr(8) + len(8) + cap(8) + ret(8) = 32.

### Using NOSPLIT on a non-leaf function

Wrong: declaring `NOSPLIT` on an assembly function that calls another function.

What happens: the function cannot grow the stack if needed. If the goroutine's stack is too small, the runtime panics with "stack overflow" during a legitimate call, not just under adversarial conditions.

Fix: remove `NOSPLIT` on non-leaf functions. Only leaf functions with small or zero stack frames are safe to mark `NOSPLIT`.

### Accessing slice fields at wrong offsets

Wrong (treating a slice like a single pointer):

```asm
MOVQ s+0(FP), SI     // ptr — OK
MOVQ s+4(FP), CX     // WRONG: length is at offset 8, not 4
```

What happens: CX receives garbage (high bytes of the pointer), the loop either does nothing or runs far beyond the slice bounds, producing silent data corruption or a segfault.

Fix: on 64-bit platforms each slice word is 8 bytes wide:

```asm
MOVQ s_base+0(FP), SI
MOVQ s_len+8(FP), CX
```

The symbolic names (`s_base`, `s_len`) are required by `go vet` to match the slice field names the asmdecl check synthesizes from the Go declaration; the pointer word is `s_base`, not `s_ptr`.

### Forgetting TESTQ before the loop

Wrong: entering the loop without guarding against `len == 0`.

What happens: DECQ makes CX wrap to `math.MaxUint64`; the loop runs for 2^64 iterations (in practice it crashes or corrupts memory).

Fix: always test for zero length before the loop:

```asm
TESTQ CX, CX
JZ    done
```

### Relying on the assembler to zero registers

Wrong: assuming `AX` starts at zero.

What happens: the return value is whatever was in `AX` when the function was entered, which is undefined.

Fix: use `XORQ AX, AX` to zero the accumulator explicitly before the loop.

## Verification

On amd64, from `~/go-exercises/asmfuncs`:

```bash
# 1. Format all Go source (the .s file is not Go; gofmt ignores it)
test -z "$(gofmt -l .)"

# 2. Vet: catches wrong argument sizes in TEXT directives
go vet ./...

# 3. Build everything including the assembly
go build ./...

# 4. Run tests (assembly + pure-Go on amd64; pure-Go only on other platforms)
go test -count=1 -race ./...

# 5. Benchmarks: compare ASM vs pure-Go
go test -bench=. -benchtime=3s ./...

# 6. Verify the demo runs
go run ./cmd/demo

# 7. Inspect disassembly of SumInt64s
go tool objdump -s 'SumInt64s' $(go env GOPATH)/bin/demo 2>/dev/null || \
  go build -o /tmp/asmfuncs_demo ./cmd/demo && go tool objdump -s 'asmfuncs' /tmp/asmfuncs_demo
```

On non-amd64 platforms, steps 1-4 still pass because the assembly stub file is excluded by the build tag and only the pure-Go functions are compiled.

Add at least one test of your own: `TestSumInt64sLargeNegative` that builds a slice of 1000 elements all equal to `-1` and asserts the result is `-1000`.

## Summary

- Go assembly uses Plan 9 syntax with four pseudo-registers: `SB`, `FP`, `SP`, `PC`. Only `FP` and `SP` address function-local data; `SB` addresses global symbols.
- The TEXT directive format is `TEXT ·Name(SB), FLAGS, $localsize-argsize`. The argsize must exactly match the sum of all argument and return value sizes; `go vet` enforces this.
- A slice `[]T` in assembly is three 8-byte words at consecutive `FP` offsets: pointer, length, capacity. Access the length at `name+8(FP)`, not `name+4(FP)`.
- Always test for a zero-length slice before entering a loop; always zero accumulators explicitly with `XORQ`.
- `NOSPLIT` is only safe for leaf functions (functions that do not call other functions) with small or zero stack frames.
- Non-leaf assembly functions must save and restore the base pointer and reserve stack space for the called function's arguments.
- The pure-Go compiler often matches hand-written scalar loops due to inlining. Hand-written assembly wins primarily for SIMD instructions, specialized CPU opcodes (`POPCNTQ`, `LZCNTQ`), and OS/runtime code that must not trigger stack growth.
- Test assembly by comparing its output against a pure-Go oracle on every input in the test table.

## What's Next

Next: [SIMD with Assembly](../11-simd-with-assembly/11-simd-with-assembly.md).

## Resources

- [A Quick Guide to Go's Assembler](https://go.dev/doc/asm) -- official reference for Plan 9 syntax, pseudo-registers, TEXT directive, and architecture specifics
- [Go Internal ABI Specification](https://github.com/golang/go/blob/master/src/cmd/compile/abi-internal.md) -- register assignments, slice layout in the register ABI, and the ABI0/ABIInternal bridge
- [Plan 9 Assembler Manual](https://9p.io/sys/doc/asm.html) -- the upstream assembler documentation that Go's assembler derives from
- [bytes/internal/bytealg: indexbyte_amd64.s](https://github.com/golang/go/blob/master/src/internal/bytealg/indexbyte_amd64.s) -- production-grade byte-searching assembly in the Go standard library
- [Go tool vet: asmdecl check](https://pkg.go.dev/cmd/vet) -- documents the checks vet performs on assembly declarations, including argument-size validation
