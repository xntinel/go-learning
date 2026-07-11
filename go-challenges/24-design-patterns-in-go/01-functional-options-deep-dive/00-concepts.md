# Functional Options — Concepts

Go has no optional parameters, no default argument values, and no constructor overloading. A `Server` that exposes only a port and a timeout today will want TLS, a logger, retries, and middleware tomorrow, and a positional constructor that tries to absorb that growth breaks every existing call site the moment a new parameter is added. The functional options pattern is the idiomatic answer: each setting becomes a small, self-documenting function that mutates a freshly-defaulted value, and the constructor takes a variadic list of those functions. The pattern is so pervasive in the ecosystem — `grpc.Dial`, `zap.New`, `redis.NewClient`, and countless internal libraries — that "configurable Go constructor" almost always means functional options. This file is the conceptual foundation; read it once and you will have everything needed to build the three exercises, which construct a validating server, an aggregating-and-required-field client, and a generic option toolkit as independent, self-contained Go modules.

## Concepts

### The Option Type Is a Function That Mutates a Value

The entire pattern rests on one type declaration:

```go
type Option func(*Server) error
```

An option is a function that receives a pointer to the partially-built value and either mutates a field or returns a validation error. A "with" constructor returns one of these closures, capturing the caller's argument:

```go
func WithPort(port int) Option {
	return func(s *Server) error { /* validate, then s.port = port */ }
}
```

The crucial design choice is the `error` return. A naive version, `type Option func(*Server)`, cannot signal that a value is out of range; a bad port either panics or silently misconfigures the server and the caller never finds out. Returning `error` turns every option into a validator and gives the constructor a single channel through which any failure — from a directly-passed option or from a preset that bundles several — propagates uniformly.

### Defaults First, Options Override in Order

The constructor sets every field to a default once, then iterates the options:

```go
func New(opts ...Option) (*Server, error) {
	s := &Server{port: 8080 /* , other defaults */}
	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, fmt.Errorf("server: %w", err)
		}
	}
	return s, nil
}
```

Two invariants fall directly out of this two-line shape, and both are worth pinning with tests. First, later options win: `New(WithPort(3000), WithPort(4000))` ends on port 4000, because the second closure runs after the first and overwrites the field. Second, a preset plus an explicit override yields the override: `New(WithProductionDefaults(), WithPort(443))` keeps the production timeouts but listens on 443. If defaults were applied *after* the options instead of before, both properties would invert and the default would silently clobber the caller's choice — the single most common structural bug in a hand-rolled options constructor.

### Sentinel Errors and `errors.Is`

Each validating option returns a package-level sentinel, wrapped with `%w` when it wants to include the offending value:

```go
var ErrInvalidPort = errors.New("port must be between 1 and 65535")
// ...
return fmt.Errorf("%w: got %d", ErrInvalidPort, port)
```

The `%w` verb keeps the sentinel reachable through `errors.Is(err, ErrInvalidPort)` while still surfacing the bad input in the message. Tests assert on the sentinel, not on a substring of the message, so rewording the text never breaks a test and callers can branch on the specific failure. This is the contract that lets the pattern scale: as the number of options grows, the error vocabulary stays stable and machine-checkable.

### Presets Are Just Options That Apply a Bundle

A preset — `WithProductionDefaults()`, `WithDevelopmentDefaults()` — is nothing more than an option whose body runs other options:

```go
func WithProductionDefaults() Option {
	return func(s *Server) error {
		return apply(s, WithReadTimeout(30*time.Second), WithMaxConns(1000))
	}
}
```

The constructor cannot tell a preset apart from a single option, and that is exactly the point: a preset composes through the same `Option` type and the same error channel, so adding or removing a setting inside a preset is a one-line change and a failure inside it propagates like any other. Extracting the shared apply-a-bundle loop into a small helper keeps every preset to a single expression.

### Encapsulation: Unexported Fields, Narrow Accessors

A validated value should not be mutable after construction. If the struct exposes `Port int`, any downstream holder can write `s.Port = 99999` and bypass the validator that ran in `New`. The pattern keeps every field unexported and offers narrow read-only accessors (`Port()`, `Addr()`, `ReadTimeout()`) as the public surface. A separate `cmd/demo` package — which can only see exported identifiers — proves the discipline: it reads everything it needs through accessors and never touches a field. The moment you feel tempted to export a field so the demo can read it, add an accessor instead.

### Two Validation Strategies: Short-Circuit vs Aggregate

The basic constructor stops at the first failing option and returns immediately. That is the right default for a CLI or a server boot path where the first error is enough to abort. But some constructors should report *every* problem at once — a configuration loaded from a file, where surfacing one error per round-trip is a poor experience. The aggregating variant runs all the options, collects their errors into a slice, and joins them:

