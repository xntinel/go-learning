# 11. SIMD with Assembly

Go does not auto-vectorize. When a tight loop over a float32 slice runs on a modern CPU, the compiler emits scalar SSE2 instructions — one float per cycle — even though the hardware can process four (SSE2) or eight (AVX2) floats per instruction in the same cycle budget. Closing that gap requires hand-written Plan 9 assembly using the XMM or YMM register file. This lesson builds three SIMD-accelerated functions — element-wise vector addition, dot product, and maximum — each with a pure-Go fallback selected by build tag, a correctness test suite, and a benchmark that proves the speedup.

```text
simdvec/
  go.mod
  doc.go
  vec.go                          (pure-Go fallbacks, package API)
  vec_amd64.go                    (platform stubs that call asm functions)
  vec_amd64.s                     (SSE2 + AVX2 Plan 9 assembly)
  vec_other.go                    (stub for non-amd64 architectures)
  vec_test.go                     (correctness tests + benchmarks)
  cmd/demo/main.go                (runnable demo)
```

The package name is `simdvec`. Tests live in `package simdvec` (same-package access to unexported helpers). The demo lives in `cmd/demo` (`package main`) and touches only the exported API.

## Concepts

### How SIMD Works: One Instruction, Multiple Data

A scalar `ADDSS` instruction adds one pair of float32 values. `ADDPS` adds four pairs simultaneously using a 128-bit XMM register (`X0`–`X15`). `VADDPS` with 256-bit YMM registers (`Y0`–`Y15`) doubles the width again: eight float32 values per instruction. The hardware executes all lanes in the same number of clock cycles as a scalar operation, delivering 4x or 8x throughput on arithmetic-bound loops.

Go's compiler does not emit `VADDPS` or `VMULPS` for range loops. The loop body is always scalar unless you explicitly choose a SIMD instruction in assembly. This is the reason SIMD in Go must be hand-written.

### The XMM / YMM Register File

On amd64, registers X0–X15 are 128 bits wide (four float32 or two float64 lanes). Registers Y0–Y15 are 256 bits wide (eight float32 or four float64 lanes); the lower 128 bits of Yn alias Xn. In Plan 9 assembly the names are uppercase: `X0`, `Y0`.

Key instructions used in this lesson:

| Mnemonic   | Width | Operation                             |
|------------|-------|---------------------------------------|
| MOVUPS     | 128   | Unaligned load/store (scalar prefix)  |
| VMOVUPS    | 128   | Unaligned load/store (VEX prefix)     |
| VADDPS     | 256   | Add eight float32 lanes               |
| VMULPS     | 256   | Multiply eight float32 lanes          |
| VMAXPS     | 256   | Per-lane max of eight float32 lanes   |
| VHADDPS    | 256   | Horizontal pairwise add               |
| VEXTRACTF128 | 256 | Extract upper 128-bit lane            |
| VZEROALL   | --    | Zero all YMM registers (AVX/SSE transition) |

Always use `VMOVUPS` (unaligned) rather than `VMOVAPS` (aligned). Go's allocator aligns to pointer size (8 bytes), which does not satisfy the 32-byte alignment required for `VMOVAPS` on YMM registers. Using `VMOVAPS` on misaligned memory raises a general protection fault at runtime.

### CPU Feature Detection with GOAMD64 and Build Tags

Go 1.18 introduced the `GOAMD64` environment variable. Setting `GOAMD64=v3` at build time guarantees AVX, AVX2, BMI1, BMI2, FMA, and others are present. At the default `GOAMD64=v1`, only SSE2 is guaranteed. The corresponding build constraint is `//go:build amd64.v3`.

For runtime dispatch the standard library uses `internal/cpu`, which is not public. The idiomatic approach for user packages is:

1. Compile one implementation per feature level under separate build tags.
2. At `GOAMD64=v1` (baseline), use the SSE2 path.
3. At `GOAMD64=v3`, use the AVX2 path.

This lesson targets `GOAMD64=v1` (SSE2, four float32 per instruction) so the assembly runs on every amd64 machine without runtime checks. An AVX2 variant is shown in the extended exercises as a drop-in replacement.

### Slice Layout and the ABI0 Calling Convention

