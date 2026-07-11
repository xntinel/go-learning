# Function Declaration And Multiple Return Values — Concepts

A function's signature is a contract, and in Go the most information-dense part of
that contract is the return list. There are no exceptions to unwind and no
checked-exception ceremony; instead, the shape of what a function hands back tells
the caller exactly what can happen and what they are obligated to handle. Get the
shape right at the declaration site and a repository, a config loader, or an HTTP
handler composes safely. Get it wrong and you ship silent zero-value bugs: a page
number of `0` that was really "the parameter was missing", a user struct that is
empty because the store was down and nobody noticed. This file is the conceptual
foundation for the ten independent exercises that follow, each of which drills one
return shape on an artifact a senior actually writes in production.

## Concepts

### The signature is the contract; choose the shape at declaration time

When you declare `func FindByID(ctx context.Context, id string) (User, error)`,
you have already told every future caller three things: the call can fail, the
failure carries information (an `error`, not a bare `bool`), and a `User` is only
trustworthy when `err == nil`. You do not get to decide this later. The return
shape is fixed at the declaration, and everyone who composes on top of the
function inherits it. This is why senior review pushes back hardest on
signatures: a wrong return shape is a design defect that propagates to every call
site, and changing it later is a breaking change to the contract.

### `(value, error)`: the operation can fail, and the failure is informative

This is the canonical fallible shape. `error` is an interface; it is `nil` on
success and non-nil on failure, and the non-nil value carries a message and,
through wrapping, a whole chain of causes. The rule is that `value` is only valid
when `err == nil`; on the error path the caller must treat `value` as
meaningless (usually the zero value, but never rely on that). Ignoring the error
is a bug, and Go makes ignoring it visible: the compiler keeps the `err` variable
in scope, and `errcheck`/`go vet`-adjacent linters flag `v, _ := f()`. Use this
shape whenever a failure has a reason a human or a caller would want to know.

### `(value, ok)`: expected, non-exceptional absence

Some absences are not failures. A map key that is not present, a comma-ok type
assertion that does not match, a query parameter the client simply did not send —
these are normal outcomes, part of the contract, and the caller must not log them
as errors or wrap them as failures. Go encodes this with the `(value, ok)` shape,
where `ok` is a plain `bool`: `v, ok := m[key]`, `v, ok := x.(T)`,
`v, ok := q.First("page")`. The signal to the caller is "absence is fine, branch
on it, do not treat it as broken". The two built-in `(value, ok)` sites — map
index and type assertion — are the templates every hand-written `(value, ok)`
accessor imitates.

### Not-found versus failure: when a bool is not enough

The trap is using `(value, bool)` where the `bool` is secretly doing the job of an
error. Consider a repository read. Two very different things can go wrong: the row
does not exist (a 404, a normal outcome), or the database connection is broken (a
500, an operational failure you must alert on). A single `bool` collapses these
into one indistinguishable "false", and the caller cannot tell "no such user"
from "the store is on fire". The correct encoding is a `(value, error)` signature
plus a dedicated *sentinel* error for the not-found case, exactly as
`database/sql` does with `sql.ErrNoRows`. The caller writes
`if errors.Is(err, ErrNotFound)` to map absence to 404, and any other non-nil
error to 500. The sentinel lets one `error` return channel carry both meanings
without ambiguity.

### Error wrapping with `%w` preserves the chain

`fmt.Errorf("parse %q: %w", key, err)` produces a new error whose message adds
context and whose *cause* is still reachable. `errors.Is(err, target)` walks that
chain looking for a match against an exported sentinel —
`strconv.ErrSyntax`, `strconv.ErrRange`, `sql.ErrNoRows`, `context.Canceled` —
and `errors.As(err, &target)` extracts a typed error from it. Wrapping with `%w`
(not `%v`) is what keeps `Is`/`As` working; `%v` flattens the cause into a string
and severs the chain. One honest limitation to internalize:
`time.ParseDuration` returns an *unexported* error type and exports no sentinel,
so neither `errors.Is` nor `errors.As` can match it — for that one you assert on
the message. That is a real stdlib gap, not a mistake in your code, and knowing
which stdlib errors expose a sentinel is part of using this shape well.

### Functions are first-class values

A `func` is a type as concrete as `int`. Functions can be passed as parameters (a
retry helper takes `func() (T, error)`), returned from other functions (a
constructor hands back a `func() error` cleanup closure), and stored in named
types (`type HandlerFunc func(http.ResponseWriter, *http.Request) error`). This is
how Go composes behavior without inheritance: instead of subclassing to override,
you pass the varying behavior as a function value. The multiple-return convention
and first-class functions reinforce each other — a `func() (T, error)` value is a
"fallible computation" you can hand to a scheduler, a retryer, or a cache, and the
tuple threads through unchanged.

### The cleanup-func return pattern

A constructor that acquires a resource often returns the release for it:
`Open(cfg) (*Resource, func() error, error)`. The caller writes
`r, cleanup, err := Open(cfg); if err != nil { return err }; defer cleanup()`.
The ordering contract is strict and easy to get wrong: on the error path the
constructor must return `(nil, nil, err)` and release any partially-acquired
resource *internally*, so the caller never defers a nil or half-built cleanup. A
non-nil cleanup alongside a non-nil error is a landmine — the caller either
skips the defer (leak) or defers a closure over a resource that was never fully
built (panic or double-free). This pattern shows up for DB pools, temp files,
tracing spans, and test fixtures.

### Variadic parameters are a slice inside the function

