# 3. Strategy Pattern via Interfaces — Concepts

When one piece of code must support several algorithms for the same task — sorting, compression, pricing, authentication, routing — embedding the variants in one growing `if`/`else` or `switch` turns every change into a hunt through unrelated code, and every new variant edits the same hot function. The strategy pattern extracts each algorithm into its own value behind a common contract, so the code that uses an algorithm never names a specific one. Adding a variant becomes new code rather than edited code. This file is the conceptual foundation for the three exercises that follow: the same pricing domain expressed three ways — as interface implementations, as plain function values, and as a runtime registry keyed by string — so you can see what stays constant (the context is blind to the concrete algorithm) and what each encoding buys you.

## Concepts

### The Context, the Strategy, and the Contract

Three roles define the pattern. The strategy is the interchangeable algorithm. The context is the object that holds a strategy and delegates to it without knowing which one it has. The contract is the interface (or function signature) that every strategy satisfies and the context depends on. The defining property is that the context depends only on the contract: it has a field of the contract type, it calls through that field, and it never switches on the concrete strategy. That single indirection is the whole pattern; everything else is encoding detail.

In Go the contract is usually a small interface, and the idiom is to define the interface where it is consumed — in the context's package — rather than where strategies are implemented. This is the inverse of how interfaces work in nominally typed languages: a strategy in another package satisfies the contract structurally, by having the right methods, with no `implements` keyword and no import of the context. New strategies can be written in packages the context has never heard of, which is exactly why the pattern scales.

### Designing the Method Signature

The shape of the contract method is the most consequential decision, because every strategy and every caller is bound by it. A pricing contract that returns only the discount forces every caller to write `total := subtotal - discount`, and that subtraction drifts: one caller forgets to clamp, another rounds differently, and the "single algorithm" has leaked arithmetic into its callers. Returning the full result — both the discount and the resulting total — keeps the computation inside the strategy where it belongs and gives callers nothing to recompute. The rule generalizes: a strategy contract should return everything the caller needs so the caller never has to redo part of the work the strategy was supposed to own.

Keep the contract minimal. A one- or two-method interface is satisfied by the widest range of types, including plain functions (covered below). Every extra method you demand is a method every strategy must implement, including the trivial ones, so resist adding `Name()` or `Describe()` unless the context genuinely needs it. When in doubt, a narrow contract with a separate optional interface (type-asserted when present) beats one fat interface that every strategy pays for.

### Strategies Are Usually Stateless Values

Most strategies are pure functions of their input plus a little configuration: a flat discount is its amount, a percentage discount is its rate. These are values, not objects with a lifecycle. Implementing the contract with value-receiver methods on small structs makes a strategy cheap to copy, safe to store in a slice or map, and free of pointer-aliasing surprises. A `FlatDiscount{Amount: 10}` is a configuration carried in the value itself; two of them with the same amount are interchangeable. Reach for a pointer receiver only when a strategy must mutate shared state across calls — a rate limiter that counts, a cache that fills — and then be deliberate about whether that shared state is safe under concurrent use.

### Functions Are Strategies Too

A strategy with a single method is, in effect, a function. Go lets you skip the struct entirely: declare a named function type matching the contract, and a closure becomes a strategy directly. The standard library's `http.HandlerFunc` is exactly this move — `type HandlerFunc func(ResponseWriter, *Request)` with a method that calls the function, so an ordinary function satisfies the `http.Handler` interface. The function-value encoding shines when the algorithm carries no state worth naming, when you want to build strategies by composing other functions (a decorator that caps a discount is just a function that calls another and adjusts the result), and when an inline closure at the call site is clearer than a top-level type. Its cost is that a bare function has no place to hang auxiliary methods or a stable identity; if the context needs to ask the strategy its name or compare two strategies, the struct-plus-interface form earns its keep.

### Choosing the Strategy at Runtime

Selecting a strategy is one assignment — `ctx.strategy = NewPercentageDiscount(15)` — and the interesting question is where the decision comes from. When it comes from a configuration file, a CLI flag, an HTTP parameter, or a database column, the selector is a string, not a Go expression, and the natural structure is a registry: a `map[string]Strategy` that the program populates at startup and consults at request time. The registry inverts the usual coupling. Adding a strategy is a `Register("key", impl)` call, and the dispatch site — the `switch` that a naive design would grow — disappears into a single map lookup that never changes. A good registry returns a clear error for an unknown key (naming the key and listing the available ones, so a typo in a config file is diagnosable rather than a silent default), and exposes its keys so callers can validate input or render a menu. This is the same shape the standard library uses for `sql.Register` and `image.RegisterFormat`: open extension by key, closed dispatch logic.

## Common Mistakes

### Leaving the Dispatch in the Context

The most common failure is keeping the very `switch` the pattern exists to remove. A `Process(items, kind string)` that switches on `kind` and constructs a strategy inside the method has not applied the pattern; it has renamed the branches. Every new strategy still edits `Process`, the context still depends on every concrete type, and the indirection bought nothing. The fix is to inject the chosen strategy through the constructor or a setter and make the context depend only on the contract, so `Process` never mentions a concrete strategy by name.

### A Contract That Returns Too Little

Designing the method to return a fragment of the result — the discount but not the total, the comparison key but not the ordering — pushes the rest of the computation onto every caller, where it is duplicated and drifts. The contract should return a complete, usable result. If callers consistently combine the strategy's output with the same follow-up step, that step belongs inside the strategy.

### An Interface Wider Than the Context Needs

Demanding `Name()`, `Describe()`, `Validate()`, and `Calculate()` from every strategy means the trivial strategies implement four methods to be used for one. Wide contracts also lock out the function-value encoding, because a bare function can satisfy only a single-method interface. Keep the contract to what the context actually calls; promote rarely needed behavior to a separate, optionally type-asserted interface.

### A Silent Default for an Unknown Registry Key

A registry that returns a zero-value or no-op strategy when a key is missing turns a misconfigured `"percetage"` into a checkout that silently charges full price. Lookups by externally supplied keys must report the miss — an error that names the bad key and lists the valid ones — so the failure surfaces at the boundary instead of as a wrong number downstream.

---

Next: [01-strategy-via-interfaces.md](01-strategy-via-interfaces.md)
