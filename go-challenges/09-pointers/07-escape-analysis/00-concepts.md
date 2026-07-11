# Escape Analysis: Controlling Stack vs Heap Allocation in Hot Backend Paths — Concepts

Every value a Go program creates lives either on a goroutine's stack or on the
shared heap, and the compiler — not you — decides which. That decision is called
escape analysis, and on a request hot path it is one of the highest-leverage
performance levers a backend engineer has. A logger, a JSON serializer, or a
rate-limiter key that quietly allocates once per request turns into millions of
heap objects per second under load: more allocation latency, a larger live set
for the garbage collector to scan, more mark-assist stealing CPU from your
handlers, and a p99 that creeps up as traffic scales. This file is the
conceptual foundation for the nine independent exercises that follow. Read it
once and you have the model you need to reason through each one: what escapes,
why, how to read the compiler's verdict, and how to lock a good result behind an
executable guard so a refactor cannot silently undo it.

## Concepts

### What escape analysis actually proves

Escape analysis is a compile-time interprocedural dataflow analysis. For each
value the compiler asks a single question: can I prove that this value's lifetime
is bounded by the stack frame that created it? If it can prove yes, the value is
stack-allocated. If it cannot — and note the asymmetry, the rule is "cannot
prove non-escape", not "must escape" — the compiler conservatively puts the value
on the heap, because a stack slot that outlived its frame would be a
use-after-free. The compiler is allowed to be pessimistic; correctness beats
optimality. This is why a value can escape for reasons that feel indirect: the
analysis simply ran out of proof, often because a pointer to the value flowed
through a function boundary the analyzer could not see through.

### Why the stack is nearly free and the heap is not

Stack allocation is a pointer bump: to make room for a frame the runtime moves
the stack pointer, and on return it moves it back. Nothing is tracked, nothing is
scanned, the garbage collector never sees it. Heap allocation costs the
allocation itself (a trip through the size-classed allocator, possibly a growth)
plus ongoing GC work: every live heap pointer is scanned and marked on each
cycle, and the more you allocate between cycles the sooner the next cycle fires.
So reducing escapes lowers two different costs at once — the per-allocation
latency on the hot path, and the steady-state GC CPU that the whole process pays.
This is the mechanism behind the container economics: the GC targets a heap of
`live + live*GOGC/100`, and `GOMEMLIMIT` puts a soft ceiling on it; cutting the
live set shifts that entire curve down and buys you headroom before an OOM or a
GOGC-driven CPU spike.

### The canonical escape triggers

A handful of patterns account for almost every escape you will meet:

- Returning `&local` — a pointer to a function-local variable. The value must
  survive the return, so it moves to the heap.
- Storing a pointer into something that itself outlives the frame: a heap object,
  a global, a slice, a map, or a channel.
- Assigning a concrete non-pointer value to an interface (`any`, `error`,
  `io.Writer`, `...any`). This "boxing" typically escapes — the single most
  common surprise on logging and formatting paths.
- Capturing a variable in a closure that outlives the enclosing frame — most
  often a closure launched on a goroutine.
- A value whose size is not known at compile time (a slice grown to a dynamic
  length; a `[]byte` of runtime-determined size).
- Transitive reachability: if anything a value points to escapes, the value tends
  to escape too. The compiler annotates these as "leaking param": a parameter
  whose pointer flows to a result or to the heap.

### Interface satisfaction is a hidden allocator

This one deserves its own heading because it catches senior engineers. Assigning
a non-pointer concrete value to an interface boxes it: the runtime needs a place
to store the concrete data plus a type word, and that place is usually the heap.
So `var x any = someStruct` allocates, `fmt.Sprintf("%v", n)` allocates for each
scalar it reflects over, and a variadic `func Log(fields ...any)` allocates for
the backing array and for every non-pointer argument it boxes. One nuance that
prevents over-correcting: the runtime keeps a small static table for tiny
integer values, so boxing a constant like `42` may not allocate while boxing a
large or runtime-computed int does. Never reason from a single micro-example;
measure. The practical rule for hot paths is to prefer typed field builders
(`slog.Int`, `slog.String`) or preformatted keys over `...any` and `fmt`.

