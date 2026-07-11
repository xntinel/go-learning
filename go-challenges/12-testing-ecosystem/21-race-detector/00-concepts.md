# The Race Detector: Finding, Reading, and Fixing Data Races in Backend Code

A senior backend engineer does not run `go test -race` and hope it passes. They
treat the race detector as a diagnostic instrument with a precisely known
contract: it is a runtime, happens-before detector built on ThreadSanitizer, so
it reports a race only on code paths and interleavings that actually execute.
Green under `-race` means "no race on the interleavings I exercised," never
"provably race-free." Everything in this chapter follows from that one fact.
This file is the conceptual foundation. Read it once and you have what you need
to reason through each of the nine independent exercises that follow, every one
of which is a real server artifact -- a metrics counter, an in-memory cache, a
feature-flag store, a lazy connection pool, a hot-reloaded config, a worker
pool, a rate limiter, an HTTP handler, a CI race gate -- never a syntax toy.

## What a data race precisely is

The Go memory model gives the exact definition, and it is worth memorizing
because every fix in this chapter is aimed at satisfying it. A data race occurs
when two goroutines access the same memory location, at least one of those
accesses is a write, and there is no happens-before edge ordering them. All
three clauses matter. Two concurrent reads are not a race. Two accesses ordered
by a synchronization edge -- a mutex `Unlock` that happens-before a later
`Lock`, a channel send that happens-before the corresponding receive, an atomic
`Store` that happens-before an atomic `Load` that observes it, a `WaitGroup`
`Done` that happens-before the `Wait` it releases -- are not a race even when one
is a write. A data race is exactly the absence of such an edge between a write
and another access.

Distinguish a data race from a race condition. A race condition is a logic bug
where the outcome depends on timing (two goroutines both read a balance, both
decide to withdraw, and the account goes negative even though every individual
access was properly synchronized). A data race is narrower and lower-level: a
memory-model violation. The detector targets data races. It will not catch a
lost-update race condition that is fully synchronized but logically wrong -- that
is a design bug you find with reasoning and integration tests, not with
`-race`. Keeping these two apart is the difference between "the detector is
green so we are safe" (false) and "the detector is green so we have no
memory-model violations on the paths we ran" (true).

## A data race is undefined behavior, not merely nondeterministic

The single most expensive misconception is treating a racy read as "returns a
possibly-stale value." In Go a data race is undefined behavior. The compiler is
free to reorder the racy accesses, keep a value in a register, or split a
multi-word store into two instructions; the hardware is free to reorder and to
tear a wide store. A racing read of an interface value, a slice header, a string
header, or a `map` can therefore observe a half-updated multi-word value -- a
slice pointer from one assignment with a length from another -- and dereferencing
that torn header does not return stale data, it crashes. "It works in practice on
my laptop" is not a defense: the next compiler version, a different `GOMAXPROCS`,
or a different CPU can tear the value or reorder the accesses. A race report is a
bug to fix, never a warning to silence.

Concurrent access to a built-in `map` is a special, sharpened case. The runtime
has explicit concurrent-write detection and will `throw("concurrent map writes")`
-- an unrecoverable fatal crash that no `recover` can catch -- independent of the
race detector. So a racing map is doubly dangerous: undefined behavior at the
memory-model level, plus a runtime that may deliberately abort the process.

## How the detector actually works

`go test -race`, `go build -race`, and `go run -race` compile the program with
ThreadSanitizer instrumentation. Every memory access is instrumented to update a
per-location shadow state, and every synchronization event -- mutex lock/unlock,
channel send/receive, atomic operation, `WaitGroup`, goroutine start -- updates a
vector clock that encodes the happens-before relation. When an access to a
location is not ordered (by the vector clocks) after the previous access to that
same location, and at least one is a write, the detector prints a report.

Two properties fall directly out of this design. First, it is a dynamic
detector: it observes only the accesses that actually run, so it can only find a
race on an interleaving that actually happens during the run. A rarely-taken
branch, a code path no test exercises, or an interleaving the scheduler never
chose in this run can still race and go unreported. Second, it has essentially
no false positives: if it reports a race, there genuinely was an unordered
write, because it is reasoning about the real synchronization events the program
executed. So the asymmetry is: a report is (almost) always a true bug; the
absence of a report is only as strong as your coverage of paths and
interleavings.

## Reading a race report

The core skill this chapter drills is reading the report, not merely seeing that
a test failed. A `WARNING: DATA RACE` block has a fixed shape:

```text
WARNING: DATA RACE
Write at 0x00c0000b4010 by goroutine 8:
  example.com/cache.(*Cache).Set()
      /path/cache.go:23 +0x64
Previous read at 0x00c0000b4010 by goroutine 7:
  example.com/cache.(*Cache).Get()
      /path/cache.go:31 +0x8c
Goroutine 8 (running) created at:
  main.main()
      /path/main.go:14 +0x120
Goroutine 7 (running) created at:
  main.main()
      /path/main.go:13 +0x108
```

