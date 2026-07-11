# Closure Gotchas: Loop-Variable Capture in Concurrent Backend Code

Loop-variable capture is not a language trivia question. It is the single most
common concurrency bug in production Go fan-out code: worker pools, `errgroup`
batches, per-request handler registration, timer-based retry and expiry
scheduling, graceful-shutdown cleanup stacks. Go 1.22 changed the language to
close one class of this bug, but it left two classes wide open that a senior
engineer must still catch in review. This file is the conceptual foundation for
nine independent, production-shaped exercises. Read it once and you have the
model you need to reason through every one of them.

## Concepts

### Closures capture variables, not values

A closure captures a *variable* — a reference to a storage location — not the
value that location held at the moment of capture. When the closure runs, it
reads whatever value the location holds *at execution time*. This one fact is
the root mechanism behind every gotcha in this lesson and behind every
legitimate use of a stateful closure. A counter closure works because the
`n` it increments is the same storage location on every call; a buggy fan-out
loop fails for exactly the same reason — every goroutine reads the same shared
location.

```go
n := 0
inc := func() int { n++; return n }
// inc() returns 1, then 2, then 3: it reads the live n, not a snapshot.
```

If you come from a language where lambdas snapshot their environment by value,
this is the mental model to unlearn. In Go, mutations to a captured variable
after capture are visible inside the closure — surprising in both the bug
direction and the feature direction.

### The historical bug (Go 1.21 and earlier)

Before Go 1.22, a `for i, v := range xs` loop reused ONE `i` and ONE `v` for the
entire loop. All N closures that captured them aliased the same two locations, so
after the loop every closure saw the final iteration's values:

```go
for _, tag := range tags {
	go func() {
		process(tag) // pre-1.22: every goroutine sees the LAST tag
	}()
}
```

The historical fix was to introduce a fresh per-iteration copy — `tag := tag` at
the top of the loop body — or to pass `tag` as a goroutine argument. You will
still see `tag := tag` in older codebases; it was correct then.

### The Go 1.22 language change

Since Go 1.22, each iteration of a `for` range loop AND a three-clause `for`
loop gets its OWN instance of the loop variable(s). The `tag := tag` shadow is
now redundant when the module's `go.mod` declares `go 1.22` or later. Crucially,
this behavior is gated by the *module's* declared Go version, not by the
toolchain running the build: the same source is compiled with per-iteration
variables in a `go 1.22` module and with the old shared variable in a `go 1.21`
module, even under the exact same `go` binary. Behavior is per-module. In a
monorepo where different modules pin different `go.mod` versions, the same
closure can be correct in one package and buggy in another, so "it compiles and
passes on my machine" is not a safety argument.

### What Go 1.22 did NOT fix

The per-iteration variable fixes *value reads*. It does not fix three things a
senior must still catch:

1. Writing to a shared slot indexed by the loop variable. If several goroutines
   share one result slice and each writes `results[i]`, the per-iteration copy
   of `i` fixes the read of `i`'s value, but if the goroutines genuinely share a
   single index variable (a classic `for i := 0; ...` counter passed by capture
   into a helper) they can still write the wrong slot or race. The robust pattern
   is to pass the index as an argument and give each goroutine its own slot.
2. Unbounded goroutine creation. One goroutine per element over a DB-sized or
   attacker-sized slice is correct capture and a resource-exhaustion outage.
3. `defer` accumulation inside a loop. Covered below; the compiler version does
   nothing for it.

### Passing the value as a call argument is version-independent

`go func(v T){ ... }(v)` evaluates `v` at the `go` statement and copies it into
the goroutine's own parameter. This is correct on *every* Go version, reads
top-to-bottom, and removes all ambiguity about what each goroutine sees. In a
mixed-version monorepo it is the pattern that does not depend on which module's
`go.mod` version the reviewer happens to be looking at. Prefer it for anything
that escapes the loop: goroutines, `time.AfterFunc` callbacks, appended-to-a-
slice closures, deferred cleanups collected for later.

### defer in a loop: two separate timing rules

`defer` has two timing rules that both bite inside a loop, and they are
independent:

- Deferred *arguments* are evaluated when the `defer` statement executes.
- The deferred *function body* (and any variables its closure captures) runs at
  enclosing-function return — NOT at the end of the loop iteration.

Deferred calls also run LIFO (last-in, first-out). So this leaks:

```go
for _, name := range files {
	f, _ := os.Open(name)
	defer f.Close() // runs only when the WHOLE function returns, in reverse order
}
```

Every file stays open until the function returns, and they close in reverse
order — a file-descriptor or connection leak in any long loop. The fix is a
per-iteration helper whose own return scopes the defer:

```go
for _, name := range files {
	func() {
		f, _ := os.Open(name)
		defer f.Close() // runs when THIS closure returns, i.e. each iteration
		process(f)
	}()
}
```

### Intended capture-by-reference: the feature is the bug's twin

