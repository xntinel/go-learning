# Efficient String Assembly for Backend Hot Paths — Concepts

String assembly is on the hot path of almost every backend service. A log line is
built for every request. A repository builds SQL text on every write. A `/metrics`
scrape rebuilds an exposition body every 15 seconds. A CSV export streams tens of
thousands of rows. An HMAC signer canonicalizes a query string on every inbound
call. An SSE endpoint frames each event before it hits the socket. None of these
is a demo of language syntax; each is a place where the wrong assembly idiom turns
into GC pressure and tail-latency spikes under load, and where a naive
"just concatenate the fields" turns into a correctness or security bug.

This file is the conceptual foundation for ten independent exercises. Each exercise
is a self-contained Go module you can build, run, and test on its own; read this
once and you have the model you need to reason through all of them.

## Concepts

### Why `+` in a loop is O(N^2)

Go strings are immutable. The statement `s = s + piece` cannot mutate `s` in place;
it allocates a brand-new backing array sized `len(s) + len(piece)`, copies all of
the already-accumulated bytes into it, then copies `piece`. Do that in a loop that
appends N pieces and the total copy work is `1 + 2 + 3 + ... + N`, which is
`O(N^2)` bytes moved and N separate allocations that immediately become garbage.

The Go compiler *does* optimize the fixed expression form `a + b + c + d`: it knows
all operands up front, sums their lengths once, allocates one result, and copies
each operand exactly once. That fusion applies only to a single concatenation
expression, never to the loop form `for ... { s += piece }`. The loop is the trap,
and it is exactly the shape a log-line or query-clause assembler takes.

### strings.Builder is an append-only []byte with a copy-free finish

`strings.Builder` wraps a single `[]byte`. `WriteString`/`WriteByte`/`Write` append
into it, growing the backing array with amortized `O(1)` doubling, so N appends cost
`O(N)` total copies instead of `O(N^2)`. The one clever part is `String()`: it hands
out the accumulated bytes as a `string` without copying them, using an internal
`unsafe` conversion that is sound precisely because a Builder is append-only and
never lets you mutate bytes you already wrote. So the steady-state allocation is the
growing buffer, not the final cast.

