# Exercise 16: Rotate a Partition Ring in Place with Three Reversals

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

When a broker leaves a partition assignment ring, the simplest rebalance is
to rotate the whole ring so ownership shifts evenly to the remaining members
instead of piling extra load on one neighbor. The obvious way to rotate a
slice left by `k` -- build a new slice from `s[k:]` followed by `s[:k]` -- is
correct but allocates a fresh backing array every time, which matters when
the ring is rebalanced on every membership change in a hot path. There is a
classic in-place alternative that needs no extra buffer at all: reverse the
first `k` elements, reverse the rest, then reverse the whole thing. Three
linear passes, zero allocations, O(1) extra space.

This exercise builds `rotate`, a package with exactly one operation --
`Left` -- that performs that in-place rotation, and proves both its
correctness and its zero-allocation claim with `testing.AllocsPerRun` rather
than asserting either by argument.

The same shape shows up anywhere ownership or priority needs to shift by a
fixed number of slots without disturbing relative order: a consistent-hashing
ring reassigning virtual nodes after a member joins, a round-robin load
balancer's backend list advancing past the server it just used, a fixed-size
circular schedule of on-call engineers rolling forward by one week. In every
one of those, the rotation runs far more often than the data changes size,
which is exactly the profile where "correct but allocates every call"
quietly becomes a measurable line in a profiler's allocation flame graph,
long after the code review that approved it moved on.

Three reversals is not the only way to rotate in place -- a cyclic
permutation using a single temporary element and following the cycle each
element belongs to also achieves O(1) space -- but it is the version worth
knowing first, because it needs no case analysis on `gcd(k, len(s))` to
prove termination, and because `reverse` is a function most Go programmers
have already written for an unrelated reason (printing a slice backwards,
palindrome checking) and can immediately recognize as safe. Composing a
primitive you already trust into a bigger correct algorithm is a more
reliable path to correct code than inventing a new primitive from scratch,
and this module is as much about that composition habit as it is about
rotation specifically.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
rotate/                        module example.com/rotate
  go.mod                       go 1.24
  rotate.go                    Left(s []int, k int) -- three-reversal in-place rotation
  rotate_test.go                table over k=0, k=len, k>len, negative k, empty, single element; allocating baseline contrast; ExampleLeft
```

- Files: `rotate.go`, `rotate_test.go`.
- Implement: `Left(s []int, k int)`, which normalizes `k` modulo `len(s)` and rotates `s` left in place via three calls to a private `reverse(s []int)` helper (`s[:k]`, `s[k:]`, then the whole of `s`).
- Test: a table covering ordinary rotation, `k == 0`, `k == len(s)`, `k > len(s)` (wraps via modulo), negative `k`, an empty slice, and a single-element slice; a contrast against an unexported allocating baseline showing it produces the identical result at strictly higher allocation cost; a `testing.AllocsPerRun` test proving `Left` allocates zero times per call; `ExampleLeft` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/rotate
cd ~/go-exercises/rotate
go mod init example.com/rotate
go mod edit -go=1.24
```

### Why three reversals rotate a slice, and why that beats an allocation

Picture `s = [A B | C D E]` split at index `k = 2`. Reversing the first
segment gives `[B A | C D E]`. Reversing the second segment gives
`[B A | E D C]`. Reversing the *whole* slice then flips both halves end for
end and swaps their order in one pass: `[C D E | A B]`. That is exactly `s`
rotated left by `k` -- the two segments swapped places, and each segment's
internal order was restored by the reversal that reversed it twice (once
alone, once as part of the whole-slice reversal). Each of the three passes
touches every element it covers exactly once, so the whole algorithm is O(n)
time, and because `reverse` only swaps elements within the slice it is
given -- itself just a two-index sub-slice of `s`, `s[:k]` and `s[k:]` -- no
auxiliary buffer is ever allocated. This is the in-place counterpart to the
two-pointer swap this lesson's sibling exercises use for reversal; here it
is composed three times to produce a rotation instead of a simple flip.

