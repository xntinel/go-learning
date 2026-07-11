# 2. Builder Pattern — Concepts

A builder separates the construction of an object from its final representation. When a value has many fields, when some fields are required and others optional, or when the validation rules depend on several fields at once ("if TLS is on, a certificate and key are required"; "a GET request cannot carry a body"), a constructor with a long positional argument list becomes unreadable and a bare struct literal cannot enforce the invariants. A builder accumulates the configuration across a sequence of small, named methods and turns it into a finished value in one place. Read this file once and you have the reasoning behind all three exercises, which build three different flavours of builder as independent, self-contained Go modules.

## Concepts

### What A Builder Actually Is

A builder is a temporary object whose only job is to gather the inputs for some other object and then assemble it. The builder is mutable and throwaway; the thing it produces — the product — is the value the caller keeps. Keeping these two roles distinct is the whole idea. The caller talks to a small, forgiving, step-by-step interface while building, and walks away with a finished, validated, often immutable product. The construction logic that would otherwise be smeared across the call site, or hidden inside a constructor with eight parameters, lives in one named place.

The fluent style, where each method returns the builder so calls can be chained, is the most recognisable form: `New().Method("POST").URL(...).Body(...).Build()` reads like a sentence. But chaining is a surface convenience, not the essence. The essence is the separation of "describe what you want" from "produce it", and the freedom that gives the builder to validate, to default, to reorder, and to enforce ordering.

### Builder Versus Functional Options Versus A Plain Struct

These three are the realistic alternatives, and choosing well matters more than any implementation detail.

- A plain struct literal suits a small, fully-populated value with no cross-field rules: `Point{X: 1, Y: 2}`. There is nothing to validate and nothing to default, so a builder would only add ceremony.
- Functional options (covered in the previous lesson) suit a value with a small, fixed set of *optional* knobs where the defaults serve most callers: `New(WithTimeout(5*time.Second))`. The options compose, the zero call is the common case, and the API can grow without breaking callers.
- A builder earns its place when construction has *cross-field invariants*, a mix of *required and optional* inputs, several *aggregation points* (headers added one at a time, query parameters accumulated), or when you want the construction itself to read as a sequence. The builder is the only one of the three that can naturally hold partial state, accumulate a list of problems, and validate everything at once at the end.

None of these is "the advanced one". A struct literal is the right answer far more often than a builder. Reach for a builder when the construction is genuinely complex, not because it looks sophisticated.

### Where Validation Lives: Aggregate At Build, Not At Each Setter

A tempting but poor design pushes every check into the setter that sets the field, so the chain stops at the first mistake and the caller fixes one error, recompiles, and discovers the next. The builder pattern instead lets setters *record* problems and runs the cross-field checks in `Build`, then reports every problem together. Two ideas make this clean in Go.

First, define sentinel error values (`ErrEmptyURL`, `ErrBodyOnGet`) and wrap them with `%w` so each failure stays independently testable with `errors.Is`, rather than string-matching a message that any reword would break. Second, collect the failures in a slice and combine them with `errors.Join`, which produces one error that still answers `errors.Is` for every cause. The caller sees the whole picture in a single `Build` call:

```go
if len(errs) > 0 {
	return nil, fmt.Errorf("request: %w", errors.Join(errs...))
}
```

A subtle correctness point: the build-time checks must not be appended to the builder's own persistent error slice, or a second `Build` on a reused builder re-reports stale problems. Compute the build-time errors into a fresh local slice that starts as a copy of the setter errors. The first exercise does exactly this.

### Making Invalid Construction Impossible: The Staged Builder

A validating builder catches a missing required field *at run time*, when `Build` returns an error. A more powerful technique catches it *at compile time*. The trick is to give each construction stage its own interface type, where each required step exposes exactly the next method and returns the interface for the stage after it. `New` returns the first-stage interface, so the only method the caller can reach is the first required setter; that setter returns the second-stage interface; and so on. Only after every required field is supplied does the final interface expose the optional setters and `Build`.

