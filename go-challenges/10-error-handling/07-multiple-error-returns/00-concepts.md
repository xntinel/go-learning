# Aggregating and Returning Multiple Errors — Concepts

Real backend systems rarely fail one reason at a time. A fan-out to N upstreams,
a bulk write of N records, a config with N invalid fields, a shutdown that closes
N resources — each produces a *set* of independent failures. The question a senior
engineer answers on every such path is: do you stop at the first failure
(fail-fast) or do you keep going and report every one (fail-complete)? The wrong
default is expensive. Returning only the first invalid field forces the client
into N round-trips to fix a form. Returning on the first `Close` error leaks every
resource after it. Hiding the other four upstream failures behind the first one
turns an incident into a guessing game.

Since Go 1.20 the standard aggregation primitive is `errors.Join`. It takes any
number of errors, drops the nil ones, and returns a single error value that wraps
the rest — a value that `errors.Is` and `errors.As` can still walk. This file is
the conceptual foundation for the ten independent exercises that follow: the
`errors.Join` contract, the multi-error tree it builds, the fail-fast/fail-complete
decision, the sharp contrast with `errgroup` (whose `Wait` does the opposite), and
the two traps that reliably surface in code review.

## The `errors.Join` contract

`errors.Join(errs ...error) error` has three properties worth memorizing exactly,
because every design decision in this lesson rests on them:

1. It **discards nil inputs**. `errors.Join(nil, err, nil)` is an error you can
   recover `err` from with `errors.Is`. This is what lets a call site accumulate
   unconditionally — `errs = append(errs, step())` in a loop — and call
   `errors.Join(errs...)` once at the end without a single nil guard.
2. It **returns nil only if every input is nil**. `errors.Join(nil, nil, nil)` is
   untyped `nil`, not a non-nil error wrapping an empty slice. So the zero-failure
   case naturally produces a nil return; you do not special-case it.
3. The returned value's `Error()` method **concatenates the members' strings, one
   per line, separated by newlines**, and the value **implements `Unwrap() []error`**.

That last method is the hinge. A joined error is *one* error value that happens to
expose its children as a slice.

## The multi-error tree

Before Go 1.20, an error wrapped exactly one other error via `Unwrap() error`, so
the error "chain" was a linked list and `errors.Is`/`errors.As` walked it linearly.
Go 1.20 generalized this: `errors.Is` and `errors.As` now traverse `err` plus a
depth-first walk of every child reachable through **either** `Unwrap() error`
**or** `Unwrap() []error`. The chain became a tree.

This is precisely why `errors.Is(joined, ErrSourceB)` finds a sentinel buried
three levels deep inside a `Join` of `fmt.Errorf`-wrapped errors, and why
`errors.As(joined, &fieldErr)` pulls out the first `*FieldError` member. You get
the tree walk for free the moment your aggregate exposes `Unwrap() []error` — which
`errors.Join` does, and which you can do on your own type.

## Fail-fast versus fail-complete

The core design decision. Returning on the first error (fail-fast) is *correct*
when later steps depend on earlier ones: if opening the DB failed there is no point
trying to run the migration. Aggregating every error (fail-complete) is *required*
when the failures are independent:

- **Validation of a request body** — the client wants every invalid field at once,
  not to discover them one submit at a time.
- **Shutdown / closing resources** — every closer must run; stopping at the first
  `Close` error leaks the DB pool, the cache client, and the listener after it.
- **A bulk write / batch** — each record stands alone; the caller wants to commit
  the good rows and know exactly which ones failed.

Fail-complete is `errors.Join`. Fail-fast is a plain `return err` — or, for
concurrent work, `errgroup` (below).

## Wrap-then-join

