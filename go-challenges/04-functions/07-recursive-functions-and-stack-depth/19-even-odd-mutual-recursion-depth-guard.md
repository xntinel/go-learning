# Exercise 19: Mutual Recursion for Validation Rules with Depth Limit

**Nivel: Intermedio** — validacion rapida (un test corto).

The textbook example of mutual recursion is `IsEven`/`IsOdd`, each calling
the other with `n-1`. That toy hides a real pattern: a chain of validation
rules where each step's rule depends on which "kind" of step it is, and
control alternates between two functions instead of one function calling
itself. Here that pattern validates that a slice of values alternates even,
odd, even, odd — `checkEven` handles a position and hands the next one to
`checkOdd`, which hands the one after that back to `checkEven`. Because the
recursion depth tracks the length of the input slice directly, a
caller-supplied slice with no size limit is a stack-exhaustion risk exactly
like unbounded recursion anywhere else, so a depth guard caps how many
positions the mutual recursion will walk.

This module is fully self-contained: its own `go mod init`, the validator
inline, its own demo and tests.

## What you'll build

```text
chainvalidate/                independent module: example.com/chainvalidate
  go.mod                        go 1.24
  chainvalidate.go               func ValidateAlternating(values []int, maxDepth int) error
  chainvalidate_test.go           valid chain, empty, broken parity, depth limit, exact limit
  cmd/
    demo/
      main.go                     valid chain, a broken chain, and a depth-guarded huge chain
```

- Files: `chainvalidate.go`, `cmd/demo/main.go`, `chainvalidate_test.go`.
- Implement: `func ValidateAlternating(values []int, maxDepth int) error`
  backed by two mutually recursive functions, `checkEven` and `checkOdd`,
  each validating one position's parity and calling the other for the next.
- Test: a valid alternating chain passes; an empty slice is trivially valid;
  a chain that breaks parity at some position reports `ErrParityMismatch`;
  a chain longer than `maxDepth` reports `ErrDepthExceeded`; a chain exactly
  as long as `maxDepth` is still accepted.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Two functions, one shared depth counter

`checkEven` and `checkOdd` are not two independent recursions — they are one
recursion split across two functions, each responsible for one alternating
"kind" of step. `checkEven(values, i, maxDepth)` requires `values[i]` (when
`i` is in range) to be even, then calls `checkOdd(values, i+1, maxDepth)`
for the next position; `checkOdd` is the mirror image, requiring an odd
value and handing off to `checkEven`. Between the two of them, every
position in `values` gets validated exactly once, in order, with the active
function alternating in lockstep with the position's required parity.

The depth guard has to live in both functions, checked before either one
looks at `values[i]`, because either one might be the one executing when
the limit is reached — the recursion does not stay inside a single
function the way a plain `factorial`-style recursion would. Get the guard
into only one of the two functions and half the possible chains sail past
the limit unchecked, off by exactly one position. Checking `i >= maxDepth`
before touching `values[i]` also means the guard fires strictly before the
position it would have processed, so a slice built exactly `maxDepth` long
is still valid — the guard rejects going *past* the limit, not landing
on it.

Create `chainvalidate.go`:

```go
// Package chainvalidate validates that a slice of values alternates even,
// odd, even, odd, ... using two mutually recursive functions, one per
// parity, each calling the other for the next position. A depth guard bounds
// how many positions the mutual recursion will walk, so a very long input
// slice cannot exhaust the goroutine stack.
package chainvalidate

import (
	"errors"
	"fmt"
)

// ErrDepthExceeded is returned when values is longer than the configured
// maxDepth, before the mutual recursion walks any further into it.
var ErrDepthExceeded = errors.New("chainvalidate: validation depth exceeds limit")

// ErrParityMismatch is returned when a value at some position has the wrong
// parity for that position (position 0 must be even, 1 odd, 2 even, ...).
var ErrParityMismatch = errors.New("chainvalidate: value has wrong parity for its position")

// ValidateAlternating checks that values[0] is even, values[1] is odd,
// values[2] is even, and so on, using two mutually recursive checkers. It
// refuses to validate more than maxDepth positions, returning
// ErrDepthExceeded instead of continuing to recurse.
func ValidateAlternating(values []int, maxDepth int) error {
	return checkEven(values, 0, maxDepth)
}

// checkEven requires values[i] (if any) to be even, then hands the next
// position to checkOdd. checkOdd does the mirror image. Between them they
// walk the whole slice one position per pair of mutual calls.
func checkEven(values []int, i, maxDepth int) error {
	if i == len(values) {
		return nil
	}
	if i >= maxDepth {
		return fmt.Errorf("%w: position %d", ErrDepthExceeded, i)
	}
	if values[i]%2 != 0 {
		return fmt.Errorf("%w: position %d (%d) must be even", ErrParityMismatch, i, values[i])
	}
	return checkOdd(values, i+1, maxDepth)
}

func checkOdd(values []int, i, maxDepth int) error {
	if i == len(values) {
		return nil
	}
	if i >= maxDepth {
		return fmt.Errorf("%w: position %d", ErrDepthExceeded, i)
	}
	if values[i]%2 == 0 {
		return fmt.Errorf("%w: position %d (%d) must be odd", ErrParityMismatch, i, values[i])
	}
	return checkEven(values, i+1, maxDepth)
}
```