The `.go` declaration in `vec_amd64.go` uses pointer arguments (`*float32` plus an `int` count) rather than slice headers. The assembler file uses the **ABI0** (stack-based) calling convention: every argument and return value is passed at a named offset from the `FP` pseudo-register. ABI0 is the only convention available to `.s` files; the Go compiler emits a thin ABI wrapper automatically when Go callers invoke an assembly function.

For `addFloat32Asm(dst, src1, src2 *float32, n int)` the FP frame is:

```
dst+0(FP)    pointer  8 bytes
src1+8(FP)   pointer  8 bytes
src2+16(FP)  pointer  8 bytes
n+24(FP)     int      8 bytes
             total    32 bytes  → TEXT ...,NOSPLIT,$0-32
```

For functions that return a value, the return slot follows the arguments. `dotFloat32Asm(a, b *float32, n int) float32`:

```
a+0(FP)     pointer   8 bytes
b+8(FP)     pointer   8 bytes
n+16(FP)    int       8 bytes
ret+24(FP)  float32   4 bytes
            total     28 bytes  → TEXT ...,NOSPLIT,$0-28
```

The frame-size suffix (`-32`, `-28`, `-20`) must match exactly; `go vet` compares it against the declared signature and reports a mismatch. Always write the scalar return value to its FP slot before `RET`: `MOVSS X0, ret+24(FP)`.

### Tail Handling

A slice of n elements where n is not a multiple of the vector width W leaves a tail of n mod W elements. The SIMD loop processes floor(n/W) complete vectors; the tail must be handled by a scalar fallback loop. Skipping the tail is a correctness bug. The tests cover odd lengths explicitly.

### Register Preservation and the `VZEROALL` Idiom

Mixing legacy SSE instructions with VEX-encoded instructions (those starting with `V`) can cause a performance penalty on some Intel microarchitectures (the "SSE/AVX transition penalty"). Calling `VZEROALL` before returning from an AVX function ensures all YMM registers are zeroed and transitions are clean. This also prevents inadvertent leakage of data across function calls.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/simdvec/cmd/demo
cd ~/go-exercises/simdvec
go mod init example.com/simdvec
```

This is a library verified by `go test`. There is no `main` in the package itself.

### Exercise 1: Package API and Pure-Go Fallbacks

Create `doc.go` to establish the package documentation:

```go
// Package simdvec provides SIMD-accelerated float32 vector operations.
// On amd64 the hot paths use SSE2 instructions; the pure-Go fallbacks
// are always available and are used on other architectures.
package simdvec
```

Create `vec.go` with the exported API and pure-Go implementations:

```go
package simdvec

import "math"

// AddFloat32 adds src1 and src2 element-wise and writes the results to dst.
// All three slices must have the same length; if they do not, AddFloat32 panics.
func AddFloat32(dst, src1, src2 []float32) {
	mustSameLen3(dst, src1, src2)
	addFloat32(dst, src1, src2)
}

// DotFloat32 returns the dot product of a and b.
// Both slices must have the same length; if they do not, DotFloat32 panics.
func DotFloat32(a, b []float32) float32 {
	mustSameLen2(a, b)
	return dotFloat32(a, b)
}

// MaxFloat32 returns the maximum value in s, or -Inf if s is empty.
func MaxFloat32(s []float32) float32 {
	return maxFloat32(s)
}

// --- pure-Go fallback implementations used on non-amd64 targets
// and as reference implementations in tests.

func addFloat32Go(dst, src1, src2 []float32) {
	for i := range dst {
		dst[i] = src1[i] + src2[i]
	}
}