Aggregation and *context* are orthogonal, and you want both.
`fmt.Errorf("source %q: %w", name, err)` preserves `errors.Is`/`errors.As` matching
(the `%w` verb) *and* stamps per-item context onto the error: which source, which
record index, which field. Joining the **wrapped** errors gives an aggregate that
is simultaneously greppable in a log ("source \"billing\": ...") and machine-
inspectable (`errors.Is(agg, ErrTimeout)` still works through both wrappers). The
rule of thumb: wrap each failure with its identity as you collect it, then join the
collection.

## Partial-success return shape

A bulk operation should not collapse to a single `error`. The honest signature is
`(succeeded int, err error)` (or a richer result carrying the set of successful
IDs), where `err` is the `errors.Join` of the failures. That shape lets the caller
commit the `succeeded` rows and surface exactly what failed. Returning only an
error throws away the fact that 998 of 1000 records were written; returning only a
count throws away *why* the other two failed.

## `errors.As`, and `errors.AsType` in Go 1.26

`errors.As(err, target)` finds the **first** error in the tree assignable to
`*target` and sets it, returning true. `target` must be a non-nil pointer to a type
implementing `error` (or to an interface); pass anything else and it *panics* at
runtime. That ceremony — declare a `var fe *FieldError`, take its address, check
the bool — is why Go 1.26 adds the generic `errors.AsType[E error](err error) (E, bool)`,
which returns the matched value directly: `fe, ok := errors.AsType[*FieldError](err)`.
Same tree walk, same "first match" semantics, no pointer dance and no panic mode.

Both return only the *first* match. If an aggregate holds three `*FieldError`s and
you must handle every one, `As`/`AsType` is not enough — you flatten the tree
yourself and range over the members.

## The contrast that matters: `errgroup`

`golang.org/x/sync/errgroup` is the fail-fast dual of `errors.Join` for concurrent
work. `Group.Wait()` returns only the **first** non-nil error any goroutine
returned, and when constructed with `errgroup.WithContext` it **cancels the derived
context** on that first error, tearing down the remaining goroutines. That is the
right tool when the earliest failure should abort the whole fan-out (a request that
is doomed the moment one dependency fails). It is the wrong tool when you need a
*census* of every failure — a health check that must report all N unhealthy
upstreams, a validation that must list all bad fields. For that you accumulate the
per-goroutine errors (guarded by a mutex or drained from a channel) and
`errors.Join` them. Same fan-out shape; opposite error policy. Choosing between
them is choosing between "earliest failure plus cancellation" and "full accounting
of every failure".

## Concurrent aggregation must be synchronized

When you accumulate errors from goroutines, the shared `[]error` is shared mutable
state. `errs = append(errs, err)` from multiple goroutines without a `sync.Mutex`
(or a channel drain) is a data race that `-race` flags and that can corrupt the
slice header under load. Lock around the append, or send each error on a channel
and build the slice in a single consumer, *then* `errors.Join`.

## Trap 1: `errors.Unwrap` does not unwrap a `Join`

`errors.Unwrap(err)` calls **only** `err`'s `Unwrap() error` method. A joined error
exposes `Unwrap() []error`, not `Unwrap() error`, so `errors.Unwrap(errors.Join(a, b))`
returns `nil`. This bites people who reach for `errors.Unwrap` in a loop to
enumerate members — they get nothing. `errors.Is`/`errors.As` know about both
methods, but `errors.Unwrap` does not. To list the members you type-assert
`interface{ Unwrap() []error }` and recurse yourself. That handwritten `Flatten`
is exactly the traversal you need when you must map each leaf error to, say, its
own API error code.

## Trap 2: dropped deferred `Close` errors

`defer f.Close()` on a *write* path silently discards the error `Close` returns —
and for a buffered writer or a committing transaction, that error is where the
flush/commit failure surfaces. Dropping it loses data-loss signals. On a write
path use a named return and a deferred closure that joins the `Close` error into
the function's error, so a failed final flush cannot vanish.

## A joined error is never `==` a member

