# Range Over Integers — Concepts

Go 1.22 added the simplest member of the new range family: ranging directly over an integer. `for i := range n` runs the loop body `n` times and binds `i` to the values `0` through `n-1`. There is no init clause, no condition, and no post statement to get wrong. This file is the conceptual foundation for the exercises, which build a counter package, a grid-and-fixture builder, and a retry-and-closure helper as independent, self-contained Go modules. Read it once and the rest is application.

## Concepts

### What the form means and why it exists

A loop whose only job is "do this n times" has, in the classic three-clause form, three places to make a mistake: the start value, the comparison operator, and the increment. `for i := 0; i < n; i++` is correct; `for i := 1; i <= n; i++`, `for i := 0; i <= n; i++`, and `for i := 0; i < n; i--` are all the off-by-one or infinite-loop variants that the same keystrokes can produce. The integer range form collapses those three clauses into one expression: `for i := range n`. The bound is the whole statement, so the loop reads as its own intent. This is the same motivation that gave slices and maps a range form years earlier; Go 1.22 simply extended `range` to the one container that was missing, the count itself.

The iteration values are zero-based: `for i := range 5` yields `0, 1, 2, 3, 4`, never `5`, and never `1` through `5`. That matches indexing (`s[i]` for a length-`n` slice walks `0` to `n-1`) and matches `for i := range someSlice`, so the two forms compose without a mental gear change. When the domain is one-based, the body uses `i+1`; the iterator itself stays zero-based.

### The iteration value is optional

When the loop body never needs the count, drop the variable entirely: `for range n`. This is the honest form for pure repetition — writing a string `n` times, retrying an operation `n` times, pushing `n` items onto a queue. It removes an unused identifier that `go vet` and readers would otherwise have to account for, and it states plainly that the iteration number carries no meaning. Reaching for `for i := 0; i < n; i++` when `i` is never read is the most common thing this form replaces.

### Bounds, types, and the zero-or-negative case

Three properties of the bound expression are worth holding precisely, because each one is a question a reader eventually asks.

First, the bound is evaluated exactly once, before the first iteration. Assigning to the bound variable inside the body does not change how many times the loop runs; `n := 3; for range n { n = 100 }` runs three times. This mirrors the three-clause loop's condition only being meaningful per-iteration if you write it that way, and it means the count is fixed the instant the loop begins.

Second, the loop variable takes the type of the bound. Range over an `int` and `i` is an `int`; range over a `uint8` and `i` is a `uint8`. Only integer types are allowed — there is no float or string form of this construct. An untyped constant such as `range 5` gives `i` the default type `int`.

Third, a zero or negative bound produces zero iterations, with no panic and no compile error. `for range 0` and `for range -3` simply never execute the body. This is convenient but also a trap for a library: a function that accepts a count and silently treats `-1` as "do nothing" cannot tell a caller's bug from a legitimate request for zero items. The counter exercise addresses this directly by validating the count at the API boundary and returning a sentinel error, rather than leaning on the loop's tolerance of negatives.

### Per-iteration scope and closures

Since Go 1.22 the loop variable in a `for ... range` statement has per-iteration scope: each pass through the loop gets a fresh variable rather than reusing one across all iterations. For the integer range form this matters the moment a closure captures `i`. A slice of functions built with `for i := range n { fns[i] = func() int { return i } }` produces closures that return `0, 1, 2, ...`, each capturing its own `i`. Under the pre-1.22 rules the same code produced closures that all returned `n-1`, because they shared a single variable whose final value outlived the loop. The closure exercise relies on this: it builds a pipeline of stages, each tagging its input with its own stage number, and the test asserts the numbers are distinct. (The semantic change itself, and the migration it required, is the subject of the next lesson; here it is simply the behavior you can count on.)

### Where the form earns its place

The integer range form is not only for toy counters. Three realistic patterns recur. Building grids and matrices: nested `for r := range rows` / `for c := range cols` walks a two-dimensional structure with no index arithmetic in the loop headers. Generating deterministic test fixtures: `for i := range n` produces `n` rows of seeded data with predictable IDs and names, so a test can assert exact values without a random source. Bounded retries and repeated side effects: `for range attempts` expresses "try this up to attempts times" without a counter the body might accidentally read or mutate. The exercises build one of each.

## Common Mistakes

### Expecting one-based values

Wrong: writing `for i := range 5` and expecting `i` to run `1, 2, 3, 4, 5`. It runs `0, 1, 2, 3, 4`. Use `i+1` in the body when the domain is one-based; the iterator stays zero-based to compose with indexing and slice ranges.

### Letting a negative count mean "do nothing"

Wrong: relying on `for range n` producing zero iterations so a public function "handles" a negative count for free. The loop is tolerant, but the API is now ambiguous: a caller's `-1` bug is indistinguishable from an intentional request for zero items. Validate the count at the boundary and return a typed error so `errors.Is` can distinguish invalid input from a valid empty result.

### Keeping the three-clause loop when the index is unused

Wrong: writing `for i := 0; i < n; i++` when the body never reads `i`. That keeps an unused variable and the whole off-by-one surface area for nothing. Write `for range n`; it says "repeat n times" and removes the index entirely.

### Assuming the bound is re-read each iteration

Wrong: mutating the bound variable inside the body and expecting the iteration count to change. The bound is evaluated once before the loop starts, so the count is fixed at entry. If you need a dynamic stop condition, that is a three-clause or condition-only `for`, not an integer range.

Next: [01-counter-package.md](01-counter-package.md)