func dotFloat32Go(a, b []float32) float32 {
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

func maxFloat32Go(s []float32) float32 {
	if len(s) == 0 {
		return float32(math.Inf(-1))
	}
	m := s[0]
	for _, v := range s[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

// mustSameLen3 panics if the three slices do not have the same length.
func mustSameLen3(dst, a, b []float32) {
	if len(dst) != len(a) || len(a) != len(b) {
		panic("simdvec: slice length mismatch")
	}
}

// mustSameLen2 panics if the two slices do not have the same length.
func mustSameLen2(a, b []float32) {
	if len(a) != len(b) {
		panic("simdvec: slice length mismatch")
	}
}
```

`addFloat32`, `dotFloat32`, and `maxFloat32` are the per-architecture dispatch functions defined in the next two files.

### Exercise 2: amd64 Stubs and Assembly

Create `vec_other.go` for non-amd64 architectures. This file is selected when building on arm64, 386, or any other GOARCH:

```go
//go:build !amd64

package simdvec

import "math"

func addFloat32(dst, src1, src2 []float32) { addFloat32Go(dst, src1, src2) }
func dotFloat32(a, b []float32) float32    { return dotFloat32Go(a, b) }
func maxFloat32(s []float32) float32       { return maxFloat32Go(s) }

// Silence the unused import on non-amd64 builds.
var _ = math.Inf
```

Create `vec_amd64.go` — the Go-side declarations that the assembler satisfies:

```go
//go:build amd64

package simdvec

// addFloat32Asm adds src1 and src2 element-wise into dst using SSE2.
// n must equal len(dst) == len(src1) == len(src2).
//
//go:noescape
func addFloat32Asm(dst, src1, src2 *float32, n int)

// dotFloat32Asm returns the dot product of n elements of a and b using SSE2.
//
//go:noescape
func dotFloat32Asm(a, b *float32, n int) float32

// maxFloat32Asm returns the maximum of n elements of s using SSE2.
//
//go:noescape
func maxFloat32Asm(s *float32, n int) float32

func addFloat32(dst, src1, src2 []float32) {
	if len(dst) == 0 {
		return
	}
	addFloat32Asm(&dst[0], &src1[0], &src2[0], len(dst))
}

func dotFloat32(a, b []float32) float32 {
	if len(a) == 0 {
		return 0
	}
	return dotFloat32Asm(&a[0], &b[0], len(a))
}

func maxFloat32(s []float32) float32 {
	if len(s) == 0 {
		return maxFloat32Go(s) // returns -Inf
	}
	return maxFloat32Asm(&s[0], len(s))
}
```

The `//go:noescape` directive tells the compiler that the pointer arguments do not escape to the heap, enabling stack allocation of temporary slices in callers.

Now create `vec_amd64.s` — the Plan 9 assembly. This file contains three functions. Comments describe each instruction's effect:

```asm
// Copyright example.com/simdvec authors. All rights reserved.
// Use of this source code is governed by a BSD-style license.

#include "textflag.h"

// func addFloat32Asm(dst, src1, src2 *float32, n int)
//
// Argument layout via FP pseudo-register (ABI0 / stack-based):
//   dst+0(FP)   = dst pointer   (8 bytes)
//   src1+8(FP)  = src1 pointer  (8 bytes)
//   src2+16(FP) = src2 pointer  (8 bytes)
//   n+24(FP)    = n             (8 bytes)   total args = 32
//
// Strategy: process 4 float32s per iteration (128-bit SSE2 ADDPS).
// Tail: process 1 float32 at a time with ADDSS.
TEXT ·addFloat32Asm(SB),NOSPLIT,$0-32
	MOVQ dst+0(FP),  AX   // dst pointer
	MOVQ src1+8(FP), BX   // src1 pointer
	MOVQ src2+16(FP),CX   // src2 pointer
	MOVQ n+24(FP),   DI   // n (element count)

	// Vector loop: 4 float32s per iteration.
	MOVQ DI, SI
	SHRQ $2, SI           // SI = n / 4 (number of full vectors)
	JZ   tail_add

vec_loop_add:
	MOVUPS  (BX), X0      // load 4 floats from src1
	MOVUPS  (CX), X1      // load 4 floats from src2
	ADDPS   X1, X0        // X0[i] = X0[i] + X1[i]  (4 lanes)
	MOVUPS  X0, (AX)      // store 4 floats to dst
	ADDQ    $16, AX
	ADDQ    $16, BX
	ADDQ    $16, CX
	DECQ    SI
	JNZ     vec_loop_add

tail_add:
	ANDQ    $3, DI        // DI = n mod 4
	JZ      done_add

scalar_loop_add:
	MOVSS   (BX), X0
	MOVSS   (CX), X1
	ADDSS   X1, X0
	MOVSS   X0, (AX)
	ADDQ    $4, AX
	ADDQ    $4, BX
	ADDQ    $4, CX
	DECQ    DI
	JNZ     scalar_loop_add

done_add:
	RET

// func dotFloat32Asm(a, b *float32, n int) float32
//
// Argument layout via FP pseudo-register (ABI0 / stack-based):
//   a+0(FP)    = a pointer  (8 bytes)
//   b+8(FP)    = b pointer  (8 bytes)
//   n+16(FP)   = n          (8 bytes)
//   ret+24(FP) = return     (4 bytes, padded to 4)  total args+ret = 28
//
// Strategy: accumulate products into an XMM accumulator with MULPS + ADDPS.
// Final reduction: horizontal sum of the four accumulator lanes.
TEXT ·dotFloat32Asm(SB),NOSPLIT,$0-28
	MOVQ a+0(FP), AX
	MOVQ b+8(FP), BX
	MOVQ n+16(FP), CX

	XORPS X0, X0          // acc = {0,0,0,0}

	MOVQ CX, DI
	SHRQ $2, DI           // DI = n / 4
	JZ   tail_dot

vec_loop_dot:
	MOVUPS (AX), X1
	MOVUPS (BX), X2
	MULPS  X2, X1         // X1[i] = a[i] * b[i]
	ADDPS  X1, X0         // acc[i] += X1[i]
	ADDQ   $16, AX
	ADDQ   $16, BX
	DECQ   DI
	JNZ    vec_loop_dot

tail_dot:
	ANDQ $3, CX
	JZ   reduce_dot

scalar_loop_dot:
	MOVSS  (AX), X1
	MOVSS  (BX), X2
	MULSS  X2, X1
	ADDSS  X1, X0         // acc[0] += a[i]*b[i]  (scalar lane only)
	ADDQ   $4, AX
	ADDQ   $4, BX
	DECQ   CX
	JNZ    scalar_loop_dot

reduce_dot:
	// Horizontal sum of X0 = {a3,a2,a1,a0}
	MOVAPS  X0, X1
	SHUFPS  $0x4e, X0, X1 // X1 = {a1,a0,a3,a2}
	ADDPS   X1, X0        // X0 = {a3+a1, a2+a0, a1+a3, a0+a2}
	MOVAPS  X0, X1
	SHUFPS  $0xb1, X0, X1 // X1 = {a2+a0, a3+a1, ...}
	ADDPS   X1, X0        // X0[0] = sum of all four lanes
	MOVSS   X0, ret+24(FP) // write scalar result to return slot
	RET

// func maxFloat32Asm(s *float32, n int) float32
//
// Argument layout via FP pseudo-register (ABI0 / stack-based):
//   s+0(FP)    = s pointer  (8 bytes)
//   n+8(FP)    = n          (8 bytes)
//   ret+16(FP) = return     (4 bytes, padded to 4)  total args+ret = 20
//
// Strategy: load the first 4 elements as the initial max vector;
// then compare with each successive 4-element block using MAXPS.
TEXT ·maxFloat32Asm(SB),NOSPLIT,$0-20
	MOVQ s+0(FP), AX
	MOVQ n+8(FP), BX

	// Seed the max register with the first element broadcast to all lanes.
	MOVSS   (AX), X0
	SHUFPS  $0, X0, X0    // broadcast s[0] to all four lanes

	MOVQ BX, CX
	SHRQ $2, CX           // CX = n / 4
	JZ   tail_max

	// Reload: first full vector (may overlap scalar seed, that is OK).
	MOVUPS (AX), X0
	ADDQ   $16, AX
	DECQ   CX
	JZ     tail_max

vec_loop_max:
	MOVUPS (AX), X1
	MAXPS  X1, X0         // per-lane max
	ADDQ   $16, AX
	DECQ   CX
	JNZ    vec_loop_max

tail_max:
	ANDQ $3, BX
	JZ   reduce_max

scalar_loop_max:
	MOVSS   (AX), X1
	MAXSS   X1, X0        // X0[0] = max(X0[0], s[i])
	ADDQ    $4, AX
	DECQ    BX
	JNZ     scalar_loop_max

reduce_max:
	// Horizontal max of X0 = {m3,m2,m1,m0}.
	MOVAPS  X0, X1
	SHUFPS  $0x4e, X0, X1 // X1 = {m1,m0,m3,m2}
	MAXPS   X1, X0        // X0 = {max(m3,m1), max(m2,m0), ...}
	MOVAPS  X0, X1
	SHUFPS  $0xb1, X0, X1
	MAXPS   X1, X0        // X0[0] = max of all four lanes
	MOVSS   X0, ret+16(FP) // write scalar result to return slot
	RET
```

### Exercise 3: Test the Contract

Create `vec_test.go`:

```go
package simdvec

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
)

// --- correctness tests ----------------------------------------------------

func TestAddFloat32EmptySlice(t *testing.T) {
	t.Parallel()

	var dst, a, b []float32
	AddFloat32(dst, a, b) // must not panic
}

func TestAddFloat32SingleElement(t *testing.T) {
	t.Parallel()

	dst := make([]float32, 1)
	AddFloat32(dst, []float32{3}, []float32{4})
	if dst[0] != 7 {
		t.Fatalf("got %v, want 7", dst[0])
	}
}

func TestAddFloat32TableDriven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		src1     []float32
		src2     []float32
		wantLast float32
	}{
		{"exact vector width 4", []float32{1, 2, 3, 4}, []float32{10, 20, 30, 40}, 44},
		{"5 elements — tail 1", []float32{1, 2, 3, 4, 5}, []float32{10, 20, 30, 40, 50}, 55},
		{"7 elements — tail 3", make7(1), make7(10), 11},
		{"8 elements — two vectors", make8(1), make8(10), 11},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dst := make([]float32, len(tc.src1))
			AddFloat32(dst, tc.src1, tc.src2)

			// Verify against pure-Go reference.
			ref := make([]float32, len(tc.src1))
			addFloat32Go(ref, tc.src1, tc.src2)
			for i := range dst {
				if dst[i] != ref[i] {
					t.Errorf("dst[%d] = %v, ref[%d] = %v", i, dst[i], i, ref[i])
				}
			}
			if dst[len(dst)-1] != tc.wantLast {
				t.Errorf("last element = %v, want %v", dst[len(dst)-1], tc.wantLast)
			}
		})
	}
}

func TestAddFloat32PanicOnLengthMismatch(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on length mismatch, got none")
		}
	}()
	AddFloat32(make([]float32, 2), make([]float32, 2), make([]float32, 3))
}

func TestDotFloat32Empty(t *testing.T) {
	t.Parallel()

	if got := DotFloat32(nil, nil); got != 0 {
		t.Fatalf("dot of empty = %v, want 0", got)
	}
}

func TestDotFloat32TableDriven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b []float32
		want float32
	}{
		{"unit dot", []float32{1, 0, 0, 0}, []float32{0, 0, 0, 1}, 0},
		{"parallel", []float32{1, 2, 3, 4}, []float32{1, 2, 3, 4}, 30},
		{"5 elements", []float32{1, 1, 1, 1, 1}, []float32{2, 2, 2, 2, 2}, 10},
		{"7 elements", make7(1), make7(1), 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := DotFloat32(tc.a, tc.b)
			if !approxEqual(got, tc.want, 1e-5) {
				t.Errorf("DotFloat32 = %v, want %v", got, tc.want)
			}
			// Cross-check against pure-Go.
			ref := dotFloat32Go(tc.a, tc.b)
			if !approxEqual(got, ref, 1e-5) {
				t.Errorf("asm %v != go %v", got, ref)
			}
		})
	}
}

func TestMaxFloat32Empty(t *testing.T) {
	t.Parallel()

	if got := MaxFloat32(nil); !math.IsInf(float64(got), -1) {
		t.Fatalf("MaxFloat32(nil) = %v, want -Inf", got)
	}
}

func TestMaxFloat32TableDriven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		s    []float32
		want float32
	}{
		{"single", []float32{42}, 42},
		{"4 elements — exact", []float32{3, 1, 4, 2}, 4},
		{"5 elements — tail 1", []float32{3, 1, 4, 2, 9}, 9},
		{"7 elements — tail 3", []float32{1, 8, 2, 7, 3, 6, 4}, 8},
		{"8 elements", []float32{5, 3, 8, 1, 7, 2, 6, 4}, 8},
		{"negative values", []float32{-4, -1, -3, -2}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := MaxFloat32(tc.s)
			if got != tc.want {
				t.Errorf("MaxFloat32 = %v, want %v", got, tc.want)
			}
			ref := maxFloat32Go(tc.s)
			if got != ref {
				t.Errorf("asm %v != go %v", got, ref)
			}
		})
	}
}

// TestAgainstRandom cross-checks the assembly against the Go reference
// on random data to catch edge cases not covered by the table.
func TestAgainstRandom(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(42, 0))
	for _, n := range []int{0, 1, 2, 3, 4, 5, 7, 8, 15, 16, 17, 63, 64, 65, 127, 128, 129} {
		a := randomSlice(rng, n)
		b := randomSlice(rng, n)

		// AddFloat32
		dst := make([]float32, n)
		ref := make([]float32, n)
		AddFloat32(dst, a, b)
		addFloat32Go(ref, a, b)
		for i := range dst {
			if dst[i] != ref[i] {
				t.Errorf("add n=%d [%d]: asm=%v go=%v", n, i, dst[i], ref[i])
			}
		}

		// DotFloat32
		if n > 0 {
			got := DotFloat32(a, b)
			want := dotFloat32Go(a, b)
			if !approxEqual(got, want, 1e-4) {
				t.Errorf("dot n=%d: asm=%v go=%v", n, got, want)
			}
		}

		// MaxFloat32
		got := MaxFloat32(a)
		want := maxFloat32Go(a)
		if got != want {
			t.Errorf("max n=%d: asm=%v go=%v", n, got, want)
		}
	}
}

// Your turn: add TestAddFloat32LargeSlice that allocates a slice of 1,000,001
// elements (deliberately odd to exercise the tail path), fills it with 1.0,
// calls AddFloat32 with both sources equal to that slice, and asserts every
// element in dst equals 2.0.

// ExampleAddFloat32 is an auto-verified example (go test runs the Output check).
func ExampleAddFloat32() {
	dst := make([]float32, 4)
	AddFloat32(dst, []float32{1, 2, 3, 4}, []float32{10, 20, 30, 40})
	fmt.Println(dst)
	// Output:
	// [11 22 33 44]
}

// --- benchmarks -----------------------------------------------------------

var sinkF32 float32

func BenchmarkAddFloat32(b *testing.B) {
	for _, n := range []int{64, 1024, 65536, 1 << 20} {
		a := make([]float32, n)
		bv := make([]float32, n)
		dst := make([]float32, n)
		fill(a, 1)
		fill(bv, 2)

		b.Run("asm", func(b *testing.B) {
			b.SetBytes(int64(n) * 4)
			for b.Loop() {
				AddFloat32(dst, a, bv)
			}
		})
		b.Run("go", func(b *testing.B) {
			b.SetBytes(int64(n) * 4)
			for b.Loop() {
				addFloat32Go(dst, a, bv)
			}
		})
	}
}

func BenchmarkDotFloat32(b *testing.B) {
	for _, n := range []int{64, 1024, 65536, 1 << 20} {
		a := make([]float32, n)
		bv := make([]float32, n)
		fill(a, 1.5)
		fill(bv, 2.5)

		b.Run("asm", func(b *testing.B) {
			b.SetBytes(int64(n) * 4)
			for b.Loop() {
				sinkF32 = DotFloat32(a, bv)
			}
		})
		b.Run("go", func(b *testing.B) {
			b.SetBytes(int64(n) * 4)
			for b.Loop() {
				sinkF32 = dotFloat32Go(a, bv)
			}
		})
	}
}

func BenchmarkMaxFloat32(b *testing.B) {
	for _, n := range []int{64, 1024, 65536, 1 << 20} {
		s := make([]float32, n)
		fill(s, 1.0)
		s[n/2] = 100 // known max in the middle

		b.Run("asm", func(b *testing.B) {
			b.SetBytes(int64(n) * 4)
			for b.Loop() {
				sinkF32 = MaxFloat32(s)
			}
		})
		b.Run("go", func(b *testing.B) {
			b.SetBytes(int64(n) * 4)
			for b.Loop() {
				sinkF32 = maxFloat32Go(s)
			}
		})
	}
}

// --- helpers --------------------------------------------------------------

func randomSlice(rng *rand.Rand, n int) []float32 {
	s := make([]float32, n)
	for i := range s {
		s[i] = rng.Float32()*200 - 100
	}
	return s
}

func fill(s []float32, v float32) {
	for i := range s {
		s[i] = v
	}
}

func approxEqual(a, b, eps float32) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	// Relative tolerance: scale epsilon by the magnitude of the larger operand
	// so that SIMD lane-parallel summation (which reorders additions) does not
	// produce false failures when dot magnitudes are large.
	scale := float32(math.Max(1, math.Max(math.Abs(float64(a)), math.Abs(float64(b)))))
	return d <= eps*scale
}

func make7(v float32) []float32 {
	return []float32{v, v, v, v, v, v, v}
}

func make8(v float32) []float32 {
	return []float32{v, v, v, v, v, v, v, v}
}
```

### Exercise 4: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/simdvec"
)

func main() {
	n := 9 // deliberately not a multiple of 4

	a := make([]float32, n)
	b := make([]float32, n)
	for i := range a {
		a[i] = float32(i + 1)
		b[i] = float32(2)
	}

	// Vector addition.
	dst := make([]float32, n)
	simdvec.AddFloat32(dst, a, b)
	fmt.Printf("AddFloat32: %v\n", dst)

	// Dot product.
	dot := simdvec.DotFloat32(a, b)
	fmt.Printf("DotFloat32: %.1f\n", dot)

	// Maximum.
	mx := simdvec.MaxFloat32(a)
	fmt.Printf("MaxFloat32: %.1f\n", mx)
}
```

Run with:

```bash
go run ./cmd/demo
```

Expected output (n=9, a=[1..9], b=[2..2]):

```
AddFloat32: [3 4 5 6 7 8 9 10 11]
DotFloat32: 90.0
MaxFloat32: 9.0
```

## Common Mistakes

### Using VMOVAPS on Non-32-Byte-Aligned Pointers

Wrong: `VMOVAPS (AX), Y0` when `AX` holds a pointer to a Go slice's backing array. Go does not guarantee 32-byte alignment.

What happens: a general protection fault (`SIGSEGV` or `#GP`) at runtime. The fault appears as a mysterious crash with no clear error message when address is not `0x...00` or `0x...20`.

Fix: always use `VMOVUPS` for loads and stores to Go slice memory. The performance difference between aligned and unaligned loads is negligible on CPUs since Sandy Bridge (2011); correctness is not negotiable.

### Forgetting Tail Handling

Wrong: stop the loop after `n/4` iterations without processing the remaining `n mod 4` elements.

What happens: the last `n mod 4` elements of `dst` remain at their zero value. For a dot product the result is wrong but does not panic, making this a silent data corruption bug.

Fix: after the vector loop, add a scalar loop that processes one element at a time until `i == n`. The tests in this lesson cover lengths 1, 5, 7, 15, and 17 to catch exactly this class of off-by-vector errors.

### Forgetting to Write the Return Value to the FP Slot

Wrong: compute the scalar result in X0 and issue `RET` without first storing it.

What happens: `go vet` reports "RET without writing to N-byte ret+M(FP)". The caller reads an uninitialized stack slot; the returned value is garbage. On amd64 you may observe correct behaviour by accident in unoptimized builds, masking the bug until production.

Fix: before every `RET` in a function that returns a value, write the result to its named FP slot. For a `float32` return: `MOVSS X0, ret+24(FP)`. The offset and name must match the Go declaration exactly; `go vet` checks this.

### Getting the Frame-Size Suffix Wrong

Wrong: declare `TEXT ·maxFloat32Asm(SB),NOSPLIT,$0-24` when the actual argument+return size is 20 bytes.

What happens: `go vet` reports "wrong argument size N; expected $...-20". The assembler accepts the directive, but the Go ABI wrapper built by the compiler uses the wrong frame size, which can corrupt adjacent stack slots in callers.

Fix: count every byte manually. Each pointer or `int` is 8 bytes on amd64; each `float32` return is 4 bytes. Total all argument bytes and return bytes, then write that number after the hyphen: `$0-20`.

### Omitting the `//go:noescape` Annotation

Wrong: declare `func addFloat32Asm(dst, src1, src2 *float32, n int)` without `//go:noescape`.

What happens: the compiler conservatively assumes the pointer arguments escape to the heap, causing callers to heap-allocate intermediate slices and paying an allocation cost per call. For a function called in a tight loop this adds substantial overhead that defeats the purpose of SIMD.

Fix: add `//go:noescape` directly above the declaration. The annotation is a promise to the compiler; make sure the assembly does not actually cause the pointers to escape (it must not store them in any globally reachable location).

### Mixing Legacy SSE and VEX Instructions in the Same File

Wrong: mix `MOVUPS` / `ADDPS` (legacy SSE encoding) with `VADDPS` / `VMOVUPS` (VEX encoding) in the same assembly file.

What happens: on some Intel microarchitectures (Haswell/Broadwell and older Sandy Bridge) this causes a state transition penalty of up to 70 cycles each time the CPU switches between the legacy SSE and VEX execution domains. For a tight inner loop this is catastrophic.

Fix: choose one encoding family and use it consistently throughout the entire file. This lesson uses legacy SSE encoding (`ADDPS`, `MULPS`, `MAXPS`, `MOVUPS`), which is compatible with every amd64 CPU without a `GOAMD64` requirement. If you upgrade to AVX2 (`VADDPS Y0, Y1, Y2`), use VEX encoding throughout and call `VZEROALL` before returning to clean YMM state.

## Verification

From `~/go-exercises/simdvec`:

```bash
# Confirm no gofmt diff in the Go source files (excludes .s files).
test -z "$(gofmt -l .)"

# Check for vet issues including assembly/Go signature mismatches.
go vet ./...

# Build the package and demo binary.
go build ./...

# Run all tests, including the random cross-check and table tests.
go test -count=1 -race ./...

# Run benchmarks to see the SIMD vs Go speedup.
go test -bench=. -benchtime=3s -benchmem ./...
```

All five commands must succeed. On amd64 the benchmark output should show the `asm` sub-benchmark with higher `MB/s` than the `go` sub-benchmark at every data size >= 1024. The speedup varies from 1.5x at 64 elements (loop overhead dominates) to 3-4x at 65536+ elements (memory bandwidth reveals the full throughput difference).

Inspect the generated code to confirm SSE2 instructions appear:

```bash
go build -o /dev/null ./... && go tool objdump -s 'simdvec.addFloat32Asm' ./simdvec.test 2>/dev/null | head -20
```

## Summary

- Go does not auto-vectorize. SIMD requires hand-written Plan 9 assembly in a `.s` file alongside the Go package.
- XMM registers (X0–X15) hold 4 float32 or 2 float64 lanes; YMM registers (Y0–Y15) hold 8 float32 or 4 float64 lanes.
- Use `MOVUPS` / `VMOVUPS` for all Go slice memory access. Never use aligned forms (`MOVAPS`, `VMOVAPS`) without a guaranteed-aligned allocator.
- Assembly files (`.s`) always use ABI0: every argument is read from a named `FP` offset (`dst+0(FP)`, `n+24(FP)`, etc.) and every return value must be written to its `FP` slot before `RET`. The Go compiler generates a thin ABI wrapper so Go callers using the register ABI can invoke the function transparently.
- Always handle the tail: the `n mod W` elements that do not fill a complete vector must be processed by a scalar fallback loop.
- Mark assembly-backed functions `//go:noescape` to prevent spurious heap escapes in callers.
- Cross-check the SIMD path against a pure-Go reference on random inputs of every length mod W (0, 1, 2, ..., W-1) to catch off-by-vector bugs.
- Do not mix legacy SSE and VEX instruction encodings in the same file; prefer consistent use of one family.

## What's Next

Next: [Analyzing Compiler Output](../12-analyzing-compiler-output/12-analyzing-compiler-output.md).

## Resources

- [A Quick Guide to Go's Assembler](https://go.dev/doc/asm) — official Plan 9 assembly reference; covers register names, pseudo-registers, function declarations, and the FP convention.
- [Go internal/cpu source](https://go.dev/src/internal/cpu/cpu.go) — canonical source for how the Go runtime detects CPU features at startup; the reference for the `X86.HasAVX2` pattern.
- [Go 1.18 Release Notes: GOAMD64](https://go.dev/doc/go1.18) — official documentation of `GOAMD64` levels (v1–v4) and corresponding build constraints (`//go:build amd64.v3`).
- [stuartcarnie/go-simd](https://github.com/stuartcarnie/go-simd) — real-world Go SIMD library with AVX2 assembly; useful as a structural reference for multi-function `.s` files.
- [Go issue #53171: proposal for a SIMD package](https://github.com/golang/go/issues/53171) — the ongoing discussion around adding first-class SIMD support to Go; useful context for understanding why hand-written assembly is still the standard approach.
