# Labels, break, continue, and goto: loop control in real backend code — Concepts

Labels are rare in Go, and most services ship without a single one. But the two
places labels matter are load-bearing in every production backend, and both are
about leaving a loop correctly. The first is the difference between a service
that drains cleanly on a deploy and one that hangs until the orchestrator sends
`SIGKILL`: a bare `break` inside a `for`-`select` event loop leaves only the
`select`, so the loop keeps spinning and the worker never shuts down. The fix is
a labeled `break` on the `for`. The second is `goto`, which is not dead — it is
the C-style resource-unwind ladder still used in stdlib hot paths and generated
code, and the honest way to teach the spec's jump restrictions. This file is the
conceptual foundation for the nine independent exercises that follow; read it
once and each module makes sense on its own.

## Concepts

### Plain break and continue only reach the innermost for

A plain `break` exits the innermost enclosing `for` loop, and a plain `continue`
skips to the next iteration of that innermost loop. Neither reaches an outer
loop. In two nested loops, a bare `break` in the inner loop leaves only the inner
loop; the outer loop keeps running. That is exactly right for "skip the rest of
this row and move to the next row," and exactly wrong for "we found it, leave the
whole scan." When "leave the whole scan" is what you mean, the bare `break` is a
silent bug: the outer loop continues and, in a first-match search, a later
iteration overwrites the result you already found.

### The senior gotcha: break and continue inside switch and select

This is the single most important fact in the lesson. `break` and `continue`
inside a `switch` or a `select` refer to the `switch` or `select`, not to an
enclosing `for`. A `for`-`select` event loop is the canonical shape:

```go
for {
	select {
	case ev := <-events:
		if ev.Kind == Shutdown {
			break // leaves the select, NOT the for
		}
	}
}
```

That `break` exits the `select` and the `for` immediately loops again. The
service never shuts down; it burns a core spinning on `select` forever, and on a
deploy the orchestrator waits out the grace period and `SIGKILL`s it. This is the
number-one labeled-break production bug. The fix is a label on the `for`:

```go
loop:
	for {
		select {
		case ev := <-events:
			if ev.Kind == Shutdown {
				break loop // leaves the for
			}
		}
	}
```

The same trap applies to `continue`: a `continue` inside a `select` or `switch`
does not continue an enclosing `for` unless you name the loop's label.

### Labeled break and labeled continue

A labeled `break` exits the `for`, `switch`, or `select` named by the label. A
labeled `continue` continues the enclosing `for` named by the label — and
`continue` may only ever target a `for` loop. You cannot `continue` a `switch` or
a `select`; there is no next iteration to continue. So a labeled `continue`
always references a `for` label, never a `switch`/`select` label. A labeled
`break`, by contrast, can target any of the three.

The label is a labeled statement placed immediately above the `for`, `switch`, or
`select` it names. Naming it after intent — `search`, `done`, `drain`,
`nextService`, `merge` — reads as a statement of purpose, not as machinery. An
unused label is a compile error (`label ... defined and not used`), which
usefully catches a label you deleted the target of.

### goto and its spec restrictions

`goto label` transfers control to a labeled statement in the same function. Its
scope is the entire enclosing function body and excludes the body of any nested
function literal — you cannot `goto` into or out of a closure. Two restrictions
matter and are enforced at compile time, not at runtime:

- A `goto` may not jump over the declaration of a variable that is still in scope
  at the label. Jumping forward past `x := f()` to a label where `x` is live is a
  compile error (`goto ... jumps over declaration of x`). The practical
  consequence: in a cleanup ladder, declare the variables the labels touch up
  front with `var`, before any `goto`, so no jump crosses a declaration.
- A `goto` outside a block may not jump to a label inside that block (`goto ...
  jumps into block`).

### goto's legitimate niche: the resource-unwind ladder

`goto`'s honest use is the reverse-order cleanup ladder. Acquire A, then B, then
C; if acquiring B fails, jump to the label that releases only A; if C fails, jump
to the label that releases B then A. Each failure lands on the label matching the
last resource successfully acquired, and the labels fall through in reverse order
so resources are released LIFO. `defer` is the modern idiom for this and is what
you should reach for in ordinary code — a `defer` per resource, guarded by an
`ok` flag, releases in LIFO order automatically. But `goto` still appears in
stdlib hot paths and in generated code where the per-call cost of `defer` is
deliberately avoided, so a senior engineer should be able to read and write the
ladder, and to explain why the variable-declaration restriction exists.

### The canonical labeled break in servers: leaving a for-select