Tracing why the double reversal preserves each segment's internal order is
worth doing once with real indices, because "reversing something twice puts
it back the way it was" is easy to state and easy to doubt. Take
`s = [0 1 2 3 4 5 6 7]` with `k = 3`. After `reverse(s[:3])`, the first
three elements read `2 1 0` and the rest are untouched:
`[2 1 0 3 4 5 6 7]`. After `reverse(s[3:])`, the last five read
`7 6 5 4 3`: `[2 1 0 7 6 5 4 3]`. Now `reverse(s)` flips the entire
eight-element slice end for end. Reading the array back to front --
`3 4 5 6 7 0 1 2` -- lands on exactly the target rotation, and each of the
two segments comes out in its *original* internal order (`3 4 5 6 7`, then
`0 1 2`) because reversing an already-reversed run un-reverses it; the
whole-slice pass is what relocates the segments, and the two prior passes
are what cancel out to leave each segment's own element order untouched by
that relocation. Nothing about this depends on `k` dividing `len(s)` evenly
or on the element type being comparable -- it works identically for a slice
of structs, pointers, or (with the type parameter generalized) any `T`.

The rotation performed here operates on `[]int` specifically, which keeps
the exercise's core idea -- the three-reversal composition and its
allocation cost -- uncluttered by generic type-parameter syntax. The same
three calls to `reverse` work verbatim on `[]T` for any `T`, and turning
`Left` into `Left[T any](s []T, k int)` and `reverse` into
`reverse[T any](s []T)` is a mechanical change that does not touch the
algorithm at all -- worth doing the moment a second call site in a real
codebase needs to rotate something other than `[]int`.

The edge in the modulo is worth naming directly: normalizing with
`((k % n) + n) % n` handles `k > n` (a rotation request larger than the
ring, which wraps around one or more full turns) and negative `k` (a
"rotate right" request expressed as a negative left rotation) in the same
line, without a separate branch for either. Only `n == 0` needs an explicit
early return, because `k % 0` panics.

Tracing the negative case concretely is worth doing once, because `%` on a
negative operand is where a hand-rolled rotation most often goes wrong in
Go: `-1 % 5` evaluates to `-1`, not `4` -- Go's `%` follows the sign of the
dividend, unlike Python's. `((-1 % 5) + 5) % 5` first computes `-1`, then
`-1 + 5 = 4`, then `4 % 5 = 4`, landing on the correct positive-equivalent
rotation. Skipping the outer `% n` after adding `n` would also be a subtle
bug for the ordinary case `k > n`: `((k % n) + n)` alone can still exceed
`n` when `k` was already non-negative and less than `n` to begin with is not
an issue, but writing the formula without the final `% n` "because k is
never negative here" is exactly the kind of reasoning that breaks the first
time someone calls `Left` with a computed offset that turns out negative at
runtime.

The tempting shortcut -- and this module's real trap -- is to build the
rotated result the straightforward way instead:

```go
result := slices.Concat(s[k:], s[:k])   // correct, but allocates every call
```

That produces the identical permutation and does not mutate its argument,
which is sometimes exactly what you want. But it allocates a brand-new
backing array on every single call, and a ring rebalanced on every consumer
join and leave in a hot path turns that into steady allocator and GC
pressure for no reason. This module does not export that version at all: it
lives only in the test file, as the thing the allocation test proves wrong.

Create `rotate.go`:

```go
// Package rotate rotates a slice left by k positions in place, using the
// classic three-reversal algorithm: reverse the first k elements, reverse
// the rest, then reverse the whole slice. It is meant to be dropped into a
// hot rebalance path -- a partition ring reassigned on every broker join or
// leave -- where allocating a fresh backing array on every rotation would
// otherwise become steady GC pressure.
package rotate

// Left rotates s left by k positions in place: reverse s[:k], reverse
// s[k:], then reverse the whole of s. Composing those three reversals
// produces the same permutation as copying s[k:] followed by s[:k] into a
// new buffer, but in O(n) time and O(1) extra space -- no allocation,
// regardless of len(s). k is normalized modulo len(s) first, so k == 0,
// k == len(s), k > len(s), and negative k (a "rotate right" expressed as a
// negative left rotation) are all handled without a separate branch; only
// the empty slice needs an early return, since %0 would panic.
//
// Left mutates the elements of s through its existing backing array; it
// never allocates and the slice header s itself is unchanged, only its
// contents. It is not safe for concurrent use: s must not be read or
// written by another goroutine while Left is running.
func Left(s []int, k int) {
	n := len(s)
	if n == 0 {
		return
	}
	k = ((k % n) + n) % n
	if k == 0 {
		return
	}
	reverse(s[:k])
	reverse(s[k:])
	reverse(s)
}

// reverse flips s end for end in place.
func reverse(s []int) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
```

