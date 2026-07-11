# Schema-First GraphQL with gqlgen — Concepts

GraphQL is not a convenience layer bolted onto a service; from a senior backend
seat it is a public, client-shaped attack surface. A client sends one query and
composes exactly the response it wants from your type graph — which also means a
client can compose an arbitrarily deep and expensive query from a perfectly legal
schema. The value gqlgen adds over the reflection-based alternatives is that it is
*schema-first with code generation*: you write the `.graphqls` schema, gqlgen
generates Go that binds every schema field to a typed resolver method, and any
drift between the contract and your implementation becomes a Go compile error
rather than a runtime 500. This file is the conceptual foundation for the three
exercises that follow: a real query+mutation service, the production hardening it
needs before it faces the internet, and the custom-scalar / nested-resolver work
that sets up the N+1 problem you pay off in lesson 05.

## Concepts

### Schema-first codegen versus code-first reflection

There are two families of Go GraphQL libraries. Code-first libraries
(`graphql-go`, `graph-gophers`) derive the schema from Go types and resolver
signatures at runtime through reflection or builder calls; the schema is a
*consequence* of your code, and it can drift from what you intended without any
compiler noticing. gqlgen inverts this. The `.graphqls` SDL is the single source
of truth. `gqlgen generate` reads it and emits three things: an
`ExecutableSchema` (the machine that parses, validates, plans, and executes
queries against your schema), a `Config` struct that carries your `Resolvers`
(and optionally per-field complexity functions), and — unless you bind your own
types — a set of Go model structs. Because the generated code declares a resolver
*interface* whose method set is dictated by the schema, if the schema says
`createProduct(input: NewProduct!): Product!` and your resolver has the wrong
signature, the package does not build. Contract drift is a compile error. That is
the whole reason to prefer gqlgen for a contract that other teams depend on.

### The generated wiring, and who owns which file

`graph.NewExecutableSchema(graph.Config{Resolvers: r})` returns the executable
schema you hand to a server. The root `Resolver` is the object you own: it holds
your dependencies (stores, clients, loaders) and returns per-type sub-resolvers
through generated binding methods like `func (r *Resolver) Query() QueryResolver`.
The `follow-schema` layout is the one to use: on every regeneration gqlgen stubs
*new* resolver methods into `schema.resolvers.go` and leaves your existing
implementations untouched, so regenerating after a schema change never clobbers
your logic. The hard rule is ownership: `generated.go` and `models_gen.go` are
machine-owned. Editing them by hand is a mistake that the next `gqlgen generate`
silently reverts. You edit the schema and the resolver file; you never edit the
generated files.

### Autobind versus generated models

By default gqlgen generates a parallel set of model structs (`model.Product`,
`model.NewProduct`) from the schema. That is fine for input types, but for your
core domain objects it forces resolvers to speak a generated vocabulary that
duplicates types you already own. The `models:` and `autobind:` config lets you
map a GraphQL type to your own package's struct, so a resolver returns
`*domain.Product` — your real domain type — instead of a generated twin. gqlgen
matches schema fields to struct fields by name; any schema field with no matching
struct field automatically becomes a *field resolver* method you implement. That
last fact is the hinge for the N+1 discussion below.

### handler.NewDefaultServer is documented as not production-suitable

gqlgen ships `handler.NewDefaultServer` for the five-minute demo, and its own
documentation is explicit that it is not for production. It wires no query cache,
imposes no complexity limit, and leaves introspection on. Production wiring is a
deliberate act you perform with `handler.New` plus explicit calls:

```text
srv := handler.New(graph.NewExecutableSchema(cfg))
srv.AddTransport(transport.Options{})
srv.AddTransport(transport.GET{})
srv.AddTransport(transport.POST{})
srv.SetQueryCache(lru.New[*ast.QueryDocument](1000))
srv.SetErrorPresenter(...)   // mask internals
srv.SetRecoverFunc(...)      // panics -> safe error
srv.Use(extension.FixedComplexityLimit(limit))
if introspectionAllowed { srv.Use(extension.Introspection{}) }
```