Read it top to bottom. The first stack is the current conflicting access (here a
write in `Set`), with the exact file:line. The second is the previous access to
the same address (a read in `Get`) -- the two are the pair that is unordered.
The address `0x...` is the same in both, confirming it is one location. The two
`Goroutine N ... created at` stacks tell you where each racing goroutine was
launched, which is often where the missing synchronization should have been.
When present, an allocation stack tells you where the racing variable was
allocated. The diagnostic question is always the same: what happens-before edge
should have ordered these two accesses, and why is it missing?

## Choosing the fix by access pattern

This is the senior judgment the chapter is built around. There is no single
"make it thread-safe" answer; the right tool is a function of the access
pattern, and each has a distinct cost profile.

- A single machine word updated independently (a counter, a flag, a gauge) maps
  to `sync/atomic` -- `atomic.Int64`, `atomic.Bool`, `atomic.Pointer[T]`. One
  atomic op is one indivisible read-modify-write; no critical section.
- A read-heavy shared structure (a config map, a routing table) read by many
  goroutines and written rarely maps to `sync.RWMutex`: many concurrent
  `RLock` readers, exclusive `Lock` for the infrequent writer.
- A one-time, check-then-act initialization (a lazy singleton pool) maps to
  `sync.Once`, which guarantees exactly one initialization and publishes its
  result to every caller with the right happens-before edge.
- A whole object that is read-mostly and hot-swapped wholesale maps to
  copy-on-write with `atomic.Pointer[T]`: readers `Load` a pointer to an
  immutable snapshot with zero locking, the writer builds a fresh value and
  `Store`s it.
- The strongest option is to eliminate the sharing entirely: share by
  communicating. Instead of many goroutines writing one shared collection under a
  lock, each sends its result on a channel and a single collector owns the
  aggregate. No shared mutable state means no data race to guard.

The trade-offs: atomics are the cheapest but only cover a single operation.
`RWMutex` scales reads but its own bookkeeping is not free and a writer starves
readers. `sync.Once` is perfect for init and useless for ongoing mutation.
Copy-on-write gives lock-free reads at the cost of allocating a new snapshot per
write, so it wins only when reads vastly outnumber writes. Channels remove the
race by construction but add scheduling and back-pressure to reason about.

## atomic is atomicity of one operation, not of a compound invariant

The trap that produces "atomic but still wrong" code: making each of two fields
atomic does not make a two-field update atomic. A token-bucket limiter holds a
token count and a last-refill timestamp; a correct `Allow` must read the clock,
refill, and consume as one indivisible step. If the count is one atomic and the
timestamp is another, two goroutines can interleave between the two atomics and
both refill, or consume against a stale count -- the per-field atomicity is real
but the invariant spanning both fields is violated. Compound state that must
change together needs a single critical section (one `sync.Mutex`), not several
independent atomics. Recognizing compound state is the skill that prevents the
subtlest concurrency bugs.

## Copy-on-write and the memory model

Copy-on-write with `atomic.Pointer[T]` is worth understanding precisely because
its correctness rests on the memory model, not on luck. The writer allocates a
brand-new, never-again-mutated `*Config`, fully initializes it, then calls
`ptr.Store(newCfg)`. A reader calls `ptr.Load()`. The atomic `Store` and the
`Load` that observes it form a release/acquire edge: everything the writer wrote
to the new struct before the `Store` happens-before everything the reader does
after the `Load`. So a reader either sees the entire old snapshot or the entire
new one, never a half-built struct, and it never needs a lock. The absolute rule
is that a snapshot is immutable once published: after `Store`, no one mutates
the pointed-to struct. A new generation is a new allocation. Mutating a field of
an already-published shared `*Config` in place is exactly the race copy-on-write
exists to avoid.

## Maximizing detection in tests

Because the detector cannot find a race on a path it never runs, designing tests
that exercise concurrency is part of the job. Three levers widen coverage.
Concurrent hammering: launch many goroutines that pound the shared object
simultaneously so a racing interleaving actually occurs. `-count=N`: re-run the
test N times so the scheduler explores different interleavings across runs; a
race that appears in one run out of twenty is caught by `-count=20`. Realistic
load: drive an `httptest.Server` with N concurrent clients so the race surfaces
on the real request path, not a synthetic one. A single-goroutine test under
`-race` proves almost nothing about concurrency; the test has to create the
contention.

## Operating the detector: cost and boundaries

The overhead is why `-race` is a CI and load-test flag, never a production build
flag: roughly 2-20x CPU and 5-10x memory, because every access is instrumented
and the shadow state is large. It requires cgo and a C compiler, and is
supported only on specific platform/arch combinations (linux, darwin, windows,
freebsd, netbsd on supported architectures). It also changes defer/recover
allocation behavior. You run it in CI (`go test -race`), in load tests, and when
reproducing a suspected race locally; you do not ship a binary built with it.