### Using it

`Left` is the whole surface: call it with the slice to rotate and the number
of positions, and it returns nothing -- the mutation happens through `s`'s
existing backing array. That is the aliasing contract the doc comment
states directly: no new array is ever allocated, so a caller that needs the
pre-rotation order preserved must clone `s` first (`slices.Clone`) before
calling `Left`. Because `Left` mutates through a shared backing array, it
carries the concurrency contract this lesson has applied throughout: it is
not safe to call from one goroutine while another reads or writes the same
slice, and the caller is responsible for any synchronization a shared ring
needs.

A caller that needs the rotation applied without disturbing the original
order elsewhere -- for example, computing a preview of the next assignment
before committing to it -- clones first: `preview := slices.Clone(ring);
rotate.Left(preview, 1)`. That is the caller's responsibility, not
`Left`'s, because a package meant for a hot path should not decide on the
caller's behalf whether a defensive copy is warranted; forcing an allocation
on every call to protect against a caller who did not need it would defeat
the entire point of building this in-place instead of using
`slices.Concat`.

The module has no `main.go`, because an in-place rotation is a library
primitive, not a tool. Its executable demonstration is `ExampleLeft`: `go
test` runs it and compares its standard output against the `// Output:`
comment, so the usage shown below cannot drift away from the code.

```go
func ExampleLeft() {
	ring := []int{0, 1, 2, 3, 4, 5, 6, 7}
	fmt.Println("before:", ring)

	Left(ring, 3)
	fmt.Println("after rotating left by 3:", ring)

	Left(ring, -3)
	fmt.Println("after rotating back right by 3:", ring)

	// Output:
	// before: [0 1 2 3 4 5 6 7]
	// after rotating left by 3: [3 4 5 6 7 0 1 2]
	// after rotating back right by 3: [0 1 2 3 4 5 6 7]
}
```

Rotating left by `3` and then by `-3` returns the ring to its original
order, which is the modulo normalization's negative-`k` handling doing
double duty: a negative rotation is just a positive one in the other
direction, computed by the same line of arithmetic. Notice, too, that the
example never checks an error return -- `Left` has none, because there is no
input it can reject. Every `int` value of `k` is valid (the modulo
normalizes it), and every `[]int`, including `nil`, is valid (the length-zero
early return handles it). That absence of an error path is itself a design
choice worth naming: not every component in this lesson needs a sentinel
error, only the ones with a genuine invalid-configuration case, and manufacturing
one here just to have one would be worse than omitting it.

### Tests

`TestLeft` is the correctness table: it drives `Left` through ordinary
rotation and every edge the modulo normalization has to get right -- `k ==
0`, a full-length `k` (also a no-op), a `k` larger than the slice, a
negative `k`, an empty slice, and a single-element slice. `rotateLeftNaive`
is the antipattern this module warns against: it builds the rotated result
with `slices.Concat(s[k:], s[:k])` instead of rotating in place, is never
exported and never reachable from `Left`, and exists solely so the tests can
measure what it costs. `TestNaiveAgreesWithLeft` pins that the antipattern
is not a correctness bug -- it agrees with `Left` on every case -- only a
cost one. `TestNaiveAllocatesAndLeftDoesNot` is the test that actually
proves the O(1)-space claim rather than assuming it: it asserts `Left`
allocates exactly zero times and `rotateLeftNaive` allocates at least once,
a property, never an exact realloc count, since the runtime's growth curve
is not a documented contract. It does not call `t.Parallel`, because
`testing.AllocsPerRun` panics if run from a parallel subtest.

