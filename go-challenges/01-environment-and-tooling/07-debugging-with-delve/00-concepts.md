# Debugging Go with Delve — Concepts

`fmt.Println` debugging works until it does not. The moment the failure is a
deadlock that only appears under load, a handler that returns wrong data for one
tenant, or a binary that crashes only inside the production container, print
statements force you to redeploy to change what you observe and still cannot show
you a goroutine's wait reason or the value of a local three frames up. Delve is
the Go-native debugger: it reads the runtime's own data structures, so it decodes
goroutines, slice headers, channel buffers, interface values, and maps the way Go
sees them. It is the same engine VS Code's Go extension and GoLand drive, so
learning it at the command line transfers to every editor. This file is the
conceptual foundation shared by all ten independent exercises that follow; read
it once and you have the model you need for interactive, test, goroutine, attach,
remote, watchpoint, and post-mortem debugging.

## Concepts

### Why a Go-aware debugger beats GDB

A generic debugger like GDB can attach to a Go binary, but it sees the world in C
terms: raw stack frames, raw addresses, and no notion of a goroutine. Go
multiplexes many goroutines (G) onto OS threads (M) via logical processors (P),
and that mapping lives in runtime structures GDB does not understand. Delve is
written for Go specifically. It walks the runtime's goroutine list, so `print s`
on a slice shows `[]int len: 5, cap: 8, [10,20,30,40,50]` rather than a pointer;
`print ch` shows a channel's buffer and length; an interface prints its concrete
type and value because Delve reads the `itab` and data word; a map prints its
keys and values because Delve understands the bucket layout. Most importantly,
`goroutines` enumerates every G with its wait reason, which is the single most
useful view for a deadlock or a leak and something GDB cannot reconstruct.

### The optimization tax: -N and -l

The Go compiler optimizes aggressively. It inlines calls, keeps values in
registers, reorders instructions, and elides variables whose storage it can
prove is unnecessary. To a debugger that view is hostile: a breakpoint requested
on a line that was inlined away has nowhere to land, and `print n` on an elided
variable reports `<optimized out>`. Two compiler flags restore a faithful view.
`-N` disables optimizations; `-l` disables inlining. You pass them together as
`-gcflags='all=-N -l'`, where the `all=` prefix applies them to every package in
the build, not just the top one, so stepping into a dependency still works.

The critical operational fact: `dlv debug` and `dlv test` inject `all=-N -l`
automatically because they compile the binary for you. `dlv exec`, `dlv attach`,
and `dlv core` do not — they consume a binary that already exists. If that binary
was built with optimizations, you are debugging a lie: line numbers drift and
variables vanish. For those three modes you must have built with
`go build -gcflags='all=-N -l'` yourself.

### Breakpoints are source-location or symbol based

`break file.go:18` sets a breakpoint at line 18; `break pkg.Func` sets one at a
function's entry by resolving the symbol through DWARF debug information. Delve
never lets a breakpoint sit on a blank line or a comment — it silently relocates
the request forward to the next executable statement. That relocation is a
frequent source of confusion: list the source first so you know which statement
the breakpoint actually guards. A conditional breakpoint attaches a Go expression
that Delve re-evaluates in the breakpoint's scope on every hit; execution stops
only when the expression is true. This trades a little CPU per hit for skipping
the N manual continues it would otherwise take to reach one iteration of a hot
loop.

### Execution control: next, step, stepout

Three commands move the current goroutine forward, and confusing them wastes
sessions. `next` (n) steps over a call: the callee runs to completion and you
stop on the next line in the current function, so you never see the callee's
locals. `step` (s) descends into the call, stopping on its first line. `stepout`
(so) runs the current function to its return and stops in the caller. A trap
worth internalizing: `step` into a line that calls into the standard library
drops you inside runtime or stdlib code you did not want; `next` past it stays in
your code. These commands act on the current goroutine only — the scheduler is
free to advance other runnable goroutines while you single-step, which is exactly
why a concurrent bug can look non-deterministic under the debugger.

### The goroutine model in Delve

`goroutines` lists every G with its id, the function that created it, its current
function, and its wait reason (chan send, chan receive, select, semacquire, IO,
and so on). `goroutine <id>` switches the inspection context so that subsequent
`stack`, `locals`, and `print` reflect that goroutine's frames rather than the
current one. This pair is the primary tool for diagnosing deadlocks and leaks:
when a program hangs, `goroutines` shows you which G is stuck and on what, and
switching to it lets you read the stack that led there. It complements
`runtime.NumGoroutine` (a count you can log) and `net/http/pprof`'s goroutine
profile (an aggregate dump), giving you the interactive, per-goroutine view.

### Launch vs attach vs remote

Delve reaches a target four ways. `dlv debug`, `dlv test`, and `dlv exec` launch
a fresh process under the debugger. `dlv attach <pid>` stops a process that is
already running — the production-flavored mode, used to inspect a live server
without restarting it; it requires the binary and source to match the running
process and the OS permission to ptrace it. `dlv debug --headless
--listen=:2345 --api-version=2` starts the debugger as a backend server that
speaks its own protocol over a socket, and `dlv connect host:port` attaches a
separate client to it — this is the container and remote-host pattern, since the
backend can run inside Docker while your client runs on the host. Adding
`--accept-multiclient` lets several clients share one backend. Finally, `dlv dap`
runs that same backend speaking the Debug Adapter Protocol, the language-agnostic
protocol VS Code and other editors drive under the hood, so the editor experience
is the same backend you already understand.

### Watchpoints are hardware data breakpoints