Two of these are easy to forget with confusing consequences. Transports are
opt-in: if you never `AddTransport(transport.POST{})`, a POST request is answered
with HTTP 422 "transport not supported", which looks like a broken endpoint
rather than a missing line. And `SetQueryCache(lru.New[*ast.QueryDocument](N))`
is what stops the server re-parsing and re-validating the identical query
document on every request — without it, parsing is per-request work an attacker
(or a busy client) can amplify.

### GraphQL error semantics invert the status-code intuition

This is the single most surprising thing for a backend engineer arriving from
REST. Once a request is transported and parsed, a GraphQL response is almost
always HTTP 200 — even when a resolver failed. Field errors do not live in the
status code; they live in a top-level `errors` array in the JSON body, alongside
whatever `data` could still be resolved (partial data). So alerting and log
correlation that key off non-2xx status codes will silently miss field failures
entirely. Worse, error propagation has a blast radius: a resolver error on a
*non-null* field cannot be represented as null, so GraphQL propagates the error
*up* to the nearest nullable ancestor and nulls that whole subtree. A single
failing non-null leaf can null an entire parent object. Your schema's nullability
choices are therefore also failure-isolation choices.

### Error hygiene: presenter, recover, and multi-error accumulation

Because resolver errors flow straight to clients, an unconfigured server leaks
whatever string your error carried — a database driver message, a wrapped `sql`
error, an internal path. `SetErrorPresenter` is the choke point. It is built on
`graphql.DefaultErrorPresenter(ctx, err)`, which produces a `*gqlerror.Error`
with the path already filled in; you then inspect the original error with
`errors.As` (to match a typed, client-safe error) and either keep a curated
message plus an `extensions` code, or mask everything else behind a generic
"internal server error". `SetRecoverFunc` is the panic equivalent: a resolver
that panics would otherwise crash the request; the recover func turns the panic
into a generic `gqlerror` so a bug becomes a safe error instead of a stack trace
on the wire. For a list field that partially fails, you do not return one error —
you call `graphql.AddError(ctx, err)` (or `AddErrorf`) once per failing element,
using `graphql.GetPath(ctx)` so each error carries the correct array path, and
still return the successful elements.

### Query cost is a denial-of-service vector

A legal schema permits illegal-in-spirit queries. Nesting `product { reviews {
product { reviews { ... } } } }` or requesting a list field with a huge argument
lets one client demand work proportional to the depth and fan-out they chose, not
to anything you sized for. `extension.FixedComplexityLimit(limit)` assigns a cost
to a query *before any resolver runs* and rejects it if the total exceeds the
limit — a rejected over-complex query never touches your store or database.
Options tune the model: `complexity.WithFixedScalarValue(n)` sets the per-scalar
cost, `complexity.WithIgnoreFields(m)` zeroes named fields, and for a field whose
cost scales with an argument you set a per-field function on
`Config.Complexity`, e.g. `cfg.Complexity.Query.Batch = func(childComplexity int,
ids []string) int { return childComplexity * len(ids) }`. Complexity limiting
pairs conceptually with depth limiting; both exist because the client, not you,
writes the query.

### Introspection is attack surface

Introspection (`__schema`, `__type`) lets any client download your entire schema:
every type, field, argument, and deprecation. That is exactly what the playground
and code generators consume, and exactly what an attacker mapping your API wants.
`extension.Introspection{}` is therefore opt-in, and for a public deployment you
gate it behind an environment flag or role check — on in development for the
playground, off (or restricted) in production.

### Custom scalars: a two-function contract

A custom scalar (a `Money`, a `Time`, a `UUID`) is defined by a marshaler and an
unmarshaler. `MarshalXXX(v T) graphql.Marshaler` returns something that writes the
wire representation, usually `graphql.WriterFunc(func(w io.Writer){ ... })`. The
unmarshaler, `UnmarshalXXX(v interface{}) (T, error)`, is where the sharp edge
lives: `v` is whatever the JSON decoder produced, and a client can send more
shapes than you expect. A JSON number arrives as `float64`, not `int`; a quoted
literal arrives as `string`; you may also see `json.Number`, `int`, or `bool`.
An unmarshaler that only handles `string` will panic or misbehave the first time
a client sends a number. Handle every plausible shape, and return a real error
(never a silent zero) for input you cannot parse — that error becomes a clean
GraphQL error rather than corrupt data.

