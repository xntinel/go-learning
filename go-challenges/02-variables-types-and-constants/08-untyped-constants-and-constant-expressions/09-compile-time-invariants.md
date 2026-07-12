# Exercise 9: Compile-Time Invariant Assertions With Constant Expressions

Some invariants should never reach a test, let alone production: a ring-buffer size
that must be a power of two, two limits that must stay ordered, a lookup table
whose length must match an enum count. This module encodes each as a constant
expression the compiler must satisfy, so a drift fails the *build* — zero runtime
cost, caught before deploy.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
constguards/                  independent module: example.com/constguards
  go.mod                      go 1.26
  guards.go                   const _ = uint(...) assertions; Mask, LevelName, limits
  cmd/
    demo/
      main.go                 exercises the guarded values at runtime
  guards_test.go              runtime confirmation of the guarded behavior
```

Files: `guards.go`, `cmd/demo/main.go`, `guards_test.go`.
Implement: a power-of-two guard on `bufSize` and a `Mask`; an ordering guard on two
limits; a table-length-matches-enum-count guard; runtime accessors for each.
Test: `Mask` wraps a power-of-two ring buffer; the limits are ordered;
`len(levelNames) == levelCount`; `go build ./...` succeeds; `go vet` is clean.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/08-untyped-constants-and-constant-expressions/09-compile-time-invariants/cmd/demo
cd go-solutions/02-variables-types-and-constants/08-untyped-constants-and-constant-expressions/09-compile-time-invariants
```

### The const _ = uint(expr) assertion idiom

Converting a *negative* untyped constant to `uint` is a compile error. That single
fact is the whole toolkit. `const _ = uint(expr)` compiles only when `expr` is
non-negative, so you phrase each invariant as "some constant expression that is
non-negative exactly when the invariant holds" and let the build enforce it. The
blank identifier `_` means the constant is never referenced — its only job is to be
type-checked.

Three invariants, three phrasings:

- Power of two: `bufSize & (bufSize - 1)` is zero exactly when `bufSize` is a power
  of two. So `const _ = uint(0 - (bufSize & (bufSize-1)))` is `uint(0)` when the
  invariant holds and `uint(negative)` — a build error — when it does not. A power-
  of-two size lets you replace `i % bufSize` with the faster `i & (bufSize-1)`, and
  the mask is only correct if the size really is a power of two, so the guard
  protects the optimization.
- Ordering: `const _ = uint(hardLimit - softLimit)` fails the build the moment
  `hardLimit` drops below `softLimit`. Two limits defined independently (perhaps in
  different files or from different config sources) can now never drift out of
  order without breaking the build.
- Exact table length: pairing `const _ = uint(len(levelNames) - levelCount)` with
  `const _ = uint(levelCount - len(levelNames))` asserts the two are equal — each
  direction guards one inequality, and only equality satisfies both. `len` of an
  array-typed value is a compile-time constant, so this is a pure build-time check;
  add a log level without extending the table and the build fails.

These guards have zero runtime cost — no code is emitted for them. Their success
*is* the test. A commented, would-not-compile variant of the power-of-two guard is
shown so you can see the failure without breaking the build.

Create `guards.go`:

```go
package constguards

// bufSize is a ring-buffer capacity. It MUST be a power of two so that the wrap
// can use a bit-mask instead of a modulo.
const bufSize = 1 << 12 // 4096

// Power-of-two guard: bufSize & (bufSize-1) is 0 iff bufSize is a power of two.
// If it is not, the expression is negative and uint() fails to compile.
const _ = uint(0 - (bufSize & (bufSize - 1)))

// A non-power-of-two would fail the build. Left commented so it does not:
//
//	const badSize = 3000
//	const _ = uint(0 - (badSize & (badSize - 1))) // constant -2992 overflows uint

// softLimit and hardLimit are defined independently but must stay ordered.
const (
	softLimit = 900
	hardLimit = 1000
)

// Ordering guard: fails the build if hardLimit ever drops below softLimit.
const _ = uint(hardLimit - softLimit)

// Log levels; levelCount is the iota-derived member count.
const (
	levelDebug = iota
	levelInfo
	levelWarn
	levelError
	levelCount
)

// levelNames is the lookup table; its length must match levelCount.
var levelNames = [...]string{
	levelDebug: "debug",
	levelInfo:  "info",
	levelWarn:  "warn",
	levelError: "error",
}

// Table-length guard: both directions assert len(levelNames) == levelCount.
const (
	_ = uint(len(levelNames) - levelCount)
	_ = uint(levelCount - len(levelNames))
)

// Mask wraps an index into the ring buffer using the power-of-two bit-mask.
func Mask(i int) int {
	return i & (bufSize - 1)
}

// BufSize returns the ring-buffer capacity.
func BufSize() int {
	return bufSize
}

// SoftLimit and HardLimit expose the guarded limits.
func SoftLimit() int { return softLimit }
func HardLimit() int { return hardLimit }

// LevelName returns the name for a level, or "unknown" if out of range.
func LevelName(level int) string {
	if level < 0 || level >= len(levelNames) {
		return "unknown"
	}
	return levelNames[level]
}

// LevelCount returns the number of defined log levels.
func LevelCount() int {
	return levelCount
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/constguards"
)

func main() {
	n := constguards.BufSize()
	fmt.Printf("bufSize=%d mask(n)=%d mask(n+1)=%d mask(n+3)=%d\n",
		n, constguards.Mask(n), constguards.Mask(n+1), constguards.Mask(n+3))

	fmt.Printf("limits soft=%d hard=%d ordered=%v\n",
		constguards.SoftLimit(), constguards.HardLimit(),
		constguards.SoftLimit() <= constguards.HardLimit())

	fmt.Printf("levels=%d names:", constguards.LevelCount())
	for i := 0; i < constguards.LevelCount(); i++ {
		fmt.Printf(" %s", constguards.LevelName(i))
	}
	fmt.Println()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
bufSize=4096 mask(n)=0 mask(n+1)=1 mask(n+3)=3
limits soft=900 hard=1000 ordered=true
levels=4 names: debug info warn error
```

### Tests

The compile-time guards have already passed by the time any test runs — a build
that got this far proves them. The runtime tests confirm the *behavior* those
guards protect: `TestMaskWrapsPowerOfTwo` shows the mask equals a modulo for a
power-of-two size; `TestLimitsOrdered` confirms the ordering; `TestTableMatchesEnum`
confirms the table length equals the level count at runtime too.

Create `guards_test.go`:

```go
package constguards

import "testing"

func TestMaskWrapsPowerOfTwo(t *testing.T) {
	t.Parallel()

	n := BufSize()
	for _, i := range []int{0, 1, n - 1, n, n + 1, n + 3, 2*n + 7} {
		if got, want := Mask(i), i%n; got != want {
			t.Errorf("Mask(%d) = %d, want %d (i%%n)", i, got, want)
		}
	}
}

func TestLimitsOrdered(t *testing.T) {
	t.Parallel()

	if SoftLimit() > HardLimit() {
		t.Fatalf("soft %d must not exceed hard %d", SoftLimit(), HardLimit())
	}
}

func TestTableMatchesEnum(t *testing.T) {
	t.Parallel()

	if len(levelNames) != LevelCount() {
		t.Fatalf("len(levelNames) = %d, LevelCount() = %d", len(levelNames), LevelCount())
	}
	if got := LevelName(levelWarn); got != "warn" {
		t.Fatalf("LevelName(levelWarn) = %q, want warn", got)
	}
	if got := LevelName(99); got != "unknown" {
		t.Fatalf("LevelName(99) = %q, want unknown", got)
	}
}
```

## Review

The guards are correct when the build succeeds with the invariants intact and would
fail if any drifted — which is why the module ships the commented would-not-compile
variant so you can see the failure mode without breaking the build. `Mask` is only
valid because the power-of-two guard holds; the test cross-checks it against the
modulo it replaces. The lesson is that constants can protect themselves: a
mismatched table length or an inverted limit pair is caught at compile time, with no
runtime cost and no chance of the bug reaching an incident.

## Resources

- [Go Language Specification: Constant expressions](https://go.dev/ref/spec#Constant_expressions) — why a negative constant cannot convert to uint.
- [Go Language Specification: Length and capacity](https://go.dev/ref/spec#Length_and_capacity) — when `len` of an array is a constant.
- [The Go Blog: Constants](https://go.dev/blog/constants) — the compile-time constant model these guards exploit.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-job-status-typed-enum.md](08-job-status-typed-enum.md) | Next: [../09-blank-identifier-and-shadowing/00-concepts.md](../09-blank-identifier-and-shadowing/00-concepts.md)