The runtime reads several `GORACE` options from the environment to control the
detector: `exitcode` (the process exit code on a detected race, default 66),
`halt_on_error` (stop at the first race rather than continuing, useful to fail
fast in CI), `log_path` (write reports to `<path>.<pid>` instead of stderr;
`stdout`/`stderr` are special), `history_size` (per-goroutine memory-access
history; increase it when a report says it "failed to restore the stack" on a
deep call chain, at a memory cost), and `strip_path_prefix` (trim a leading path
prefix from the stack frames). The `race` build tag, set automatically when
building with `-race`, lets you include or exclude code per build via
`//go:build race` and `//go:build !race` -- the standard way to keep an
intentionally-racy micro-benchmark out of the race build so the CI gate stays
green.

## The detector is necessary but not sufficient

`-race` finds data races. It does not find deadlocks (two goroutines each waiting
on a lock the other holds), lost-update race conditions that are correctly
synchronized but logically wrong, or goroutine leaks (a goroutine that blocks
forever and never exits). A green race gate is one guarantee among several. Pair
it with deadlock timeouts in tests, goroutine-leak checks, and ordinary
reasoning about invariants. Treat `-race` as the memory-model conscience of the
codebase, not as a proof of concurrent correctness.

## Common Mistakes

### Not running -race in CI at all

Wrong: the CI pipeline runs `go test ./...`. The detector never observes the
racing interleavings, so every data race in the codebase ships. A plain
`go test` is not a race gate.

Fix: run `go test -race ./...` in CI, ideally with `-count` to vary
interleavings on the concurrent tests. The race gate is only real if it runs.

### Compiling the production binary with -race

Wrong: shipping a release built with `-race` "to be safe." The 2-20x CPU and
5-10x memory overhead and the cgo dependency make it unusable in production.

Fix: `-race` is a test and CI flag only. Build release binaries without it.

### Treating a race report as "probably fine, works in practice"

Wrong: seeing a `DATA RACE` report and leaving it because the program seems to
work. A data race is undefined behavior; the next toolchain, `GOMAXPROCS`, or CPU
can tear a value or crash.

Fix: every race report is a bug to fix, with atomics, a mutex, copy-on-write, or
a channel, depending on the access pattern.

### Believing green under -race means race-free

Wrong: concluding the code is race-free because one `go test -race` run passed.
It only means no race occurred on the interleavings that ran; an untested
concurrent path or a rare interleaving can still race.

Fix: increase coverage -- concurrent hammering, `-count=N`, realistic load. Green
is "no race on what I ran," which is only as strong as what you ran.

### Guarding compound state with independent atomics

Wrong: making a limiter's token count one atomic and its refill timestamp
another, then updating both. The per-field atomicity does not make the two-field
update atomic; goroutines interleave between the atomics and break the invariant.

Fix: put compound state (fields that must change together) under one `sync.Mutex`
critical section.

### Returning the internal map or slice from under a lock

Wrong: `RLock`, `return c.internalMap`, `RUnlock`. The caller now reads the
internal map after the lock is released, racing with the next writer.

Fix: return a defensive copy (`maps.Clone`) while holding the lock, or hold the
lock for the whole read. Never hand out a reference to lock-protected state.

### Check-then-act for lazy init across goroutines

Wrong: `if p == nil { p = open() }` for a shared lazy singleton. Two goroutines
both observe `nil` and both initialize, racing on the assignment and possibly
leaking one of the two resources.

Fix: `sync.Once.Do` (or an atomic compare-and-swap) so exactly one
initialization happens and every caller observes the same result.

### Mutating fields of a shared config in place on a read-heavy path

Wrong: a reload goroutine writes `cfg.Timeout = ...; cfg.Limit = ...` on a
`*Config` that request goroutines are reading. Readers see half-updated state.

Fix: build a new immutable `*Config` and swap the pointer with
`atomic.Pointer.Store` (copy-on-write). Never mutate a published snapshot.

### Racing on the aggregation, not just the work

Wrong: fanning work out to N goroutines that each write results into one shared
slice or map with no lock. The collection step is the race even if each unit of
work is independent.

Fix: fan results in over a channel to a single collector, or guard the shared
collection with a lock. Synchronize the handoff, not only the computation.

### Putting an intentionally-racy demo in the normal test suite

Wrong: a deliberately-racy benchmark or demo that `go test -race` compiles and
runs, so the CI gate trips on the demo you added to teach the race.

Fix: gate such code behind `//go:build !race`, or keep it under `cmd/racy`
invoked manually with `go run -race`. The CI race gate must stay green on the
real code.

### Ignoring GORACE tuning in CI

Wrong: leaving `history_size` at its default and getting "failed to restore the
stack" on deep call chains, or ignoring the detector's exit code so a reported
race passes the harness unnoticed.

Fix: set `GORACE="halt_on_error=1 exitcode=1 history_size=2"` (or similar) in the
CI race target, and make the harness honor the exit code.

Next: [01-concurrent-metrics-counter.md](01-concurrent-metrics-counter.md)