`func InClause(col string, args ...any) (string, []any)` receives `args` as a
plain `[]any` inside the body. A caller passes individual arguments
(`InClause("id", 1, 2, 3)`) or spreads an existing slice with `slice...`
(`InClause("id", ids...)`). Spreading is a distinct operation: passing the slice
without `...` makes it a single argument of type `[]any`, which is usually a bug.
The empty-variadic edge is a real hazard — an IN-clause built from zero args must
never emit `col IN ()`, which is a SQL syntax error; you return an always-false
predicate instead. Variadic is a convenience at the call site, not a licence to
skip the zero-length case.

### Unpacking rules: arity, `:=` versus `=`, and reusing `err`

The number of variables on the left must exactly match the function's return
arity — `v := Int(q, "page")` on a two-return function fails to compile with
"multiple-value in single-value context", and that compile error is a feature. `:=`
introduces new variables; `=` assigns into existing ones. The idiom for a
sequence of fallible calls is to declare with `:=` on the first and reuse the one
`err` with `=` after that: `page, err := Int(q, "page"); ...; timeout, err =
Duration(q, "timeout")`. As long as at least one variable on the left of a `:=` is
new, the others may already exist. Naming matters too: name returns for their
meaning (`host`, `port`, `cleanup`, `err`), never `error` (which shadows the
builtin type) and not a wall of `res`/`ok`.

### Chaining fallible calls

Real parsing threads one call's success value into the next: `ParseEndpoint`
calls `net.SplitHostPort`, and only if that succeeds does it call `strconv.Atoi`
on the port string, and only then does it range-check. Each step early-returns on
its own error, and each error is wrapped with `%w` and enough context that the
final message traces the failure path. The three-value return `(host, port, err)`
names each purpose, so the caller reads intent off the signature.

### Generics keep a multiple-return helper type-safe

Without generics, a reusable retry helper would have to return `(any, error)` and
force every caller to type-assert. `Retry[T any](ctx, attempts, fn func() (T,
error)) (T, error)` preserves the concrete type of the wrapped tuple: retrying a
`func() (User, error)` returns a `User`, not an `any`. The same applies to a
`(value, ok)` extractor: `Field[T any](payload map[string]any, key string) (T,
bool)` gives back a typed value and a bool without collapsing to `any`. Generics
let the multiple-return shapes stay precise across payload and result types.

## Common Mistakes

### Using `(value, bool)` where the bool really means "an error happened"

Wrong: `func Get(key string) (int, bool)` where `false` conflates "not present",
"invalid syntax", and "backend down". The caller cannot pick the right status
code or decide whether to alert.

Fix: return `(value, error)`, and add a not-found sentinel so `errors.Is(err,
ErrNotFound)` distinguishes the normal-absence case from a real failure. Reserve
`(value, ok)` for genuinely non-exceptional absence.

### Ignoring the error because it is "probably nil"

Wrong: `v, _ := Int(q, "page")`. If the key is missing the function returns
`(0, err)`, and the caller now uses `0` as a real page number.

Fix: handle the error on the spot. If a function returns an `error`, that error is
part of the contract, not decoration.

### Unpacking into the wrong arity

Wrong: `v := Int(q, "page")` on a two-return function — "multiple-value in
single-value context". Forgetting or adding a return value is a compile error by
design.

Fix: match the arity exactly — `v, err := Int(q, "page")`, and use `=` when a
variable is already declared.

### Naming a return `error` or naming everything `res`/`ok`

Wrong: `func Parse() (error string, ok bool)` shadows the builtin `error`;
`func f() (res T, ok bool)` says nothing.

Fix: name returns for meaning — `host`, `port`, `cleanup`, `value`, `err`.

### Single-return type assertion that panics

Wrong: `v := x.(T)` on a value that might not be a `T` panics at runtime. This
bites hardest on `map[string]any` decoded from JSON.

Fix: use the comma-ok form `v, ok := x.(T)` on any dynamically-typed value and
branch on `ok`.

### A constructor returning a partial resource or a non-nil cleanup on error

Wrong: returning `(res, cleanup, err)` with a non-nil `err` and a non-nil
`cleanup` (or a non-nil `res`). The caller defers a half-built cleanup or leaks
the partial resource.

Fix: on the error path return `(nil, nil, err)` and release any partial resource
internally, so the success path is the only one that produces a cleanup.

### Emitting `col IN ()` for the empty-variadic case

Wrong: building the IN-clause by joining placeholders unconditionally, so zero
args yields `col IN ()` — a SQL syntax error.

Fix: special-case zero args and return an always-false predicate (for example
`1=0`) with an empty bind slice.

### Forgetting to spread a slice into a variadic sink

Wrong: `InClause("id", ids)` passes the whole `[]any` as one argument; or the
reverse, spreading `ids...` into a parameter that is not variadic.

Fix: spread with `ids...` into a variadic parameter; pass individual values
otherwise. The `...` is load-bearing.

### A retry loop that ignores context cancellation

Wrong: a loop that sleeps between attempts without watching `ctx.Done()`. A
cancelled request keeps burning attempts and wall-clock time.

Fix: `select` on `ctx.Done()` during the backoff wait and return `ctx.Err()`
promptly when the context is cancelled.

### Assuming JSON integers decode as `int`

Wrong: `Field[int](payload, "count")` on a value that came from
`json.Unmarshal` into `map[string]any`. `encoding/json` decodes every JSON number
as `float64`, so an `int` comma-ok assertion silently fails.

Fix: assert `float64` (or decode into a typed struct, or use `json.Number`), and
document that JSON integers arrive as `float64`.

Next: [01-typed-accessors-value-error.md](01-typed-accessors-value-error.md)
