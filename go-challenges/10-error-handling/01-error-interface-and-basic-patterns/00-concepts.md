# The error Interface and Basic Error Patterns — Concepts

Error handling is not a topic you finish; it is a set of load-bearing decisions
you make on every function a service exposes. Go made a deliberate, contested
choice: errors are ordinary values returned alongside results, checked by hand,
never thrown. That choice pushes the discipline onto you and your tooling instead
of onto the runtime. The payoff is that a failure is just data — you can wrap it,
compare it, store it, branch on it, and test it like any other value. The cost is
that every one of those decisions is now yours to get right: what you return on
the error path, how a caller recognizes a specific failure, where you handle
versus propagate, and what your returned error set promises. This file is the
conceptual spine for the nine independent exercises that follow; each one turns
one of these decisions into a production-shaped artifact — a service layer, a
config loader, a cache, a client constructor, a validator, a rate limiter, a
write path — with a real test that pins the behavior.

## The error interface is a one-method contract, and an interface value

`error` is nothing but `interface { Error() string }`. Any type with an
`Error() string` method satisfies it. That is the entire language-level story.
Because it is an interface, an `error` value is a `(concrete type, value)` pair:
a type word and a data word. A concrete `*ServiceError`, a `*net.OpError`, an
`errors.errorString` from `errors.New`, or a bare `io.EOF` can all inhabit the
same `error`-typed variable. In that sense `error` is the informal sum type of
every failure a program can express: one static type, arbitrarily many dynamic
types underneath. Understanding that it is an interface value — not a magic
nullable — is what makes the typed-nil trap below comprehensible rather than
mysterious.

## nil means success is a convention, enforced by discipline

The compiler does not force a caller to check the error you return. `v, _ := f()`
compiles fine. "A non-nil error means the call failed, nil means it succeeded" is
a convention that `go vet`, `errcheck`, and code review enforce, not the language.
The important corollary is a contract you owe the caller: when you return a
non-nil error, return the zero value for every other result. The caller is
entitled to ignore those other results entirely once it sees a non-nil error, so
a half-populated struct handed back alongside an error is a booby trap — someone
will eventually use it. "Zero value on the error path" is the single most
repeated rule across these exercises, from the service layer to the config loader
to the constructor.

## error is always the last return value

By convention `error` is the final return value: `v, err := f()`, then
`if err != nil { ... }`. Uniform placement is what lets every reader and every
linter reason about error handling mechanically. Returning `(error, value)` or
burying the error in the middle of a result list fights every tool and every
human who reads the code. This is a small rule with an outsized payoff in
consistency.

## The typed-nil trap: a nil pointer inside an interface is not nil

This is the highest-severity Go-specific bug in this lesson. If a function is
declared to return a concrete pointer type — `func validate() *ValidationError` —
and you assign its result into an `error` variable, a nil `*ValidationError`
becomes a non-nil `error`. The interface's type word is set (to `*ValidationError`)
even though its value word is nil, and an interface is nil only when *both* words
are nil. So:

```
var e *ValidationError = nil
var err error = e
// err != nil is TRUE — the type word is non-nil
```

The consequence is a `if err != nil` that fires on success, shipping a spurious
failure to production. The fix is a rule, not a trick: declare the return type as
`error`, and return a literal `nil` on the success path. Never return a
concrete-pointer type when the caller will treat it as `error`.

## Errors are values: identity, not text

`errors.New` returns a fresh `*errorString` pointer on every call. Two errors
built from identical text are not equal, because equality on that pointer is
identity, not string comparison:

```
errors.New("cache miss") == errors.New("cache miss") // false
```

This is why sentinel errors work: a sentinel is a *single* package-level variable,
created once, compared by identity. `errors.Is(err, ErrNotFound)` walks the
`Unwrap` chain and succeeds when it reaches that exact value. It follows that you
must never build a "sentinel" inline at the return site (each call makes a
distinct value no caller can match) and never compare error *messages* for control
flow (the text is a log concern and will drift). Branch on sentinels and types via
`errors.Is` and `errors.As`; treat the string strictly as output for humans.

## Error strings have conventions because callers concatenate them

An error message should start with a lowercase letter and carry no trailing
punctuation or newline, because callers wrap it: `fmt.Errorf("load config: %w", err)`
produces `load config: ...`, and a capitalized, period-terminated inner message
reads as `load config: Could not read file.: ...` — broken twice over. The
`Error()` output is for logs, operators, and structured-logging fields; it is
never a UI string for end users and never a control-flow key. Include actionable
context (which key, which id, which amount) so an on-call engineer can act on the
line without reading source.

## Handle an error exactly once