### The runnable demo

The demo validates a correct alternating chain, then a chain broken at
position 3, then a 5000-element chain checked against a `maxDepth` of 1000
to show the guard tripping instead of recursing the rest of the way through.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/chainvalidate"
)

func main() {
	valid := []int{2, 3, 4, 5, 6, 7}
	if err := chainvalidate.ValidateAlternating(valid, 100); err != nil {
		fmt.Println("unexpected error:", err)
	} else {
		fmt.Println("valid chain: ok")
	}

	broken := []int{2, 3, 4, 4, 6, 7}
	if err := chainvalidate.ValidateAlternating(broken, 100); err != nil {
		fmt.Println("broken chain:", err)
	}

	huge := make([]int, 5000)
	for i := range huge {
		huge[i] = i // index parity already matches the required value parity
	}
	if err := chainvalidate.ValidateAlternating(huge, 1000); err != nil {
		fmt.Println("depth-guarded chain:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid chain: ok
broken chain: chainvalidate: value has wrong parity for its position: position 3 (4) must be odd
depth-guarded chain: chainvalidate: validation depth exceeds limit: position 1000
```

### Tests

`TestValidateAlternatingAcceptsValidChain` and `TestValidateAlternatingEmptyIsValid`
cover the happy paths. `TestValidateAlternatingRejectsBrokenParity` and
`TestValidateAlternatingRejectsWrongFirstValue` check that a mismatch
anywhere — including position 0 — is caught with `ErrParityMismatch`.
`TestValidateAlternatingEnforcesDepthLimit` checks the guard fires on a
chain past the limit, and `TestValidateAlternatingAcceptsChainAtExactLimit`
is the boundary check: a chain exactly `maxDepth` long must still pass.

Create `chainvalidate_test.go`:

```go
package chainvalidate

import (
	"errors"
	"testing"
)

func TestValidateAlternatingAcceptsValidChain(t *testing.T) {
	t.Parallel()

	if err := ValidateAlternating([]int{2, 3, 4, 5, 6, 7}, 100); err != nil {
		t.Fatalf("ValidateAlternating() error = %v, want nil", err)
	}
}

func TestValidateAlternatingEmptyIsValid(t *testing.T) {
	t.Parallel()

	if err := ValidateAlternating(nil, 100); err != nil {
		t.Fatalf("ValidateAlternating(nil) error = %v, want nil", err)
	}
}

func TestValidateAlternatingRejectsBrokenParity(t *testing.T) {
	t.Parallel()

	err := ValidateAlternating([]int{2, 3, 4, 4, 6, 7}, 100)
	if !errors.Is(err, ErrParityMismatch) {
		t.Fatalf("error = %v, want %v", err, ErrParityMismatch)
	}
}

func TestValidateAlternatingRejectsWrongFirstValue(t *testing.T) {
	t.Parallel()

	err := ValidateAlternating([]int{1, 3, 5}, 100)
	if !errors.Is(err, ErrParityMismatch) {
		t.Fatalf("error = %v, want %v", err, ErrParityMismatch)
	}
}

func TestValidateAlternatingEnforcesDepthLimit(t *testing.T) {
	t.Parallel()

	values := make([]int, 2000)
	for i := range values {
		values[i] = i
	}

	err := ValidateAlternating(values, 500)
	if !errors.Is(err, ErrDepthExceeded) {
		t.Fatalf("error = %v, want %v", err, ErrDepthExceeded)
	}
}

func TestValidateAlternatingAcceptsChainAtExactLimit(t *testing.T) {
	t.Parallel()

	values := make([]int, 500)
	for i := range values {
		values[i] = i
	}

	if err := ValidateAlternating(values, 500); err != nil {
		t.Fatalf("ValidateAlternating() error = %v, want nil at exact limit", err)
	}
}
```

Run it: `go test -count=1 ./...`

## Review

The validator is correct when both mutually recursive halves agree on where
they are: `checkEven` only ever looks at even positions, `checkOdd` only at
odd ones, and the depth guard is checked identically in both, so no
position slips through unguarded no matter which function happens to be
handling it. The mistake this exercise targets is adding the depth check to
only one of the two mutually recursive functions — a plausible slip since
it is easy to think of `checkEven` as "the" recursive function and
`checkOdd` as a helper it calls, when in fact they are peers sharing one
piece of state. `TestValidateAlternatingEnforcesDepthLimit` uses a slice
long enough that the limit is hit while `checkOdd` (not `checkEven`) is
active, which is exactly the case an incomplete guard would miss.

## Resources

- [Go Specification: Function declarations (mutual recursion)](https://go.dev/ref/spec#Function_declarations)
- [fmt package (error wrapping with %w)](https://pkg.go.dev/fmt#Errorf)
- [errors.Is](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-graph-shortest-path-explicit-stack.md](18-graph-shortest-path-explicit-stack.md) | Next: [20-nested-cache-invalidation-memoized-cascade.md](20-nested-cache-invalidation-memoized-cascade.md)