A watchpoint stops the program the instant a piece of memory is read or written,
which is the correct tool for the question "who is mutating this field?".
`watch -w &s.Count` stops on write, `-r` on read, `-rw` on either. Delve
implements these with the CPU's debug registers, not by polling and comparing, so
they are cheap at runtime but scarce: a typical x86-64 machine has four debug
registers, so you get a handful of watchpoints at once. A watchpoint is also tied
to an addressable, in-scope expression; when the variable's frame returns, the
watched location goes out of scope and the watchpoint is removed. Watchpoints
require hardware support in the Delve backend, so availability depends on the
platform and architecture.

### Post-mortem debugging with core dumps

Some crashes cannot be reproduced interactively — they happen once, in
production, under a load you cannot recreate. For those, you debug the corpse.
Setting `GOTRACEBACK=crash` makes the Go runtime, on an unrecovered panic or
fatal error, raise `SIGABRT` so the OS writes a core dump (given
`ulimit -c unlimited` and a platform that supports core files). `dlv core <exe>
<corefile>` then loads that snapshot and reconstructs goroutines, stacks, and
locals as they were at the instant of death, so `goroutines`, `stack`, and
`locals` work on a dead process exactly as they do on a live one. The same
binary that crashed must be paired with the core, built with `-N -l`, or the
frames and variables will be wrong. Within a live session, the `dump` command
writes a core file of the current process so you can capture state and analyze it
later. Linux supports this cleanly; macOS core-dump behavior differs and is
documented as a limitation in the post-mortem exercise.

### Scriptability for CI and headless use

Every Delve REPL command can be replayed from a file. `dlv <mode> --init
script.txt` runs the commands in the file before handing you the REPL, and
`source file.txt` loads a command file mid-session. Put `quit` at the end of the
script and Delve exits after running it, turning debugger output into a captured,
assertable artifact. Combined with headless mode this enables reproducible,
human-free debugging steps inside a pipeline: build with `-N -l`, run a scripted
session, grep the captured output for the value you expect, and fail the job if
the marker line is missing.

### The senior trade-off: prints vs Delve

Prints and structured logs are zero-friction and always-on, but changing what you
observe means editing code and redeploying, and they cannot show you arbitrary
post-hoc state. Delve gives arbitrary inspection of any variable, frame, or
goroutine after the fact, but it needs an unoptimized build, a source-to-binary
match, and — for attach and core — elevated privileges. The senior skill is
matching the tool to the failure class: a logic bug that reproduces locally wants
`dlv debug` or `dlv test`; a concurrency bug wants `goroutines` and watchpoints;
a crash that only happens in prod wants attach or a core dump. Know which of
those you are holding before you reach for a tool.

## Common Mistakes

### Debugging an optimized binary

Wrong: running `dlv exec`, `dlv attach`, or `dlv core` on a binary built with the
default optimizing compiler and being surprised by `<optimized out>` and
breakpoints that will not bind. Fix: `dlv debug` and `dlv test` handle this for
you; for the other three, build with `go build -gcflags='all=-N -l'` first and
debug that binary.

### Setting a breakpoint on a blank line or comment

Wrong: `break main.go:15` when line 15 is blank, then assuming execution stops
exactly there. Delve relocates the breakpoint forward to the next executable
statement, which may be several lines down. Fix: list the source first so you set
the breakpoint on a line that actually holds a statement.

### Confusing next with step

Wrong: pressing `next` when you meant to descend into a call, losing the callee's
locals; or pressing `step` on a line that calls into the standard library and
landing inside runtime code. Fix: `next` to stay in your function, `step` to
enter a call you own, `stepout` to return to the caller.

### Forgetting the -- separator

Wrong: `dlv debug ./cmd 10 20`, which makes Delve try to parse `10` and `20` as
its own flags. Fix: `dlv debug ./cmd -- 10 20`; everything after `--` goes to the
program.

### Mismatched binary or source for attach and core

Wrong: attaching to a running server, or opening a core, with a binary built from
a different commit than the one that is running or that crashed. The DWARF line
tables no longer match, so line numbers and variables are wrong. Fix: rebuild
from the exact commit and debug that.

### Leaving a headless backend open on a public address

Wrong: `dlv debug --headless --listen=:2345 --accept-multiclient` bound to a
public interface. A Delve backend grants full code execution over the socket to
anyone who connects. Fix: bind to `127.0.0.1` (or a private network you control)
and never expose the port publicly.

### Assuming watchpoints are unlimited

Wrong: setting many watchpoints, or watching a local and expecting it to persist
after its function returns. Watchpoints consume scarce hardware debug registers
and only work on addressable, in-scope expressions. Fix: watch a field on a
long-lived struct, keep the count small, and clear watchpoints you no longer
need.

### Expecting a core dump without the right environment

Wrong: reproducing a crash and looking for a core file without
`GOTRACEBACK=crash` and `ulimit -c unlimited`, on a platform whose core behavior
differs from Linux. Fix: set both, confirm the platform writes cores (Linux does
cleanly; macOS differs), then run `dlv core`.

### Treating other goroutines' progress as a bug

Wrong: single-stepping a concurrent program and concluding it is broken because
other goroutines advanced between your steps. The scheduler keeps running them;
`step`/`next` only control the current goroutine. Fix: use `goroutine <id>` to
switch context and breakpoints to pin the goroutine you care about instead of
assuming stepping freezes the world.

### Not detaching cleanly from a live process

Wrong: quitting an attached session in a way that kills the target, taking down a
live server. Fix: when detaching from a production process, choose detach (not
kill) so the process keeps running after your session ends.

Next: [01-interactive-bug-hunt.md](01-interactive-bug-hunt.md)
