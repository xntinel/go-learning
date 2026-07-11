# Switch Statements: Dispatch, Classification, and State Guards — Concepts

`switch` is the control-flow construct a senior backend engineer reaches for
dozens of times a day without thinking about it: routing a request by method,
mapping a domain error to an HTTP status, deciding whether an outbound call is
worth retrying, gating writes during a maintenance window, resolving a tenant's
rate limits. What separates a robust service from a fragile one is often not
*whether* a switch is used but *which form*, *how the cases are ordered*, and
*what the default does when the input is something nobody anticipated*. This
file is the conceptual spine of the lesson; read it once and every one of the
independent exercises that follow becomes a variation on the same small set of
judgments.

## Concepts

### Three switch forms, and when each one fits

Go has three shapes of `switch`, and choosing among them is a design decision,
not a stylistic one.

The **expression switch** compares a tag value against each case with `==`:

```go
switch kind {
case KindJSON:
	return decodeJSON(r)
case KindForm:
	return decodeForm(r)
default:
	return ErrUnknownKind
}
```

Use it when you dispatch on a *value drawn from a closed set* — a typed enum, an
HTTP method string, a parsed media-type Kind. Because the comparison is `==`, an
expression switch cannot express a range, a prefix test, or a wrapped-error
identity. The moment you need `>=`, `strings.HasPrefix`, or `errors.Is`, this
form is the wrong tool.

The **tagless switch** (also called the expressionless or first-true switch)
drops the tag; each case is a boolean expression and the first one that
evaluates true wins:

```go
switch {
case status >= 500:
	return retryable
case status == http.StatusTooManyRequests:
	return retryableWithBackoff
case status >= 400:
	return permanent
}
```

Use it for *predicate dispatch on a property of the value*: numeric ranges,
`errors.Is` chains, rune classification, `strings.HasSuffix`. It is the honest
replacement for a chain of `if/else if`, and reads better because the parallel
structure is explicit. (Note the deliberate bug above — see case ordering.)

The **switch-with-init** attaches a short statement that runs before the tag is
evaluated, exactly like `if x := f(); cond`:

```go
switch day := now.Weekday(); day {
case time.Sunday:
	return frozen
default:
	return open
}
```

The variable `day` is scoped to the switch and its cases only; it does not leak
into the surrounding function. Reach for this form for the common
resolve-then-branch pattern — compute a tier, a weekday, a parsed value, then
switch on it — when you do not want that intermediate name living any longer
than the branch that uses it.

### Go switch does not fall through

Cases are independent. There is no implicit fall-through from one case into the
next, so there is no `break` to remember and no accidental C-style bug where a
missing `break` runs two case bodies. In a tagless switch the *first* matching
case wins and the rest are skipped.