Running both `Left` and `rotateLeftNaive` over the same `1000`-element slice
in a loop of a hundred iterations is deliberate: `AllocsPerRun` reports a
mean, and a single call is cheap enough on a modern machine that a one-shot
measurement can be swamped by noise from an unrelated background
allocation elsewhere in the test binary. A hundred repetitions is enough to
make the mean stable without turning the test into a benchmark -- which is
also why this assertion lives in `_test.go` and never in a `Benchmark...`
function or in any documented command output: this lesson's determinism
rule treats a measured allocation *count* as a legitimate property to
assert, but a measured *duration* is exactly the kind of number that
changes between machines and toolchains, so `go test` only ever reports
pass or fail, never a number of nanoseconds.

Create `rotate_test.go`:

```go
package rotate

import (
	"fmt"
	"slices"
	"testing"
)

func TestLeft(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []int
		k    int
		want []int
	}{
		{"ordinary rotation", []int{0, 1, 2, 3, 4, 5, 6, 7}, 3, []int{3, 4, 5, 6, 7, 0, 1, 2}},
		{"k == 0 is a no-op", []int{0, 1, 2, 3, 4}, 0, []int{0, 1, 2, 3, 4}},
		{"k == len(s) is a full rotation, also a no-op", []int{0, 1, 2, 3, 4}, 5, []int{0, 1, 2, 3, 4}},
		{"k > len(s) wraps via modulo", []int{0, 1, 2, 3, 4}, 12, []int{2, 3, 4, 0, 1}}, // 12 % 5 == 2
		{"negative k rotates right", []int{0, 1, 2, 3, 4}, -1, []int{4, 0, 1, 2, 3}},    // equivalent to left by 4
		{"empty slice", []int{}, 3, []int{}},
		{"single element, any k is a no-op", []int{42}, 9, []int{42}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := slices.Clone(tc.in)
			Left(got, tc.k)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Left(%v, %d) = %v, want %v", tc.in, tc.k, got, tc.want)
			}
		})
	}
}

// rotateLeftNaive is the antipattern this module warns against: it produces
// the identical permutation as Left by building a brand-new slice with
// slices.Concat(s[k:], s[:k]) instead of rotating in place. It is correct --
// every case in TestLeft's table would pass through it too -- but it
// allocates a fresh backing array on every call, exactly the steady
// allocator and GC pressure Left exists to avoid on a hot rebalance path.
// Never exported, never reachable from Left; it exists only so the tests can
// measure that difference.
func rotateLeftNaive(s []int, k int) []int {
	n := len(s)
	if n == 0 {
		return slices.Clone(s)
	}
	k = ((k % n) + n) % n
	return slices.Concat(s[k:], s[:k])
}

// TestNaiveAgreesWithLeft pins that the antipattern is not a correctness bug
// -- it produces the same result as Left on every case -- only a cost one,
// which TestNaiveAllocatesAndLeftDoesNot measures directly.
func TestNaiveAgreesWithLeft(t *testing.T) {
	t.Parallel()

	s := []int{0, 1, 2, 3, 4, 5, 6, 7}
	for _, k := range []int{0, 3, 8, 11, -2} {
		want := slices.Clone(s)
		Left(want, k)
		if got := rotateLeftNaive(s, k); !slices.Equal(got, want) {
			t.Fatalf("rotateLeftNaive(s, %d) = %v, want %v", k, got, want)
		}
	}
}

// TestNaiveAllocatesAndLeftDoesNot is the heart of the module: it proves,
// rather than assumes, that Left is genuinely O(1) extra space.
// testing.AllocsPerRun needs exclusive control of GC accounting and panics
// if called from a parallel subtest, so this test does not call t.Parallel.
func TestNaiveAllocatesAndLeftDoesNot(t *testing.T) {
	s := make([]int, 1000)
	for i := range s {
		s[i] = i
	}

	inPlace := testing.AllocsPerRun(100, func() {
		Left(s, 37)
	})
	if inPlace != 0 {
		t.Fatalf("Left: got %v allocations per run, want 0", inPlace)
	}

	naive := testing.AllocsPerRun(100, func() {
		_ = rotateLeftNaive(s, 37)
	})
	if naive == 0 {
		t.Fatalf("rotateLeftNaive: got 0 allocations per run, want at least 1")
	}
}

// ExampleLeft is the runnable demonstration of this module: go test executes
// it and compares its stdout against the Output comment below.
func ExampleLeft() {
	ring := []int{0, 1, 2, 3, 4, 5, 6, 7}
	fmt.Println("before:", ring)

	Left(ring, 3)
	fmt.Println("after rotating left by 3:", ring)

	Left(ring, -3)
	fmt.Println("after rotating back right by 3:", ring)

	// Output:
	// before: [0 1 2 3 4 5 6 7]
	// after rotating left by 3: [3 4 5 6 7 0 1 2]
	// after rotating back right by 3: [0 1 2 3 4 5 6 7]
}
```