### Resolvers run concurrently

gqlgen executes sibling field resolvers concurrently, bounded by a worker limit.
Two fields on the same object, or the elements of a list, can resolve on
different goroutines at the same time. Any state your resolvers share — a store, a
counter, a cache — must be synchronized. An unguarded map read/written across
sibling resolvers is a data race that `-race` will catch and production will hit
under load.

### N+1: the nested field resolver trap

Put the previous two facts together with autobind. When your domain struct lacks
a schema field, that field becomes a resolver method that runs *once per parent
object*. So a query `products { reviews { ... } }` resolves the list of N
products, then invokes the `Product.reviews` resolver N separate times — N
independent fetches for related data. That is the canonical N+1: one query for the
parents plus one per parent for the children. It is invisible in a demo with three
products and melts a database under real fan-out. Naming it here is deliberate:
lesson 05 batches those N child fetches into one with dataloaders. Exercise 3
builds exactly this N+1 and instruments it so you can watch the fetch count grow
with the parent count.

## Common Mistakes

### Shipping handler.NewDefaultServer to production

Wrong: `srv := handler.NewDefaultServer(es)` because it is one line. It leaves no
query cache, no complexity limit, and introspection wide open — a self-inflicted
DoS surface with a schema map handed to anyone who asks.

Fix: use `handler.New` and add transports, an LRU query cache, an error
presenter, a recover func, and a complexity limit explicitly; gate introspection.

### Forgetting AddTransport

Wrong: wiring `handler.New` but never calling `AddTransport(transport.POST{})`,
then debugging why every request returns HTTP 422 "transport not supported".

Fix: register the transports you serve (`Options`, `GET`, `POST` at minimum) —
transports are opt-in, not on by default with `handler.New`.

### Returning raw internal errors to clients

Wrong: a resolver returns the `sql`/driver error directly and, with no error
presenter, the client sees `pq: connection refused` or a table name.

Fix: `SetErrorPresenter`; match a typed client-safe error with `errors.As` and
mask everything else behind a generic message plus an `extensions` code.

### Assuming HTTP status reflects GraphQL success

Wrong: alerting on non-2xx. A resolver failure returns 200 with an `errors`
array, so status-based monitoring reports the endpoint healthy while every query
is failing.

Fix: correlate on the `errors` array (and extension codes), not the status code.

### No complexity or depth limiting

Wrong: trusting that clients send reasonable queries. One deeply nested or
huge-argument query can exhaust CPU or the database.

Fix: `extension.FixedComplexityLimit`, per-field complexity for argument-scaled
fields, and depth limiting conceptually — bound cost before resolvers run.

### Leaving introspection on in production

Wrong: `srv.Use(extension.Introspection{})` unconditionally, publishing the full
schema to anyone.

Fix: apply it only when an environment flag or role check allows it.

### Hand-editing generated files

Wrong: tweaking `generated.go` or `models_gen.go`. The next `gqlgen generate`
overwrites your change.

Fix: change the schema (and the resolver file); regenerate. Treat generated files
as build output.

### A scalar unmarshaler that only handles string

Wrong: `UnmarshalMoney` with a single `case string`, which fails or panics the
moment a client sends the value as a JSON number (`float64`).

Fix: switch over `string`, `float64`, `json.Number`, and `int`, and return an
error for anything else.

### Being surprised by non-null error propagation

Wrong: expecting a failing non-null field to appear as `null`. Instead the error
propagates up and nulls the nearest nullable ancestor — a much larger subtree
than the one field.

Fix: choose nullability deliberately as a failure-isolation decision, not just a
"is this required" decision.

### Sharing unsynchronized state across resolvers

Wrong: a plain map on the resolver read and written by sibling field resolvers.
gqlgen runs them concurrently, so this races.

Fix: guard shared state with a mutex (or `sync.Map`/atomics) and run tests with
`-race`.

### Not recognizing a per-item child resolver as N+1

Wrong: implementing `Product.reviews` as a per-parent fetch and shipping it,
then discovering under load that a products query issues one review fetch per
product.

Fix: recognize the shape immediately; batch the child fetches with a dataloader
(lesson 05).

Next: [01-schema-first-resolvers.md](01-schema-first-resolvers.md)