Stateful closures — sequence generators, token-bucket limiters, memoizers,
accumulators — rely on the SAME captured variable persisting across calls. The
gotcha and the feature are one mechanism. A senior knows which one they are
writing: shared mutable capture that stays inside one goroutine is a design;
shared mutable capture that escapes to several goroutines is a data race unless
the state is guarded by a mutex or an atomic. When you build one limiter per
tenant in a loop, each tenant must capture its OWN state cell — otherwise every
tenant shares one bucket, which is the same loop-capture failure wearing a
business-logic costume.

### Heap escape

Any variable captured by a closure that outlives its enclosing frame — a
goroutine, a returned func, a stored callback — escapes to the heap. Escape
analysis (`go build -gcflags=-m`) shows this. A per-iteration variable that
escapes allocates once per iteration; usually negligible, occasionally relevant
in a hot fan-out loop. It is not a correctness issue, but it explains why the
"one variable per iteration" change is not free and why tight loops sometimes
pass a value by argument to keep the closure non-escaping.

### errgroup fan-out semantics

`errgroup.WithContext(ctx)` returns a `*Group` and a derived context that is
cancelled when the first goroutine returns a non-nil error OR when `Wait`
returns. `Group.SetLimit(n)` bounds concurrency: `Go` blocks until a slot frees,
which is how you cap fan-out. `Group.Wait` blocks until all started goroutines
finish and returns the first non-nil error. The closures you pass to `Go` must
not alias a shared loop index for their result slot; give each its own slot and
pass the index by argument.

### Tooling and its limits

`go vet` includes the `loopclosure` analyzer, which flags loop variables
captured by a goroutine or `defer` in the last statement of a loop body. `go
test -race` catches actual data races at runtime. Neither is complete: `vet`
only inspects specific syntactic shapes and honors the module's Go version, so
it silently passes many real cases (including all the intended-capture ones and
anything not in the final statement); `-race` only reports races that actually
occur in the schedule the test happened to exercise. Review discipline plus a
deterministic `-race` test as the acceptance gate remain necessary. Every
exercise here ships a `-race`-clean test as its gate for exactly this reason.

## Common Mistakes

### Adding a redundant `v := v` shadow in a 1.22+ module

Wrong: `for _, tag := range tags { tag := tag; go func(){ use(tag) }() }` when
`go.mod` declares 1.22 or later. The shadow is dead code that signals the author
does not know the module's language semantics.

Fix: drop it. Rely on the per-iteration variable, or pass `tag` as an argument
for a version-independent read.

### Writing `results[i]` from goroutines that share the index

Wrong: assuming Go 1.22 made a shared-slot write pattern safe. A per-iteration
variable fixes value reads, but reusing one accumulator or one shared index
across goroutines still races.

Fix: pass the index by argument and give each goroutine its own slot:
`go func(i int){ results[i] = compute() }(i)`.

### Expecting per-iteration cleanup from `defer` in a loop

Wrong: `for _, x := range xs { r := open(x); defer r.Close() }` and expecting
each `r` to close at the end of its iteration. The deferred body runs at
function return, so all resources stay open until then and close in reverse.

Fix: scope the defer in a per-iteration helper function.

### Unbounded goroutines over an external-sized slice

Wrong: `for _, id := range ids { go fetch(id) }` where `ids` comes from a DB or a
request. Correct capture, resource-exhaustion outage.

Fix: bound it with `errgroup.SetLimit(n)` or a semaphore channel.

### Timer callbacks capturing a shared item

Wrong: `time.AfterFunc(d, func(){ evict(item) })` inside a loop on a pre-1.22
module, or capturing a shared index — every timer fires against the last item.

Fix: bind the item explicitly (per-iteration variable on 1.22+, or a helper that
takes the item as a parameter) and keep each `*Timer` so you can `Stop` it on
shutdown.

### Assuming closures snapshot values at capture time

Wrong: a mental model from other languages where lambdas copy their environment.
Go captures the variable, so later mutations are visible.

Fix: internalize capture-by-reference; when you want a snapshot, make a copy or
pass by argument.

### Sharing a captured map or counter across goroutines without a guard

Wrong: an intended stateful closure (memoizer, limiter) whose captured map or
counter is touched by several goroutines with no mutex or atomic. That turns the
feature into a data race.

Fix: guard the captured state with a `sync.Mutex` or `sync/atomic`.

### Trusting `go vet` alone

Wrong: shipping on a clean `go vet` and assuming loop-capture is ruled out. `vet`
inspects only specific final-statement shapes and honors the module version.

Fix: ship a `-race` test as the gate; treat `vet` as a cheap first filter, not
proof.

### Capturing one CancelFunc for the whole shutdown stack

Wrong: building a teardown stack by capturing the loop variable that holds a
`context.CancelFunc`, so every stack entry cancels the same last resource.

Fix: append the value as an argument (`stack = append(stack, cancel)`), and tear
down by iterating the collected slice in reverse.

Next: [01-worker-fanout-tag-capture.md](01-worker-fanout-tag-capture.md)
