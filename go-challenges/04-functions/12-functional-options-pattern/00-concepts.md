# Functional Options Pattern for Production Constructors — Concepts

A constructor is the one place where an object's invariants are established, its
defaults are chosen, and its collaborators are wired in. In a service that lives
for years, that constructor also has to absorb an unpredictable stream of new
knobs — a retry policy, a timeout, a metrics hook, a clock for tests — without
breaking every existing call site each time one is added. The functional options
pattern is how senior Go engineers keep that constructor readable, validating,
injectable, and source-compatible over the long haul. This file is the
conceptual spine. Read it once and each of the following modules — an HTTP
client, a `database/sql` pool, a `slog` factory, a production `http.Server`, a
backoff retrier, a TTL cache, a generic options helper, a layered config loader,
and composable presets — becomes an exercise in applying the same small idea to a
different real artifact.

## Concepts

### The problem functional options solve

Consider the naive constructor:

```go
client := New("https://api.example.com", 2*time.Second, "orders/1.2", true, nil)
```

Nobody reading that call can say what `true` or `nil` means without opening the
signature. Worse, the moment you need a seventh setting, every caller in the
codebase either breaks or has to pass a placeholder. Positional arguments couple
the call site to the exact shape of the constructor forever. The two classic
escapes are a config struct and functional options, and the choice between them
is a genuine API-design decision, not a matter of taste.

A config struct — `New(baseURL string, cfg Config)` — is clear, cheap, and
perfect when the configuration is small, mostly required, and unlikely to grow.
Its weaknesses are the mirror image of its strengths: the zero value of every
field is a silent default the caller may not have intended, there is no natural
place to validate cross-field invariants, and a caller can hold a partially built
`Config` and mutate it after the fact. Functional options trade a little
machinery for named intent at the call site, a single validation boundary, and
the ability to add a new option years later without touching one existing caller:

```go
client, err := New(
	"https://api.example.com",
	WithTimeout(2*time.Second),
	WithUserAgent("orders/1.2"),
	WithRetryStatus(http.StatusTooManyRequests, http.StatusBadGateway),
)
```

Each option names what it configures, order-independent options can appear in any
order, and the variadic `...Option` means old code that passed none still
compiles. The Uber style guide's rule of thumb is a good one: reach for options
on a public API once it has three or more optional parameters; below that a
struct is usually simpler.

### The option type is itself a design choice

The textbook option type is `type Option func(*T)`. That is correct only when
every option is *total* — it can always be applied and can never be given invalid
input. A great many production options are not total. `WithHTTPClient(nil)` is a
mistake. `WithTimeout(-1)` is a mistake. `WithRetryStatus(42)` is a mistake. If
the option type has no way to report a problem, those mistakes either panic later
or, worse, produce a silently misconfigured object. So the production form used
throughout these modules is:

```go
type Option func(*T) error
```

With an error-returning option, the constructor stays the single place where
invalid configuration is caught. There is exactly one boundary to reason about,
one place to write the tests, and one kind of return the caller must handle:
`(*T, error)`. The cost is that every option now returns an error even when it
cannot fail; that is a small, honest price for a constructor that can refuse to
build something broken.

### The constructor lifecycle contract

Every constructor in these modules follows the same four-step contract, and the
order is not negotiable:

1. Seed defaults into a fresh value, so an option that is never passed still
   yields a sane, usable field.
2. Apply the options in the order given, each mutating the value or returning an
   error that short-circuits the whole build.
3. Validate cross-field invariants that no single option could have checked on
   its own — for example that `maxIdleConns` does not exceed `maxOpenConns`, a
   relationship only visible after both options have run.
4. Return a fully constructed, fully validated value, or `(nil, err)`. Never
   return a half-configured object that a caller might use.

Step 3 is the one beginners skip. Per-option validation catches bad *inputs*; it
cannot catch bad *combinations*. The only place a combination can be checked is
after every option has run, in the constructor body, which is precisely why the
constructor — not the option — owns invariants.

### Defensive copying: do not alias caller-owned state

An option frequently receives a pointer to something the caller still owns and
still uses: an `*http.Client`, a `*slog.LevelVar`, an `*sql.DB`. If the option
stores that pointer directly and a *later* option mutates through it, the
constructor has silently changed state that belongs to the caller. The canonical
bug is `WithHTTPClient(c)` storing `c`, then `WithTimeout(d)` setting
`c.Timeout` — now the caller's own client has a timeout it never asked for.

The fix is a shallow copy of the dependency struct before any field is mutated:

```go
clone := *httpClient   // copy the struct
clone.Timeout = timeout
c.httpClient = &clone  // store the copy, leave the caller's original alone
```

A shallow copy is enough here because the fields being changed are value fields
(`Timeout`); it does not deep-copy the `Transport`, which is intentionally shared.
The rule is: copy before you mutate anything the caller can still see.

### Dependency injection is the highest-leverage use

The single most valuable real-world reason to use options is not readability — it
is testability. Time, randomness, network, and side effects are exactly the
things that make production code flaky under test, and each of them can be turned
into a deterministic, instant unit test by injecting the collaborator through an
option:

- `WithClock(func() time.Time)` makes a TTL cache's expiry a pure function of a
  clock the test advances by hand — zero real time elapses.
- `WithSleep(func(context.Context, time.Duration) error)` lets a backoff retrier
  record the delays it *would* have slept instead of sleeping them.
- `WithRand(*rand.Rand)` seeds jitter deterministically so a "random" backoff is
  reproducible.
- `WithHTTPClient(*http.Client)` points a client at an in-process
  `httptest.Server` instead of the real network.