Two properties fall out of that design and matter in practice. A Builder must not be
copied after first use — it stores a pointer to itself to detect illegal copies, and
copying corrupts that check (this is what `go vet`'s copylocks analyzer catches).
And a Builder is write-only: there is no `Read`, no `Bytes()` you are meant to
mutate, no incremental draining. It exists to build one string and give it to you.

### Grow(n) collapses the reallocation cycles into one

`Grow(n)` guarantees room for `n` more bytes right now, allocating once if needed.
If you know the final size — the sum of the known field lengths plus an estimate for
the variable part — one `Grow` up front replaces the several doubling-and-copy
cycles the Builder would otherwise perform. Correct pre-sizing is the single biggest
Builder win. Crucially, `Grow` is a hint for *performance*, never for correctness:
under-estimating just means the Builder grows again on its own; over-estimating just
wastes a little memory. You cannot make the output wrong by getting `Grow` wrong.

### strings.Builder vs bytes.Buffer

They look similar and are not interchangeable. `strings.Builder` is write-only and
yields a `string` cheaply; it is the right tool when you build a value once and
return it. `bytes.Buffer` is read *and* write: it implements `io.Reader`,
`io.Writer`, exposes `Bytes()`, and can be drained incrementally. Reach for
`bytes.Buffer` when you need to read the bytes back, when you feed the result to an
API that wants `[]byte`, or — most importantly here — when you want to *pool and
reuse* the buffer across requests, because `Reset` returns it to empty while keeping
the capacity. A Builder cannot be safely copied or read incrementally, so it is a
poor fit for a pool that hands the same object to many callers.

### The strconv.Append* family: the tier below Builder

`strconv.AppendInt`, `AppendUint`, `AppendFloat`, and `AppendQuote` format a value
directly into a caller-owned `[]byte` and return the extended slice. Combined with
the built-in `append`, they let you serialize into a buffer you *reuse* across
calls, reaching a genuine steady state of 0 allocations per call once the backing
array is large enough. This is the tier below `strings.Builder`: more ergonomic cost
(you thread a `dst []byte` through and slice it back to `dst[:0]` each call), in
exchange for eliminating even the Builder's growth allocation on the hottest paths.
It also avoids the reflection and interface boxing that `fmt.Sprintf`/`Fprintf`
incur, which is the real reason `fmt` is slow in a tight loop.

### sync.Pool amortizes allocation but demands discipline

`sync.Pool` keeps a set of reusable objects (typically `*bytes.Buffer`) so a
high-throughput handler does not allocate a fresh buffer per request, relieving GC
pressure. It is correct only with two disciplines. First, the contents of a pooled
object are arbitrary — whatever the previous user left — so you must `Reset()` right
after `Get()`, never assume empty. Second, and easy to miss: a single huge request
can grow a buffer to megabytes, and if you `Put` it back the pool retains that giant
capacity indefinitely, a real memory leak dressed up as an optimization. The fix is
a capacity guard: before `Put`, drop buffers whose `Cap()` exceeds a threshold so
they are garbage-collected instead of pinned.

### Correctness beats cleverness for structured formats

The most expensive string-building bug is not slowness, it is a manual join that
silently corrupts structured output. Concatenating CSV fields with commas breaks the
instant a field contains a comma, a quote, or a newline. Building SQL by
interpolating values opens injection. Assembling a query string by hand mis-encodes
spaces and reserved characters and produces a different signature than the peer
computed. The rule: the builder assembles the *text* skeleton; the *values* go
through the right encoder. Use `encoding/csv` for CSV, parameter placeholders
(`$1`, `$2`) with the driver's args for SQL, and `net/url`'s `Values.Encode` /
`QueryEscape` for query strings. You still use a Builder — to hold the escaped
output — but you never do the escaping by hand.

### Stream into io.Writer when the sink is a socket

When the destination is a `net.Conn` or an `http.ResponseWriter`, you rarely want to
materialize the whole response in memory first. `fmt.Fprintf`, `io.WriteString`, and
`(*bytes.Buffer).WriteTo` all write straight into an `io.Writer`. Large exports and
streaming protocols (SSE, chunked responses) should write and `Flush` per
frame/chunk so the client sees data early and the server holds bounded memory. A
`strings.Builder` is still useful here to assemble one frame before the single write,
but the response as a whole is streamed, not buffered.

### Measurement is the deliverable

You do not get to *assert* that Builder beats `+`; you *measure* it and commit the
measurement as a regression detector. Go 1.24's `for b.Loop()` harness is the modern
idiom: it runs the body an appropriate number of times, automatically excludes the
setup that precedes the loop from the timing, and keeps the loop's results alive so
the optimizer cannot delete the work you are trying to measure. Pair it with
`b.ReportAllocs()` (or the `-benchmem` flag) to surface `allocs/op` and `B/op`.
Report ratios and allocation counts, which are stable across machines — not raw
`ns/op`, which is not. The benchmark's job is to catch the day someone reintroduces
a `+` loop and doubles the allocations.

### Concurrency and aliasing

A `strings.Builder` is not safe for concurrent use and must not be copied after
first use. The idiom is one fresh Builder per call or per goroutine, or a pooled
`bytes.Buffer` that you `Reset` on every `Get`. Passing or returning a Builder by
value is caught by `go vet` (copylocks) because the Builder embeds a
`noCopy`-style self-pointer; always pass `*strings.Builder`. Sharing one Builder or
Buffer across goroutines is a data race that `go test -race` will flag.

## Common Mistakes

### Pre-allocating with make([]byte, 0, n) then string(b)

Wrong: `b := make([]byte, 0, n); ...; return string(b)`. The final `string(b)`
conversion allocates a fresh array and copies every byte, so the pre-allocation
bought nothing. Fix: use `strings.Builder` with `Grow(n)` — its `String()` is the
single copy-free finish that `string(b)` is not.

### Copying a strings.Builder

Wrong: passing or returning a `strings.Builder` by value, which duplicates its
internal self-pointer and corrupts the copy-detection. `go vet` flags it. Fix:
always take and pass `*strings.Builder`.

### Sharing one Builder or Buffer across goroutines

Wrong: a package-level Builder written by many handlers concurrently — a data race.
Fix: one Builder per goroutine/request, or a `sync.Pool` of Buffers each `Reset` on
`Get`.

### Reusing a Builder or Buffer without Reset

Wrong: keeping a Buffer around and writing the next request's output on top of the
previous bytes. The old content bleeds into the new output. Fix: `Reset()` before
each reuse.

### Putting an unbounded buffer back into a sync.Pool

Wrong: `pool.Put(buf)` unconditionally after a request that grew `buf` to megabytes;
the pool now pins that capacity forever. Fix: guard on `buf.Cap()` and drop
oversized buffers before `Put`.

### Forgetting sync.Pool contents are arbitrary

Wrong: `buf := pool.Get().(*bytes.Buffer)` and immediately writing, assuming it is
empty. It holds whatever the last user left. Fix: `buf.Reset()` right after `Get()`.

### Manually joining CSV/SQL/query fields without escaping

Wrong: building CSV, SQL, or a query string with `+`/Builder and no escaping; it
corrupts on commas, quotes, newlines, or reserved characters, and for SQL it opens
injection. Fix: `encoding/csv`, parameter placeholders with driver args, and
`url.Values`/`QueryEscape`.

### Interpolating values into a placeholder-built SQL string

Wrong: writing a user value straight into the query text next to the placeholders.
Fix: the builder emits `$1`, `$2` only; every value travels in the args slice the
driver parameterizes.

### Micro-optimizing where it does not matter

Wrong: dropping to `strconv.Append*` for a cold, one-shot, small assembly. For those,
plain `+` or `fmt.Sprintf` is clearer and fast enough. Fix: profile or benchmark
first; reserve the append tier for proven hot paths.

### Using the old b.N scaffolding or omitting -benchmem

Wrong: `for i := 0; i < b.N; i++` with manual `ResetTimer`, and running without
`-benchmem`, so allocation regressions go unseen. Fix: `for b.Loop()` (Go 1.24+),
which resets the timer after setup and keeps results alive, plus
`ReportAllocs()`/`-benchmem`.

### Never calling String() / WriteTo / Bytes

Wrong: building into a Builder or Buffer and then discarding it without extracting
the result. The work is done and thrown away. Fix: return `b.String()` (or
`WriteTo`/`Bytes`).

### Using fmt in the tightest loops

Wrong: `fmt.Sprintf`/`Fprintf` inside a per-element hot loop, paying reflection and
interface-boxing overhead on every call. Fix: `strconv.Append*` into a reused buffer.

Next: [01-log-line-assembler-builder-vs-naive.md](01-log-line-assembler-builder-vs-naive.md)