In real backends the labeled `break` almost always appears leaving a
`for`-`select`: it is how a graceful-shutdown, reconnect, or drain loop actually
terminates on `ctx.Done()` or a sentinel event. A worker consuming a jobs channel
loops on `select { case <-ctx.Done(): ...; case j := <-jobs: ... }`; the only
correct way to leave that loop is a labeled `break` (or a `return`), because a
bare `break` in either case leaves the `select` and the loop resumes. Graceful
shutdown that drains in-flight work first uses the same shape with a non-blocking
inner drain (`select { case j := <-jobs: ...; default: break loop }`).

### Timer and backoff correctness in reconnect loops

A reconnect-with-backoff loop must race its backoff timer against `ctx.Done()` in
the `select`, so cancellation can interrupt a sleep instead of waiting out the
full backoff. Since Go 1.23 the garbage collector reclaims timers and tickers
that are no longer referenced even if you never call `Stop`, so an unstopped
`time.NewTimer` in a backoff loop no longer leaks. But calling `Stop` on the
early-exit path is still good hygiene: it makes intent explicit and prevents a
stray tick from firing into logic that has already moved on. Race the timer
channel against `ctx.Done()`; on the cancel branch, `Stop` the timer and leave the
loop with a labeled `break`.

### Modern loop idioms (Go 1.22-1.26)

Use `for i := range n` for counted loops — it reads more clearly than the
three-clause `for i := 0; i < n; i++`. Since Go 1.22 each loop iteration gets a
fresh copy of the loop variable, so a goroutine spawned inside a labeled loop no
longer captures a shared, aliased variable; the old `x := x` copy is unnecessary.
Relying on the pre-1.22 shared-variable behavior is now a latent bug, not a
compatible assumption.

### A labeled break beats a sentinel-flag double-break

A sentinel boolean plus a second `break` in the outer loop is functionally a
labeled `break` with more moving parts. It turns a straight-line search into a
hand-rolled state machine: the reader has to track the flag, notice the second
`break`, and confirm nothing else touches the flag. A single label says "leave
this loop" directly. When one label does the job, the sentinel-flag form is a
code smell.

## Common Mistakes

### A bare break inside select or switch to leave the for

Wrong: a bare `break` in a `for`-`select` case, expecting it to leave the loop.
It leaves only the `select`; the loop keeps running and the service never shuts
down. This is the number-one labeled-break production bug.

Fix: put a label on the `for` and `break theLabel`.

### A sentinel flag plus a second break instead of a label

Wrong: set `found = true; break` in the inner loop, then `if found { break }` in
the outer loop. A search now reads as a state machine.

Fix: a labeled `break` leaves both loops in one statement.

### goto to skip a loop iteration

Wrong: `goto next` at the top of a loop body to skip an iteration, with a `next:`
label at the bottom. It is harder to read and defeats the eye's ability to see
the loop body as a unit.

Fix: use `continue` — that is exactly what it is for.

### Trying to continue a switch or select

Wrong: `continue` inside a `switch`/`select` expecting it to continue the loop.
`continue` only applies to `for`, so it must reference an enclosing `for` label.

Fix: `continue theForLabel`, naming the enclosing loop.

### Placing the label on the wrong line

Wrong: a label sitting above an `if` or a statement other than the `for`/`switch`/
`select` it is meant to name, so it breaks the wrong construct.

Fix: the label must sit immediately above the `for`/`switch`/`select` it names.

### goto jumping over a declaration or into a block

Wrong: `goto cleanup` that skips forward past `x := acquire()` while `x` is still
in scope at `cleanup`, or a `goto` into a nested block. Both are compile errors
(`jumps over declaration`, `jumps into block`), not runtime surprises.

Fix: declare the variables the labels touch with `var` up front, before any
`goto`, and keep labels at the function's top level.

### Leaving a label declared but never targeted

Wrong: a `search:` label with no `break search`/`continue search` anywhere.
Compile error: `label search defined and not used`.

Fix: remove the dead label, or wire up the `break`/`continue` that needs it.

### Not racing the backoff timer against ctx.Done()

Wrong: a reconnect loop that `time.Sleep`s the backoff, so cancellation cannot
interrupt the sleep and shutdown stalls for the full backoff duration.

Fix: `select` on `timer.C` versus `ctx.Done()`, and leave the loop with a labeled
`break` on the cancel branch.

### Assuming pre-Go-1.22 loop-variable aliasing

Wrong: writing `x := x` inside a labeled loop before spawning a goroutine,
"to avoid the aliasing bug." On Go 1.22+ each iteration already has its own
variable, so the copy is dead code — and code that relies on the old shared-
variable behavior is a latent bug.

Fix: spawn the goroutine directly; the per-iteration variable is already fresh.

Next: [01-matrix-labeled-break-search.md](01-matrix-labeled-break-search.md)