Because the type the caller holds never has a `Build` method until the required fields are set, code that forgets a required field does not compile — there is no run-time error to test for, because the invalid program cannot be written. One unexported concrete struct satisfies all the stage interfaces and simply returns itself, retyped, at each step. The cost is more interfaces and a fixed ordering, so this form pays off when the required set is small and stable and the safety is worth the rigidity. It is sometimes called a *step* or *staged* builder.

### Immutability And Thread-Safety: The Value Builder And The Director

The fluent pointer-builder is mutable and not safe to share: two goroutines chaining methods on the same `*Builder` race on its fields, and `go test -race` will say so. There are two honest responses. One is to document that builders are single-goroutine and create a fresh one per goroutine. The other, more interesting, is to make the builder a *value* type whose setters take the receiver by value, mutate their own copy, and return it. Each step then yields an independent copy, so a base builder can be shared as a starting point and *forked* freely across goroutines with no mutex and no race — copying is the synchronisation. This works cleanly only when the product's fields are scalars or are themselves copied deeply; a value builder that holds a slice still shares that slice's backing array between copies, which reintroduces aliasing.

Once construction recipes recur — "the production configuration", "the bare internal message" — it is worth naming them. A *Director* is the classic Builder-pattern role that captures a fixed recipe: which steps, in which order, with which arguments. In Go a Director is naturally just a function from a starting builder to a finished one, `func(Builder) Builder`. It keeps the recipe in one place, separate from the builder, which only knows how each individual step mutates state. The same base can be fed through different Directors to produce a development config and a production config from one source of truth.

### The Product Is The Caller's; The Builder Is Spent Or Reusable — Decide

After `Build` returns, the product belongs to the caller. The builder's own lifecycle is a design decision you must make explicitly: is it spent (further use is a bug), reusable (build, tweak, build again), or immutable (the value-builder, where there is no shared state to spend)? Whichever you choose, make it true in the code and state it in the doc comment. The most common quiet bug is a caller who assumes a pointer-builder is spent after `Build`, mutates it, builds again, and ships a different request than they meant to.

## Common Mistakes

### Failing Fast In Setters Instead Of Aggregating

Wrong: `func (b *B) URL(u string) *B { if u == "" { return ... } }` with a bare `errors.New` in every setter, then `strings.Contains(err.Error(), "URL")` in the test. The caller fixes one problem at a time, and any reword of a message breaks a test for no behavioural reason. Fix: setters record into a slice, `Build` runs cross-field checks and joins everything with `errors.Join`; tests assert with `errors.Is` against sentinels wrapped with `%w`.

### Appending Build-Time Errors To The Builder's Own Slice

Wrong: `Build` does `b.errs = append(b.errs, ErrEmptyURL)`. The error now sticks to the builder, so a reused builder re-reports a problem the caller already fixed. Fix: copy the setter errors into a fresh local slice inside `Build` and append the build-time checks to that, leaving the builder's own slice untouched.

### Sharing A Mutable Builder Between Goroutines

Wrong: handing one `*Builder` to several goroutines that each chain setters and call `Build`. The map and slice writes race; the result is non-deterministic and `go test -race` flags it. Fix: either create a fresh builder per goroutine, or use the value-builder form whose copy-on-write setters make forking safe by construction.

### Reaching For A Builder When A Struct Literal Would Do

Wrong: writing a fluent builder for a three-field value with no validation and no optional fields. It is more code, more surface, and more to test, with nothing gained. Fix: use a struct literal, or functional options if a couple of fields are optional. A builder is justified by cross-field invariants, required/optional mixes, or accumulation — not by wanting to look thorough.

### A Value Builder That Holds A Slice And Calls Itself Immutable

Wrong: a value-receiver builder whose product contains a `[]string`, then claiming two forks are independent. They share the slice's backing array, so an `append` in one fork can be visible in the other. Fix: keep value-builder products to scalar fields, or copy the slice in the setter (`append([]string(nil), old...)`) before mutating, and only then claim independence.

---

Next: [01-fluent-request-builder.md](01-fluent-request-builder.md)