- `WithOnEvict(func(K, V, Reason))` exposes an internal event as an observable
  hook a test can assert on.

None of this needs a global mutable clock or a package-level `rand`. The
injection is local, explicit, and defaulted: production passes nothing and gets
`time.Now`, `time.Sleep`, and a freshly seeded RNG; the test passes an option and
gets full control. This is the pattern that separates code that is merely
"options-shaped" from code that is genuinely easy to operate.

### Fail-fast versus collect-all validation

When an option returns an error, the constructor can either return on the first
failure (fail-fast) or keep going and aggregate every failure with
`errors.Join` (collect-all). Fail-fast is the right default: it is simplest, and
most callers only need to fix one thing at a time. But a configuration surface
that a human is filling in — a config file, an admin form — is far friendlier if
it reports *all* the problems at once. `errors.Join` returns an error whose
hidden `Unwrap() []error` lets a caller `errors.Is` each sentinel it cares about,
so no information is lost. The point is to choose deliberately rather than
defaulting to whichever you happened to type first.

### Option ordering is part of the public contract

Options run sequentially, so a *replacing* option (one that overwrites a field)
means last-writer-wins, and order becomes observable behavior. There are two
honest ways to handle this. Either document and test at least one override path,
making the ordering an explicit guarantee callers can rely on, or design options
to *merge* rather than replace so that order genuinely cannot matter. The failure
mode is leaving it undefined: an option that silently clobbers an earlier one
with no test and no documentation is an API whose behavior nobody can predict.

### Composability and presets

Because an option is just a function, an option can *return* another option, and a
function can bundle several primitive options into one. That gives you presets:
`WithProductionDefaults()` expands into a handful of primitives, while an explicit
`WithBatchSize(1)` applied afterward still overrides the batch size the preset
set. Presets are how a team encodes "the way we run this in prod" once and reuses
it everywhere, without losing the ability to override a single field per call
site.

### Generics remove per-type boilerplate

The option machinery is identical for every type: a `func(*T) error`, a loop that
applies them, a place to accumulate errors. With generics you write it once —
`type Option[T any] func(*T) error` and a shared `Apply[T any](*T, ...Option[T])`
— and reuse it to configure any number of unrelated structs. One options engine,
many config types, no copy-paste.

### Precedence layering

Options compose naturally with other configuration sources into a precedence
stack. A loader that seeds defaults, then reads environment variables, then
applies functional options *last* gives the ordering every operator expects:
explicit code beats environment, environment beats defaults. Because options run
after the environment is read, an explicit `WithPort(9000)` always wins over
`PORT=8080` — the code says what it means, and it says it last.

### When not to use functional options

Options are not free. Each one is a function value and a closure; the pattern adds
a layer of indirection and a page of boilerplate. For a small, fully known,
all-required configuration, a plain struct literal is clearer, cheaper, and
easier to read. Options earn their keep when optionality, validation, dependency
injection, and long-term source compatibility dominate — which is exactly the
situation a public constructor in a long-lived service is in, and exactly why the
pattern is worth mastering.

## Common Mistakes

### A non-validating option type where input can be invalid

Wrong: `type Option func(*T)` for a constructor whose options can receive a nil
client, a zero duration, or an out-of-range status code. The bad value survives
into the object and surfaces later as a panic or corrupt behavior.

Fix: use `type Option func(*T) error` so the constructor is the one boundary that
rejects invalid configuration.

### Storing and then mutating a caller-owned pointer

Wrong: `WithHTTPClient(c)` keeps `c`, and `WithTimeout(d)` sets `c.Timeout` —
mutating a client the caller still holds and uses elsewhere.

Fix: shallow-copy the dependency struct before mutating any field, and store the
copy.

### Validating only per-option and forgetting cross-field invariants

Wrong: each option checks its own input, but nothing checks that
`maxIdleConns <= maxOpenConns`, an invariant only visible once both have run.

Fix: after the option loop, validate the combinations no single option could see.

### Returning a half-configured object on failure

Wrong: an option fails midway and the constructor returns the partially built
value anyway. Callers then use an object that was never fully configured.

Fix: on any option error, return `(nil, err)` and nothing else.

### Leaving option ordering undefined

Wrong: a replacing option silently clobbers an earlier one, with no test and no
documentation, so the API's behavior under a given order is anyone's guess.

Fix: document and test at least one override path, or design options to merge.

### Reading time and randomness from globals

Wrong: the type calls `time.Now()` and package-level `rand` directly, so retries,
TTLs, and jitter cannot be made deterministic in a test.

Fix: inject them through `WithClock` / `WithSleep` / `WithRand`, defaulting to the
real ones in production.

### Over-engineering a tiny all-required config

Wrong: a full options API for a struct with two required fields and no optionality.

Fix: use a struct literal; reach for options once there are three or more optional
parameters or real validation and injection needs.

### Failing fast when the surface wants every error

Wrong: returning on the first option error when the use case is a human-facing
config that should report every problem at once.

Fix: aggregate with `errors.Join` so callers can `errors.Is` each underlying
sentinel.

### Shipping a server with dangerous zero-value timeouts

Wrong: building an `http.Server` and leaving `ReadHeaderTimeout` and
`ReadTimeout` at their zero value, which means "no timeout" and exposes the server
to Slowloris connection-holding attacks.

Fix: have the options enforce safe non-zero defaults and refuse a zero
`ReadHeaderTimeout` rather than shipping the dangerous default.

Next: [01-error-returning-options-http-client.md](01-error-returning-options-http-client.md)