## Review

`Left` is correct when it produces the same permutation as the allocating
baseline on every case in the table -- including the two no-op shapes
(`k == 0` and `k == len(s)`) that an off-by-one in the modulo could easily
get wrong -- and it is worth its added complexity only if it genuinely
allocates nothing, which `TestNaiveAllocatesAndLeftDoesNot` measures rather
than assumes. The trap this module is built around is trusting a claim like
"in-place, no allocation" without verifying it: an easy mistake, such as
building an intermediate slice via `append` to a nil slice before reversing,
would silently reintroduce an allocation while still producing the right
answer, and only a real `AllocsPerRun` check catches it. The allocating
baseline itself is never part of the package API -- it is not a `Strategy`
or a `Mode`, it is a private test helper that exists to be measured against,
because the whole point of this module is that the reader takes `Left` into
another project, never the naive version.

A second, quieter trap worth naming: `reverse` itself must operate on a real
sub-slice, `s[:k]` and `s[k:]`, not on a copy of one. If a future edit
changed either call site to pass `slices.Clone(s[:k])` "to be safe," the
function would still compile, the swaps inside `reverse` would still run
correctly against the copy, and every test in this file that only checks the
*final* contents of `s` after all three reversals would keep passing --
except `s` itself would never actually be mutated, because the two-index
sub-slice is what makes the swaps visible through `s`'s own backing array.
That is the aliasing discipline this whole lesson keeps returning to: a
sub-slice is a view, and here that view is not an implementation detail, it
is the entire mechanism the in-place claim depends on.

Reach for `Left` when a fixed-size collection needs to shift by a known
offset on a path that runs often enough for an allocation to matter,
and reach for `slices.Concat(s[k:], s[:k])` directly when the rotation is
rare, when the caller wants a value it can hand off without worrying about
who else holds a reference to the original, or when readability at the call
site outweighs the allocation. Both are legitimate; this module exists
because the choice between them should be deliberate, not an accident of
which one an engineer typed first.

For readers coming from a language whose array slicing sugar defaults to
copy semantics, it is worth restating plainly what makes the in-place
version possible at all: Go slices are already views over a shared backing
array, so mutating through one is the ordinary case, not a special unsafe
escape hatch reached for only in performance-critical code. `Left` is not
doing anything exotic -- it is using the language's default sharing
behavior on purpose, in exactly the place that sharing is wanted. Run
`go test -count=1 -race ./...`.

## Resources

- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — the two-index sub-slices `s[:k]` and `s[k:]` each reversal pass operates on.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — the function this module uses to prove, not assert, the zero-allocation claim.
- [`slices.Concat`](https://pkg.go.dev/slices#Concat) — the allocating baseline the test file contrasts `Left` against.
- [Programming Pearls, "The Rotate Problem"](https://en.wikipedia.org/wiki/Block_swap_algorithms) — the classical block-swap and reversal algorithms for in-place rotation.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-rabin-rolling-window-chunking.md](15-rabin-rolling-window-chunking.md) | Next: [17-ring-wraparound-two-subslices-writev.md](17-ring-wraparound-two-subslices-writev.md)
