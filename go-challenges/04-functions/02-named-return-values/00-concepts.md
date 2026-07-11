# Named Return Values: defer-Coupled Error Wrapping, Recovery, and Cleanup — Concepts

Named return values look like a documentation feature and are taught that way to
beginners: "you can label the return list so godoc reads nicely." That framing
misses the only reason a senior backend engineer reaches for them. A named result
is an ordinary local variable that is *in scope inside the function's deferred
closures*, and a deferred closure can both read and rewrite it after `return` has
run but before the caller receives it. That single fact is the load-bearing
mechanism behind almost every production cleanup idiom in Go: wrapping a
repository error with operation context in one place, committing-or-rolling-back a
transaction from a single exit, turning a worker/plugin panic into a returned
error, not silently dropping a `Close`/`Flush` failure, and recording latency plus
outcome for observability. Take away named returns and none of those collapse to
one deferred closure. This file is the model you carry into all ten exercises;
read it once and each exercise becomes an application of one idea here.

## Concepts

### A named result is a pre-initialized local, scoped to the whole body

When you write `func FindUser(id string) (u User, err error)`, the identifiers `u`
and `err` are declared as local variables at the top of the function body,
initialized to their zero values (`User{}` and `nil`). They are ordinary
variables: you can assign to them, read them, and — this is the point — they are
in scope inside any `defer func(){ ... }()` you register. The Go spec is explicit
that the scope of a function's result parameters is the function body, which is
exactly why a deferred function can see them.

### `return x` assigns the named result, then defers run

The mechanic that makes everything work: `return expr` first copies `expr` into
the named result variable, and only then runs the deferred functions, in
last-registered-first order. A deferred function that assigns to the named result
overwrites what the caller will observe. So:

```go
func f() (n int) {
	defer func() { n *= 2 }()
	return 21 // sets n = 21, then defer runs n *= 2, caller sees 42
}
```

The caller receives `42`. This only works because `n` is named; a deferred closure
cannot alter an anonymous return value — there is no variable to reach. Expecting
a defer to change a non-named return, or being surprised that `return x` set the
named result before your defer ran, are both symptoms of not internalizing this
ordering.

### Naked returns: a readability win only in short functions

A naked `return` (no operands) returns the current values of the named results. In
a five-line function whose result names fully describe the answer, it reads
cleanly. In a forty-line function, a bare `return` forces the reader to scroll back
to the signature and mentally reconstruct what is being returned — a readability
tax, not a feature. The rule is not "always name returns" or "never use naked
returns"; it is: name returns when a deferred closure or a single-exit contract
needs them, and keep the function short enough that a naked return is still
legible. Otherwise return explicitly.

### The number-one production use: uniform error decoration

The most valuable everyday application is wrapping every failure exit with the
same operation context, written once:

```go
func (r *Repo) FindUser(id string) (u User, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("FindUser %q: %w", id, err)
		}
	}()
	// ... several return sites, none of which repeat the decoration ...
}
```

Every non-nil exit now carries `FindUser "…": ` in front of it, and `%w` keeps the
underlying error reachable through `errors.Is`/`errors.As`. Without a named `err`
the deferred closure would have nothing to inspect or rewrite; you would have to
paste `fmt.Errorf` at each `return`, and the day someone adds a new return site and
forgets, that exit leaks an undecorated error.

### Transaction and resource discipline collapse to one exit

The canonical transaction idiom is a single deferred closure keyed on the named
`err`:

```go
defer func() {
	if p := recover(); p != nil {
		_ = tx.Rollback()
		panic(p)
	}
	if err != nil {
		_ = tx.Rollback()
		return
	}
	err = tx.Commit()
}()
```

Commit on success, roll back on error, roll back and re-raise on panic, and — the
part people forget — surface a commit failure by assigning it to the named `err`.
All four behaviors depend on `err` being a named result the defer can read and
write. The same shape generalizes to any acquire/release pair: acquire, register a
defer that releases only when `err` is non-nil, proceed.

### Panic-to-error conversion needs a named err to carry the value out

A worker pool, a plugin host, or an HTTP boundary must not let one bad task crash
the process. The wrapper recovers and stores the recovered value in the named
result:

```go
func SafeRun(task func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return task()
}
```

Only a named `err` can carry the recovered value out of the function. Capture
`debug.Stack()` at recovery time so the panic is diagnosable, and scope the
recovery tightly to the intended boundary — a blanket recover that swallows
programming bugs which should crash in development is worse than the panic.

### Deferred Close/Flush errors must be promoted, not dropped

`defer f.Close()` throws the `Close` error on the floor. For a read that is often
fine; for a *writer* it is a data-loss bug, because a buffered or compressed writer
only reports a full disk or a short write when it flushes at close time. Promote
the close error into the named result, guarded so it never clobbers an earlier,
more meaningful error:

```go
defer func() {
	if cerr := w.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("close: %w", cerr)
	}
}()
```

The `err == nil` guard (or `errors.Join(err, cerr)` if you want both) is what keeps
a cleanup error from masking the real failure. Unconditional `err = f.Close()` in a
defer is the mirror-image bug: it overwrites the genuine error with the cleanup
one.

