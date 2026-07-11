# If/Else and Init Statements: Guard Clauses in Production Backend Paths

A senior engineer does not learn `if` to print branches. The humble `if`, and
especially its init-statement form, is the correctness spine of every hot path a
service runs: admitting a request, validating startup config, reading a cache,
classifying an error before a retry, enforcing idempotency, checking a version
before a write, deciding whether a rate-limited caller is allowed through, and
aggregating health checks behind a load balancer. This lesson reframes the
init-statement `if` as that primitive, and every exercise is a real artifact a
backend team ships. Read this file once and you have the model behind all nine
independent modules that follow.

## Concepts

### Init-statement scoping binds a value to a single decision

The form `if v := f(); cond { ... }` runs `f()`, binds its result to `v`, and
makes `v` visible only inside the `if`/`else` chain. The moment the chain ends,
`v` is gone. That is not an accident of syntax; it is the point. A value that
matters solely for one decision must not leak into the enclosing function where a
later line could accidentally read a stale copy.

```go
if auth := req.Header.Get("Authorization"); auth == "" {
	return errMissingAuth
}
// auth is not in scope here — good; nobody can misuse it
```

This is one of the few places Go encourages shadowing on purpose. The narrow
scope is a feature: the compiler stops you from reaching a decision-local value
where it does not belong.

### The comma-ok idiom is control flow, not just map access

`v, ok := m[k]` is the most visible use, but the pattern is general: a type
assertion `s, ok := x.(T)`, a channel receive `v, ok := <-ch`, and header-map
presence `_, present := header[key]` all return a value plus a boolean that says
whether the value is meaningful. Folded into an `if` init statement, the lookup
and its success test become one scoped condition:

```go
if e, ok := m[key]; ok {
	// e is valid only here
}
```

The operationally critical corollary: **absent is not the same as present-but-zero.**
A map read returns the zero value for a missing key, so `m[key] == ""` cannot tell
a missing key from a key stored as `""`. Only the `ok` boolean can. In HTTP,
`header.Get("X")` collapses both cases to `""`; to distinguish a header that was
never sent from one sent empty, you must read the header map directly with
comma-ok: `_, present := req.Header["X"]`. Those two conditions produce different
operational signals — a missing credential versus a malformed one — and conflating
them hides bugs.

### Guard clauses and early return flatten a function

A precondition that fails should return immediately, at the top of the function,
at the leftmost indentation. Each guard is one `if ... { return }`. The happy path
then flows straight down the left margin with no nesting. The alternative — an
`else` after a guarded `return` — is structurally dead code: the `return` already
took the failure branch, so the `else` only adds indentation.

```go
// Wrong: else after return
if err != nil {
	return err
} else {
	doWork()
}

// Fix: drop the else
if err != nil {
	return err
}
doWork()
```

A related trap is assuming an inner `if err := ...; err != nil` propagates `err`
to an outer scope. It does not — `err` is bound to that `if` only. Return from the
narrow scope; do not rely on a shadowed variable to carry the value out.

### Typed error values are the contract, not the message string

A decision function that returns a bare `error` and expects the caller to
`strings.Contains(err.Error(), "auth")` is broken by design: the message is for
humans, and wrapping or rewording it silently breaks the caller. The contract is a
typed error. Wrap a sentinel with `%w` (or return a struct that implements
`Unwrap`), and callers use `errors.Is(err, ErrMissingAuth)` to test the cause and
`errors.As(err, &rejection)` to extract structured fields. This is what makes a
decision function usable: the caller reads the decision, not the prose.

```go
type Rejection struct {
	Reason error
	Field  string
}

func (r Rejection) Error() string { return fmt.Sprintf("reject %s: %v", r.Field, r.Reason) }
func (r Rejection) Unwrap() error { return r.Reason }
```

`errors.As` walks the wrap chain looking for a value your target can hold;
`errors.Is` walks it comparing against a sentinel. Both are the reason wrapping
with `%w` matters — a `fmt.Errorf("load %s: %w", key, ErrMissing)` stays matchable
by `errors.Is` while adding context.

### Map a domain decision to a protocol response in exactly one place

A guard that returns only a Go error is half-finished; something must turn each
cause into an HTTP status (or a gRPC code, or an exit code). Put that mapping in a
single `statusFor(err) int` helper built on `errors.As` plus a switch over
`errors.Is`. Scattering the mapping across handlers makes the guard untestable and
lets the mapping drift as handlers are copy-pasted. One function, one source of
truth, one test. The client receives only `http.StatusText(status)`; the full
error is logged server-side so internal field names never leak into a response.

### if/else + init is the correctness core of resilience primitives

The same compact decision shape recurs across every resilience primitive, and each
must be exactly right:

- Rate limiter: refill-then-admit. `if b.tokens < 1 { return false }` else consume.
- Idempotency guard: lookup-then-replay. `if prev, ok := store.Get(key); ok { replay }`.
- Optimistic lock: exists-then-version-check. `if cur.Version != expected { conflict }`.
- Retry client: classify-then-decide. `if isTerminal(err) { return err }` else back off.

Each is one or two `if` statements, and a single wrong comparison is a lost update,
a double charge, or a retry storm against a failing dependency.

### Short-circuit versus collect-all are both legitimate aggregation shapes

When you run a set of checks, two control-flow shapes are correct, and you choose
by what the caller needs. **Fail-fast** returns on the first failure
(`if err := check.Run(ctx); err != nil { return early }`) — right when a single
critical dependency being down means the whole thing is down, and running the rest
wastes time. **Collect-all** accumulates every failure into a report and returns
the full picture — right for a readiness endpoint or a validation pipeline where an
operator wants to see everything wrong at once. `errors.Join` is the standard way
to bundle several failures into one error whose members stay matchable by
`errors.Is`.

### Modern Go loop scoping and deterministic decision tests

Since Go 1.22, each iteration of a `for range` gets its own loop variable, so the
old `tc := tc` capture before a closure or `t.Parallel()` is unnecessary — drop it.
Combined with `t.Parallel()` on subtests and `t.Context()` (Go 1.24+) for
request-scoped cancellation, this shapes how a table-driven decision tree is
tested: one table, parallel subtests, no capture boilerplate. Any decision that
depends on time — a cache TTL, a token-bucket refill — must inject a clock
(`func() time.Time`) so the test controls the instant and never flakes on wall-clock
slack.

### Concurrency safety of a decision path

A read-only package-level policy table (an allowlist of content types, a set of
retryable codes) is race-free: many goroutines reading an immutable slice never
conflict. But any per-request mutable state a decision touches — a hit counter, a
cache map, a token bucket, an idempotency store — must be guarded by a mutex, and
the lookup-then-act sequence must happen inside one critical section. Two
concurrent identical requests that each do `check` then `act` without holding the
lock across both will both act. `go test -race` with a concurrent test is what
proves the discipline; a decision path that is correct single-threaded and racy
under load is not correct.

## Common Mistakes

### Returning a bare error and forcing the caller to guess the status

Wrong: `Check` returns a plain `error` and each handler decides which HTTP status
to send. Fix: a typed `Rejection` plus one `statusFor(err) int` that uses
`errors.As` to read the cause and maps it centrally.

### Treating a missing header and an empty header as the same

Wrong: `if req.Header.Get("Authorization") == ""` and calling it "missing". That
value is `""` both when the header is absent and when it is sent empty. Fix: use
the header-map presence comma-ok (`_, present := req.Header["Authorization"]`) to
distinguish absent from empty, and `strings.TrimSpace` before the emptiness check.
They are different operational signals.

### Writing `else` after a guarded `return`

The `else` branch is structurally dead — the `return` already handled the failure.
Drop it so the happy path stays unindented.

### Assuming an inner `if err := ...` propagates err outward

The `err` is bound to that `if` only. Return from the narrow scope instead of
relying on a shadowed variable to carry the value to an outer scope.

### Reading a response-only header off the request

Per RFC 9110, `Retry-After` is a response header. A guard that parses it off
`*http.Request` models the wrong side of the protocol. Read request headers on the
request; set response headers on the response.

### Using net.Error.Temporary() to decide retryability

`Temporary()` is deprecated and ill-defined. Classify with `Timeout()`,
`errors.Is(err, context.DeadlineExceeded)` / `context.Canceled`, and explicit
sentinels for application error classes instead.

### String-matching error text instead of errors.Is/errors.As

`strings.Contains(err.Error(), "not found")` is brittle: it breaks the moment a
wrapper or reword changes the message. Use `errors.Is` against a sentinel or
`errors.As` to extract a struct.

### A package-level mutable table plus a setter read under concurrency

A `var policy []string` reassigned by a `SetPolicy` while handlers read it is a
data race. Use a read-only table for static policy, or per-instance config injected
via a struct, for anything mutable.

### Sprinkling wall-clock calls through cache/limiter logic

Inline `time.Now()` everywhere makes expiry and refill tests flaky. Inject a clock
(`func() time.Time`) so the test advances time deterministically.

### Doing the lookup and the mutation without holding the lock across both

In idempotency and optimistic-lock paths, if the comma-ok check and the following
mutation are not in one critical section, two concurrent identical requests both
pass the check and both execute the side effect. The lookup and the act must share
the lock.

Next: [01-request-guard-check.md](01-request-guard-check.md)