### Reading the compiler: gcflags -m

The compiler will tell you what it decided. `go build -gcflags=-m ./pkg` prints
one line per decision; `-m=2` and `-m=3` add reasoning depth. The lines you learn
to grep for:

```text
moved to heap: x        a named local escaped
x escapes to heap       an expression's value escaped
x does not escape       stayed on the stack
leaking param: p        a parameter's pointer flows out (transitive escape)
inlining call to f      f was inlined into its caller
```

Two operational cautions. First, `-m` reports on a package, so run it as
`go build -gcflags=-m ./yourpkg`, not on a bare file. Second, inlining interacts
with escape analysis: when `f` is inlined into its caller, the reported escape
behavior is the caller's, which can hide or reveal an escape that `f` has in
isolation. To pin a function's true behavior for study, add `//go:noinline` so
the analyzer treats it as a real call boundary. Several exercises here do exactly
that.

### Decisions are an implementation detail — pin them, don't hard-code them

Where a value lives is not part of Go's specification; it can and does change
between compiler releases and with innocuous refactors. So never encode "this is
stack-allocated" into your program's logic, and never trust a comment that claims
it. Instead, convert the claim into an executable regression guard.
`testing.AllocsPerRun(runs, f)` returns the average number of heap allocations
per call of `f`; a benchmark with `b.ReportAllocs()` (and `for b.Loop()` in
Go 1.24+) reports `allocs/op` and `B/op`. A test that asserts
`AllocsPerRun(1000, buildKey) == 0` fails in CI the moment a refactor introduces
an escape — which is far better than discovering it in a production flame graph
three weeks later. This is the throughline of the whole lesson: read
`-gcflags=-m` to understand, then lock the result with an alloc guard so the
understanding survives.

### sync.Pool amortizes the escapes you cannot remove

Some objects genuinely must outlive the call that creates them — a `bytes.Buffer`
that a JSON encoder writes into, a scratch structure reused across requests.
`sync.Pool` recycles such objects across calls so the allocation and GC cost is
paid once and amortized, not paid per request. The discipline it demands is
strict: reset the object on `Get` (its previous contents are arbitrary), return
it on `Put` (typically `defer pool.Put(x)`), and — the classic corruption bug —
never retain a reference to the pooled memory after `Put`. If you hand a caller a
slice that aliases the buffer's backing array and then `Put` the buffer, another
goroutine can `Get` it and overwrite those bytes underneath your caller. Copy the
bytes out before returning the object to the pool.

### Value vs pointer is a trade-off, not a rule

"Pass a pointer to avoid copying" is folklore, not law. A small struct copies
cheaply and stays on the stack; taking its address to "save the copy" can instead
force a heap escape that costs more than the copy ever would. A large struct is
expensive to copy on every call, and there a pointer genuinely helps — unless the
pointer leaks and escapes, in which case you have traded a stack copy for a heap
allocation plus GC pressure. There is a real crossover that depends on the struct
size and the call frequency, and the only way to know which side you are on is to
measure both with `-benchmem`. Return a value from a constructor unless the value
must be shared or mutated through the pointer.

### Preallocation defeats size-driven escapes

When you `append` to a `nil` slice in a loop, the runtime grows the backing array
several times — each growth is a heap allocation, and because the final size was
unknown the intermediate arrays escape. If you know (or can estimate) the final
length, `make([]T, 0, n)` collapses those N growth allocations into one.
`strings.Builder.Grow(n)` does the same for string assembly. On a path that maps
a known number of DB rows to DTOs, a single capacity hint turns a dozen
allocations per call into one.

### The senior backend angle