When you genuinely want to continue into the next case body, the keyword is
`fallthrough`, and it has three properties that surprise engineers arriving from
other languages: it is **explicit** (you have to type it), it is
**unconditional** (it jumps straight into the next case body *without*
re-testing that case's expression), and it is **forbidden in the final case and
in a type switch**. Because `fallthrough` does not re-evaluate the next
condition, using it to "share a body between two similar cases" is a trap; it
runs the next body whether or not that case would have matched. To share a body,
use a comma-separated case list or extract a helper (below), not `fallthrough`.

### Comma case lists are OR, not fall-through

`case A, B:` matches when the tag equals `A` *or* `B`, and runs the one body
once. This is the idiomatic way to group values that share behaviour:

```go
case http.MethodGet, http.MethodHead:
	return d.read(r)
```

It is not fall-through and carries none of `fallthrough`'s hazards. Whenever you
are tempted to duplicate a case body or reach for `fallthrough`, ask first
whether a comma list expresses the intent directly.

### Case ordering matters only when predicates overlap

In an expression switch over disjoint values, order is irrelevant — no two cases
can match the same tag, so the compiler is free and so are you. In a tagless
switch, order is load-bearing exactly when two predicates can both be true for
the same input. The classic bug: putting a broad `case status >= 400` before a
narrow `case status == 429`, so 429 is swallowed by the broad case and its
special retry handling never runs. When cases overlap, the specific case must
precede the general one, and that ordering should be a deliberate, commented
decision, not an accident of how you happened to type them.

### The default case is a fail-closed control, not a formality

The compiler never forces a `default`. That is precisely why it is a design
responsibility. On a closed dispatcher over an enum, `default` is your
exhaustiveness backstop: when an input arrives that none of the enumerated cases
expected, `default` decides what happens. The rule for anything
security-sensitive or cost-sensitive is **fail closed**: an unknown plan resolves
to the *safest* (most restrictive) limits, not to unlimited; an unknown error
maps to 500, not to 200; an unrecognized state transition is rejected, not
silently allowed. A `default` that does nothing — a silent no-op — is how an
unexpected value slips through and corrupts state in production. Your tests must
assert the default branch, because the compiler will not.

### Switch as an exhaustiveness signal over a closed enum

Switching on a small `iota` enum is more than dispatch: it documents the closed
domain of a type. The forgotten-case problem — you add `StateRefunded` to the
enum six months later and forget to add it to one of the four switches that
branch on order state — is a real production incident waiting to happen. Two
defenses: a test that drives *every* enum value through the function (so a
newly-added value with no case surfaces immediately), and the `exhaustive`
linter, which can enforce in CI that a switch over an enum type covers all its
constants. Treat "switch over an enum" as a place that needs a
grows-with-the-enum test.

### Media types and typed enums are the two canonical subjects

Two patterns dominate switch usage in backend code. First, **never `==` a raw
`Content-Type` header**: real headers carry parameters
(`application/json; charset=utf-8`), so a string compare breaks the first time a
compliant client sets a charset or a multipart boundary. Parse with
`mime.ParseMediaType` and switch on the returned media type. Second, **prefer a
typed `Kind`/`State` enum over stringly-typed switches**: a typed enum lets the
compiler and a `Stringer` help you, makes the closed set visible, and turns the
default branch into a meaningful exhaustiveness check instead of a catch-all for
arbitrary strings.

### Normalize stringly input before the switch

When the subject is an unavoidably stringly value — a `LOG_LEVEL` env var, an
HTTP method, a user-supplied enum — remember that `==` and case matching are
exact: case-sensitive and whitespace-sensitive. `"Debug"`, `" debug"`, and
`"DEBUG"` are three different strings. Normalize with
`strings.ToLower(strings.TrimSpace(raw))` *before* the switch, or the case list
silently misses valid input and the default swallows it.

## Common Mistakes

### Omitting the default on a closed dispatcher

Wrong: a `switch kind { case ...: ... }` with no `default`. When `Classify`
returns a Kind you did not anticipate, the switch falls out the bottom and the
function returns a zero value as if nothing happened.

Fix: always add a `default` that returns an error or the safest fallback. The
compiler will not force it; your test suite must.

### Comparing a raw Content-Type header with ==

Wrong: `if r.Header.Get("Content-Type") == "application/json"`. This is false the
moment a client sends `application/json; charset=utf-8`.

Fix: `mime.ParseMediaType` first, then switch on the parsed media type (and test
the `+json` suffix for vendor types).

### Reaching for fallthrough to share a body

Wrong: two cases with the same work, joined by `fallthrough` to avoid
duplication.

Fix: use a comma case list (`case A, B:`) when the values share a body, or
extract a named helper. `fallthrough` is unconditional and hides intent; it is
not a code-sharing tool.

### Assuming fallthrough re-tests the next case

Wrong: expecting `fallthrough` to jump to the next case *only if* that case's
condition holds. It does not — it jumps unconditionally into the next case body,
running it regardless.

Fix: if the next branch should run conditionally, it needs its own `if` or its
own case, not `fallthrough`.

### Ordering overlapping predicates by accident

Wrong: `case status >= 400:` before `case status == 429:` in a tagless switch, so
429 never reaches its special handling.

Fix: put the specific predicate before the general one, and comment the ordering
as intentional wherever two predicates can both match.

### Using an expression switch where you need ranges or errors.Is

Wrong: contorting an expression switch with a `switch true {` or boolean tags to
fake range or wrapped-error dispatch.

Fix: use the tagless form directly. It exists for exactly this — `>=`,
`HasPrefix`, `errors.Is`.

### Letting the default grant more privilege than intended

Wrong: an unknown plan falling through to unlimited limits, or an unknown status
defaulting to retry-forever.

Fix: on any security- or cost-sensitive switch, the default must fail closed —
safest limits, permanent failure, rejected transition.

### Forgetting to normalize stringly input

Wrong: `switch raw { case "debug": ... }` against a `LOG_LEVEL` that arrives as
`" Debug"`.

Fix: normalize with `strings.ToLower(strings.TrimSpace(raw))` before the switch.

### Not updating the switch when the enum grows

Wrong: adding a new enum constant and leaving one of the switches that branch on
it without a case, so the new value silently hits the default (or falls out).

Fix: a test that ranges over every enum value through the function, or the
`exhaustive` linter in CI, so the gap is caught the moment the enum changes.

Next: [01-content-type-classifier.md](01-content-type-classifier.md)