The aggregate is a distinct value that wraps the members (often double-wrapped, by
`fmt.Errorf` and then by `Join`). `if err == ErrSourceA` is always false against a
joined or wrapped error. Comparison by identity is gone the moment you wrap; use
`errors.Is` for sentinels and `errors.As`/`errors.AsType` for typed errors,
unconditionally.

## The `Error()` string is for humans, not parsers

The joined message's line count and order track input order, which makes it
log-friendly, but it is not an API contract. Do not parse it to decide what
failed — the moment you need to *act* on individual failures, expose structure
(`errors.As` to a typed error, or `Flatten` to enumerate) rather than string-
matching the rendered message.

## Common Mistakes

### Returning only the first error from a loop over independent items

Wrong: `for _, f := range fields { if err := check(f); err != nil { return err } }`
on a validation, shutdown, or batch path. Every other failure is hidden; the client
round-trips repeatedly, or resources after the first leak.

Fix: accumulate into a slice and `return errors.Join(errs...)`. The nil-skipping
and all-nil-is-nil rules mean the happy path still returns nil with no special case.

### Comparing a joined or wrapped error with `==`

Wrong: `if err == ErrSourceA`. It is double-wrapped (by `fmt.Errorf` and by `Join`),
so the identity comparison always fails.

Fix: `errors.Is(err, ErrSourceA)`.

### Assuming `errors.Unwrap` walks a `Join`

Wrong: looping on `errors.Unwrap` to enumerate a joined error's members. It returns
nil immediately because a `Join` has `Unwrap() []error`, not `Unwrap() error`.

Fix: type-assert `interface{ Unwrap() []error }` and recurse to collect the leaves.

### Hand-rolling an aggregate without `Unwrap() []error`

Wrong: a custom multi-error type with an `Errors []error` field and an `Error()`
method but no `Unwrap() []error`. `errors.Is`/`errors.As` cannot see the members.

Fix: implement `Unwrap() []error` on the type (that one method is what makes the
tree walk work), or just use `errors.Join` unless you need to carry metadata.

### Concatenating error strings to "combine" errors

Wrong: `errors.New(strings.Join(msgs, "; "))` to merge failures. The result is an
opaque string; `errors.Is`/`errors.As` matching is destroyed.

Fix: `errors.Join` keeps the tree inspectable while still rendering a readable
message.

### Reaching for `errgroup` when you need every failure

Wrong: using `errgroup` for a health check that must report all unhealthy
upstreams — `Wait` returns only the first error and cancels the rest.

Fix: accumulate per-goroutine errors under a mutex and `errors.Join` them. (The
reverse mistake — hand-rolling aggregation when you actually wanted first-error
plus cancellation — is just as real; pick the policy the requirement demands.)

### Appending to a shared slice from goroutines without a mutex

Wrong: `errs = append(errs, err)` from many goroutines. A data race `-race` flags,
and slice corruption under load.

Fix: guard the append with a `sync.Mutex`, or drain a channel in one consumer, then
`errors.Join`.

### Dropping a deferred `Close` error on a write path

Wrong: `defer f.Close()` when `f` buffers or commits. The flush/commit error
vanishes and you lose data silently.

Fix: named return plus `defer func() { err = errors.Join(err, f.Close()) }()`.

### Calling `errors.As` with a value, or expecting it to find all matches

Wrong: `errors.As(err, fieldErr)` with a value (panics), or assuming it returns
every `*FieldError` in the tree (it returns only the first).

Fix: pass a non-nil pointer (`&fieldErr`) or use `errors.AsType[*FieldError](err)`;
to handle every match, flatten the tree and range over it.

### Returning a non-nil aggregate for a zero-failure batch

Wrong: returning a `*MultiError{Errors: nil}` (a non-nil interface wrapping an
empty aggregate) when nothing failed — callers see a truthy error.

Fix: return untyped `nil` when there are no members, exactly as `errors.Join` does.

Next: [01-source-collector-join.md](01-source-collector-join.md)