Allocation on request hot paths is a top driver of tail latency and GC CPU in Go
services. Every escape adds to the live set the GC must scan; GC pauses and
mark-assist steal CPU from request handling precisely when you are busiest. A
senior engineer treats escape analysis as a production tuning tool: use
`-gcflags=-m` to find where a logger, serializer, or key builder leaks to the
heap; close the gap with value returns, typed fields, `sync.Pool`, and
preallocated capacity; and lock the result behind `AllocsPerRun`/`-benchmem`
guards so CI catches regressions. This connects directly to container economics
(`GOGC`/`GOMEMLIMIT` tuning, OOM avoidance) and to the daily reality of keeping
p99 flat as traffic scales — where the difference between a zero-alloc and a
per-request-alloc rate-limiter key is measured in cores and dollars.

## Common Mistakes

### Returning *T from a constructor "to avoid a copy"

Wrong: returning `*Entry` for every log line so the caller "does not copy the
struct". The pointer forces a heap allocation that is more expensive than the
value copy it was meant to save, and it adds a GC-scannable object per call.

Fix: return the value. Return `*T` only when the value must be shared across
callers or mutated through the pointer. `buildStack` (returns `Entry`) beats
`buildHeap` (returns `*Entry`) on a hot path where the caller only reads.

### Treating "moved to heap" as a bug

Wrong: seeing `moved to heap` in `-gcflags=-m` and assuming the code is broken.
Some values legitimately outlive their frame — shared state, pool objects,
closures that run on goroutines. Zero heap everywhere is neither achievable nor
the goal.

Fix: the goal is fewer unnecessary escapes on the hot path, guided by pprof, not
a heap of zero. Read the annotation and decide whether that value truly needs to
outlive the frame.

### Passing scalars through ...any / fmt on a per-request path

Wrong: `log.Info("charged", "cents", cents, "ok", ok)` through a `...any` sink,
or `fmt.Sprintf` on a metrics label, then being surprised by allocation growth
under load. Each boxed non-pointer scalar escapes.

Fix: use typed field builders (`slog.Int`, `slog.String`) or preformatted keys,
and keep `fmt` off the hot path. Exercise 3 measures the gap directly.

### Retaining pooled memory after Put

Wrong: returning `buf.Bytes()` to a caller and then `pool.Put(buf)`. When another
goroutine `Get`s and reuses that buffer, your caller's bytes are silently
overwritten — a data-corruption bug that only appears under concurrency.

Fix: copy the bytes into a fresh slice before `Put`, or write directly to the
`http.ResponseWriter` before returning the buffer to the pool.

### Hard-coding an assumption about where a value lives

Wrong: a comment saying "this stays on the stack" and code that quietly depends on
it, with nothing to catch the day a Go upgrade or a refactor moves it to the heap.

Fix: verify with `-gcflags=-m`, then pin the behavior with an `AllocsPerRun` or
`-benchmem` guard so a regression fails CI instead of being eyeballed.

### Misreading -gcflags=-m output distorted by inlining

Wrong: concluding a function does not escape when the annotation you read was for
the inlined copy in a specific caller, or running `-m` on a bare file.

Fix: run `-m` on the package (`go build ./pkg`), and add `//go:noinline` to
isolate a function's true escape behavior when studying it.

### Appending to a nil slice with a known final size

Wrong: `var out []DTO; for ... { out = append(out, ...) }` when the row count is
known, paying repeated growth reallocations.

Fix: `out := make([]DTO, 0, len(rows))`. One allocation instead of a dozen.

### Micro-optimizing a cold path

Wrong: eliminating an allocation in initialization code that runs once at startup
while the real hot path — revealed by pprof — allocates per request untouched.

Fix: profile first. Escape reduction pays off only where the allocation rate is
high. Spend the effort where the flame graph is wide.

Next: [01-log-pipeline-stack-vs-heap.md](01-log-pipeline-stack-vs-heap.md)