Either handle a failure — retry, fall back to a default, translate it to an HTTP
status, record a metric — or propagate it upward with added context. Do not do
both. The "log it and also return it" reflex is how one root failure becomes five
stack-shaped log lines and a metric counted five times, one per layer it passed
through. The layer that can actually make a decision owns the error; every layer
below it just adds context and returns. This "handle once" rule is what keeps
observability honest as a request crosses a call stack.

## Constructors and validators are the cheapest guard

The cheapest place to reject an invalid state is the boundary where it would be
created. A constructor that returns `(*Client, error)` and refuses to build a
client with a relative base URL or a non-positive timeout means a half-built
client never escapes into the rest of the program — you turn a diffuse class of
runtime failures into one guarded line. This is "make invalid states
unrepresentable" applied at the return statement: return `(nil, error)` on any
invalid input, and never return a non-nil object together with a non-nil error
(the caller cannot tell which to trust).

## Deferred Close and Flush on a writable resource can lose data

`defer f.Close()` is fine to ignore on a read-only handle: closing a file you only
read from cannot lose data. On a *writable* resource it can. A buffered writer may
hold the last kilobytes of your report in memory; `Flush` is where those bytes
actually hit the file, and `Close` may flush too — either can fail on a full disk
or a broken pipe, and if you drop that error you have silently truncated the
output while returning success. Capture it. The pattern is a named return plus a
deferred closure that folds the close error into it, often with `errors.Join` so a
mid-write failure and a close failure are *both* reported rather than one masking
the other. `_ = f.Close()` must be a deliberate, commented decision reserved for
read-only handles, not a reflex.

## The returned error set is an API contract

Which errors an exported function can return is part of its signature as surely as
its parameter types. Callers write `if errors.Is(err, ErrRateLimited)` or
`if errors.Is(err, ErrNotFound)` and branch on it. If you later widen or change
that set — return a new sentinel, or stop returning an old one — you break those
callers silently, with no compile error. So document the sentinels and typed
errors a function returns, and pin them with a test that asserts the *only*
non-nil error a method yields is the one its contract promises. Treating the error
set as a private implementation detail you can change freely is a breaking change
in disguise.

## Common Mistakes

### Returning a partially-populated value with a non-nil error

Wrong: `return partialUser, err`. The caller may use `partialUser` because it
forgot to check `err`, or because the value looks plausible. Fix: return the zero
value — `return User{}, err` — so half-valid data can never leak past the error
path.

### Declaring a concrete-pointer return instead of error

Wrong: `func validate() *ValidationError`, then `return nil` on success and
assigning the result to an `error`. The nil pointer becomes a non-nil interface
and `if err != nil` fires on success. Fix: declare the return as `error` and
return literal `nil` on success.

### Comparing wrapped errors with ==

Wrong: `if err == ErrNotFound` when `err` came back wrapped in a `*ServiceError`.
The `==` compares the outer value, which is not the sentinel, so the check fails.
Fix: `errors.Is(err, ErrNotFound)`, which walks the `Unwrap` chain. (`err == io.EOF`
is fine only when you know the error is the bare, unwrapped sentinel.)

### Matching on err.Error() text for control flow

Wrong: `if strings.Contains(err.Error(), "not found")`. The message is a
log/UI concern and will drift the moment someone rewords it. Fix: branch on a
sentinel or type via `errors.Is` / `errors.As`.

### Log-and-return at every layer

Wrong: logging the error in the repository, again in the service, again in the
handler, then returning it each time — one failure, five log lines, five metric
increments. Fix: handle once at the layer that can decide; elsewhere add context
and return.

### Ignoring a deferred Close or Flush on a writable file

Wrong: `defer f.Close()` on a file you wrote to, discarding the close error.
The last buffered writes can vanish on a full disk with no signal. Fix: capture
the close/flush error into a named return, joining it with any primary error.

### Assuming two errors.New with the same text are equal

Wrong: treating `errors.New("x")` as a reusable comparable constant. Each call is
a distinct pointer. Fix: define a sentinel once as a package-level `var` and
compare with `errors.Is`.

### Capitalizing or punctuating error strings

Wrong: `errors.New("User not found.")`, which becomes `get user: User not found.: ...`
once wrapped. Fix: `errors.New("user not found")` — lowercase, no trailing period.

### Deeply nested validation instead of flat guard clauses

Wrong: a pyramid of `if ok { if ok2 { ... } }` that hides which condition failed
and obscures short-circuit order. Fix: flat early-return guards — check, return on
failure, fall through on success — so the first failure is obvious and the order
is explicit.

### Treating the returned error set as changeable

Wrong: adding, removing, or renaming the sentinels a function returns as if it were
private. Callers branch on them; changing the set is a breaking change. Fix:
document the error contract and pin it with a test.

Next: [01-user-service-error-contract.md](01-user-service-error-contract.md)