### Observability hooks read the named results in a defer

Recording how long a call took and whether it succeeded is a single deferred
closure that reads the final `err` and result:

```go
func (r *Repo) Query(op string) (rows int, err error) {
	start := time.Now()
	defer func() {
		status := "ok"
		if err != nil {
			status = "error"
		}
		r.rec.Observe(op, status, rows, time.Since(start))
	}()
	rows, err = r.load()
	return
}
```

There is no other single place from which one hook can see both the duration and
the outcome. The metric is emitted exactly once, on every exit path, because the
defer reads the named `rows` and `err` that `return` has just set.

### The signature failure mode: shadowing with `:=`

The idioms above all key on the named `err`. The way they silently break is
shadowing: an inner `if x, err := f(); err != nil { ... }` (or a `{ y, err := … }`
block) declares a *new* `err` in the nested scope, so the outer named `err` the
deferred wrapper or rollback inspects stays at its zero value. When such a path
returns naked, the function reports success while an error actually occurred — the
error is swallowed whole. The trap is that this compiles cleanly and the default
`go vet` does *not* flag it; the shadow analyzer lives in `golang.org/x/tools`
(`go vet -vettool=$(which shadow)` / `shadow` under `golang.org/x/tools/go/analysis`)
and must be run explicitly. The dependable gate is a behavioral test that asserts
the wrapper prefix and the `errors.Is` chain are present on the returned error;
with the shadowed `:=` they are absent and the test fails. Note that an *explicit*
`return "", err` copies even a shadowed inner `err` into the named result, so the
shadow bug bites hardest on naked-return and defer-reads-err paths.

### For three-plus values, a struct beats a named return list

Named returns document nothing the body does not already say when you have three
or more non-error values. `func lookup() (id, name, email string, err error)` lets
a caller write `email, name, id, err := lookup()` and corrupt data with no
compiler complaint — positional unpacking is fragile. Return a self-describing
struct instead: `func LookupUser() (UserRecord, error)`, and callers use
`rec.Name`, `rec.Email` by name. Reserve named returns for the defer-coupled
patterns; do not spend them on documenting a wide return list.

### Named and explicit returns coexist

This is not an ideology. Use named results plus a deferred closure (and often a
naked return) where a single exit contract, guaranteed cleanup, or uniform error
decoration is needed. Use explicit `return a, b, nil` in short, pure functions
where it reads more clearly — as the batch helper in the first exercise does
deliberately, to contrast with its neighbor that uses named guards. The skill is
knowing which situation you are in.

## Common Mistakes

### Naming a return value purely as documentation

Wrong: `func Get() (result string, err error)` where the body sets `result` once
from a single expression and never touches it again. The name carries no
information a plain `return value, nil` would not. Fix: name a result only when a
deferred closure reads it, or when the body assigns it in several places, or when a
naked return is the natural shape of a short function.

### A bare `defer f.Close()` that discards the write error

Wrong: `defer f.Close()` (or `defer tx.Rollback()`) on a writer, so a failed flush
on a full disk or a network drop at commit vanishes. Fix: promote the close/commit
error into the named result, guarded with `if err == nil` (or joined via
`errors.Join`) so it does not clobber an earlier failure.

### Shadowing the named result with `:=`

Wrong: `if row, err := store.Get(id); err != nil { ... }` in a function whose
deferred wrapper or rollback keys on `err`. The inner `err` shadows the named
return, the deferred closure inspects the still-zero outer variable, and a
naked-return path swallows the error entirely. Fix: pre-declare the other variable
and use `=` (`var row Row; row, err = store.Get(id)`) so the assignment lands on
the named result. Do not rely on `go vet` to catch this; write the behavioral test.

### Naked returns in a long function

Wrong: forty lines between the result list and a bare `return`, so the reader must
scroll up to reconstruct the contract. Fix: split the function, or return
explicitly at each exit.

### Overwriting a real error with a cleanup error

Wrong: `defer func(){ err = f.Close() }()` unconditionally, masking an earlier and
more meaningful failure with the close result. Fix: guard with `if err == nil`, or
combine both with `errors.Join(err, cerr)` and assert on each with `errors.Is`.

### Assuming `go vet` catches shadowed named returns

Wrong: trusting the default `go vet` to flag `:=` shadowing. It does not; the
shadow pass ships separately in `golang.org/x/tools` and must be invoked
explicitly. Fix: rely on a behavioral test as the real gate, and optionally wire
the shadow analyzer into CI.

### Recovering a panic but losing the stack, or recovering too broadly

Wrong: `recover()` into a bare `fmt.Errorf("%v", r)` with no stack, or a recover so
wide it swallows nil-map and index-out-of-range bugs that should crash in
development. Fix: capture `runtime/debug.Stack()` at recovery time and scope the
recovery to the intended boundary (one task, one request), not the whole program.

### Three-plus positional returns

Wrong: `return id, name, email, err` and letting callers unpack positionally, where
swapping `name` and `email` is a silent data corruption. Fix: return a struct so a
mis-order is a compile error, and reserve named returns for the defer-coupled
cases.

Next: [01-header-parser-guard-clauses.md](01-header-parser-guard-clauses.md)