```go
var errs []error
for _, opt := range opts {
	if err := opt(c); err != nil {
		errs = append(errs, err)
	}
}
// ... append required-field and cross-field errors ...
if len(errs) > 0 {
	return nil, errors.Join(errs...)
}
```

`errors.Join` (Go 1.20+) returns a single error whose `Error()` prints each cause on its own line, and `errors.Is` still finds every joined sentinel inside it. The choice between the two strategies is a genuine design decision, not an accident: short-circuit favors fast failure and a single clear cause; aggregate favors completeness at the cost of running options whose inputs may already be known-bad.

### Required vs Optional, and Cross-Field Invariants

Functional options model *optional* settings well, but real constructors also have *required* inputs (a database client needs a DSN) and *cross-field* invariants (`maxIdleConns` must not exceed `maxOpenConns`). Neither belongs inside any single option: a required-field check cannot live in an option that may simply never be passed, and a cross-field rule depends on two fields whose options can arrive in any order. Both belong in a final validation pass after the option loop. The clean way to enforce a required field is to give it no default and check it at the end — if `dsn == ""` after all options ran, the value was never supplied — so a missing required field and an explicitly-empty one collapse into the same `ErrMissingDSN`. Cross-field rules read the assembled value once, after every option has had its say.

### Generic Options: One `Option[T]` for Every Type

Every config type in a codebase otherwise redeclares the same boilerplate: its own `type Option func(*X) error` and its own constructor loop. Generics collapse that to a single reusable pair:

```go
type Option[T any] func(*T) error

func New[T any](defaults T, opts ...Option[T]) (T, error) { /* loop */ }
```

Now `HTTPConfig`, `CacheConfig`, and any future type share one option type and one constructor; only the per-type `WithX` builders differ, and `T` is inferred from the `defaults` argument. The trade-off is real: the generic form usually starts from a passed-in defaults value rather than hiding defaults inside the constructor, and it tends to pair with exported config fields, so it gives up some of the encapsulation the unexported-field form provides. Use it when you have several config types that would otherwise duplicate the same machinery; use the concrete form when one type wants the tightest possible encapsulation.

### When to Choose Functional Options at All

Functional options are not always the answer. The rule of thumb: when most callers want the defaults and a few override one or two fields, options read best and stay backward-compatible as settings are added. When every caller sets every field, a plain config struct passed by value is simpler and needs no closures. When the configuration has many interlocking cross-field invariants and you want a fluent, stepwise build, a builder with a terminal `Build() (T, error)` may fit better. Functional options sit between the two: they keep the zero-configuration ergonomics of a struct while adding per-option validation and composability, which is why they dominate library constructors but rarely show up for simple internal value types.

## Common Mistakes

### Defining `Option` Without an Error Return

Wrong: `type Option func(*Server)`. A bad value cannot be signaled, so the option either panics or silently misconfigures the value and the caller has no way to know. Fix: `type Option func(*Server) error` and let the constructor's loop propagate failures. The error channel is what later lets presets and aggregation work.

### Applying Defaults After the Options

Wrong: building an empty struct, ranging the options, then assigning defaults at the end. `New(WithPort(443))` ends up on the default port because the default ran last and overwrote the caller's choice. Fix: set defaults first, then iterate. The order of those two steps is the entire mechanism.

### Returning Ad-Hoc Strings Instead of Sentinels

Wrong: `return fmt.Errorf("port must be between 1 and 65535")`. Tests must then match on substrings, and any rewording breaks them for no reason; callers cannot branch on the failure. Fix: declare a package-level `var ErrInvalidPort = errors.New(...)` and wrap it with `%w` to add the bad value. Tests and callers use `errors.Is`.

### Putting Required-Field or Cross-Field Checks Inside an Option

Wrong: trying to enforce "dsn is required" inside `WithDSN`, or "idle <= open" inside `WithMaxIdleConns`. A required check cannot fire from an option that is never passed, and a cross-field check inside one option sees the other field in whatever state the option ordering left it. Fix: enforce both in a final pass after the option loop, reading the fully-assembled value once.

### Exporting Fields So `cmd/demo` Can Read Them

Wrong: `type Server struct { Port int }` so the demo can print `s.Port`. Now any holder can mutate the value after construction and bypass the validator that ran in `New`. Fix: keep fields unexported and add narrow accessors; the demo reads through them. (The generic-options exercise deliberately uses exported config fields and documents that trade-off — it is a different point on the same spectrum, not a contradiction.)

### Short-Circuiting When the Caller Needed Every Error

Wrong: using the first-error-wins loop for a config-file constructor, forcing the user to fix one problem, re-run, and discover the next. Fix: when completeness matters, collect errors into a slice and return `errors.Join(errs...)`; `errors.Is` still finds each sentinel, and the user sees every problem at once.

---

Next: [01-server-options.md](01-server-options.md)
