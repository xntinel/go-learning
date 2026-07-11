# Testing With Environment Variables: Hermetic Config Loading — Concepts

Environment variables are the twelve-factor backbone of backend configuration:
the DB DSN, HTTP timeouts, feature flags, and the required secrets a service
needs to boot all arrive through the process environment. They are also the
single biggest source of flaky, order-dependent, non-parallel test suites,
because an environment variable is global mutable process state. One test that
sets `DATABASE_URL` and forgets to restore it corrupts every sibling that reads
it; two tests that mutate the same variable while running in parallel race in a
way no amount of retrying will fix.

This lesson treats environment-variable testing as a production concern, not a
syntax demo. The through-line is a real config loader for a service, and the
senior insight is uncomfortable: reaching for global process state in a test is
a design smell. `t.Setenv` is a correct crutch — it restores the original value
through a `Cleanup` and refuses to run under `t.Parallel` — but the durable fix
is to invert the dependency, passing the environment reader in as a
`func(string) (string, bool)` so that config parsing becomes a pure,
table-driven, fully parallel unit under test. Read this file once and you have
the model behind each of the ten independent exercises that follow.

## Concepts

### `t.Setenv` is the managed way to mutate the environment in a test

`t.Setenv(key, value)` calls `os.Setenv` under the hood and registers a
`t.Cleanup` that restores the original value when the test (and its subtests)
finish — including restoring the variable to *unset* if it was unset before.
It exists precisely so you never hand-roll `defer os.Setenv(key, original)`,
which is wrong in two ways: it cannot express "restore to unset" (it sets the
variable back to empty string instead), and a `defer` is skipped on some panic
and cleanup paths in ways `t.Cleanup` is not. The rule is simple: in a test,
never call `os.Setenv` directly; call `t.Setenv`.

### `t.Setenv` and `t.Parallel` are mutually exclusive by construction

Because the environment is process-global, mutating it from one goroutine is
visible to every other goroutine, so `t.Setenv` panics if the calling test or
any of its ancestors has called `t.Parallel`. This is not a limitation to work
around; it is the runtime telling you that env mutation and parallelism are
fundamentally incompatible. The same reasoning applies to `t.Chdir` (Go 1.24
added it with matching semantics around the process-global working directory):
both refuse to coexist with `t.Parallel` because both mutate state shared by the
whole process. A test that both sets an env var and wants to run in parallel is
a contradiction; you resolve it by removing the global mutation, not by silencing
the panic.

### `os.Getenv` erases the difference between unset and empty

`os.Getenv(key)` returns `""` both when the variable is unset and when it is set
to the empty string. For most optional flags that collapse is harmless, but for
a required secret it is an incident waiting to happen: an operator who
accidentally blanks out `DATABASE_URL=` in a deployment has a *different*
misconfiguration from one who never set it, and a loader that treats both as
"fall back to the default" silently connects to the wrong database.
`os.LookupEnv(key)` returns `(value, ok)`, and it is the only correct tool when
empty is a distinct, meaningful state. Branch on `ok` first, then on whether the
value is empty.

### Dependency inversion is the durable fix for un-parallelizable env tests

The reason env tests cannot be parallel is that they read global state. Remove
the global read and the problem evaporates. Make config parsing a pure function
of an injected reader:

```text
func LoadFrom(getenv func(string) (string, bool)) (Config, error)
func Load() (Config, error) { return LoadFrom(os.LookupEnv) }
```

Production passes `os.LookupEnv`; a test passes a closure over a
`map[string]string`. The parser now touches no process state, so every subtest
can call `t.Parallel()` and the whole table runs concurrently under `-race`.
`t.Setenv` papers over the symptom (it makes one serial test correct); injection
removes the cause. This is the single most valuable idea in the lesson.

### Typed parsing must name the offending key and value

`time.ParseDuration`, `strconv.ParseBool`, and `strconv.Atoi` turn strings into
the types your config struct actually holds. When they fail, wrap the error with
the variable name *and* the bad value: an operator debugging a failed deploy at
3 a.m. needs to read "HTTP_TIMEOUT=\"30\": invalid duration" and know both which
variable and what value to fix, not a bare "invalid config". A classic trap:
`time.ParseDuration` requires a unit, so `HTTP_TIMEOUT=30` fails while
`HTTP_TIMEOUT=30s` succeeds; the wrapped error is what makes that fixable at a
glance. `strconv.ParseBool` accepts `1, t, T, TRUE, true, True` as true and the
matching falsy set, and rejects everything else — so `FEATURE_X=yes` is an error,
not a truthy value.

### Fail-fast startup should report every error, not just the first

A loader that returns on the first bad variable forces a fix-restart-repeat loop:
the operator fixes one variable, reboots, hits the next error, and so on.
Accumulate all validation failures into a slice and return `errors.Join(errs...)`
so one boot log surfaces every misconfiguration at once. The joined error still
satisfies `errors.Is` for each underlying sentinel, so callers can branch on any
specific failure, and its `Error()` text lists them all (one per line).

### `os.Expand` with a custom mapping composes templates safely

Building a DSN from parts is a twelve-factor staple:
`postgres://${DB_USER}:${DB_PASS}@${DB_HOST}:${DB_PORT}/${DB_NAME}`.
`os.ExpandEnv` does this but substitutes `""` for any undefined variable, so a
single missing `DB_HOST` yields a syntactically valid, semantically broken
connection string that fails later with a confusing error. `os.Expand(s, mapping)`
lets you supply your own `mapping func(string) string`; from inside it you call
`os.LookupEnv`, record any key that is absent, and return an error listing every
undefined variable — turning a silent late failure into a loud startup failure.

### Twelve-factor precedence is defaults < file < environment

When the same key can come from a compiled default, a config file, and the
environment, the twelve-factor rule is that the environment wins: it is the
highest-priority override, because it is what an operator changes per deployment
without recompiling or rewriting a file. Encode this ordering as an executable
test — a small `Resolve(defaults, fileValues, getenv)` whose per-field behavior
is asserted — rather than leaving it as tribal knowledge that drifts.

### Config read at package init is frozen before any test runs

A package-level `var Timeout = parse(os.Getenv("HTTP_TIMEOUT"))` or an `init()`
that reads the environment executes once, when the package is loaded, which is
*before* any test function or `t.Setenv` runs. A test that then calls
`t.Setenv("HTTP_TIMEOUT", ...)` and asserts on that variable observes the stale,
init-time value and either fails mysteriously or — worse — passes against the
wrong data. The fix is to read lazily: put the read inside a function that runs
on each call, or behind `sync.OnceValue` when you want a cache. Note that
`sync.OnceValue` caches the *first* call's result, so a value-caching loader must
be constructed per test (a fresh `sync.OnceValue`) to stay observable.

### Config that carries secrets must be safe to log

Dumping the config at startup with `%+v` or `slog` is a common and useful
diagnostic — and a common credential-leak path straight into log aggregation. A
config struct that carries a DB password or API token should implement
`slog.LogValuer`: its `LogValue()` returns a `slog.GroupValue` where the secret
fields are replaced with `"REDACTED"` while host, port, and timeout stay visible.
`slog` resolves the `LogValuer` when the struct is logged, so the real secret
never reaches the handler. If the type also has a `String()` method, redact there
too, so `fmt` verbs are equally safe.

### Cleanup ordering and hermeticity are contracts you assert

`t.Cleanup` functions run last-added-first-called (LIFO), just before the test's
`t.Context()` is canceled; when one cleanup restores state that another asserts
on, the order matters. Hermeticity — the property that a test leaves the
environment exactly as it found it — is something you *assert*, not assume:
snapshot the original with `os.LookupEnv`, mutate, and verify restoration inside
a `t.Cleanup`. Do that and a leak becomes a deterministic failure in the test
that caused it, instead of a flake in some unrelated sibling test three files
away.

## Common Mistakes

### Manual `defer os.Setenv` restoration

Wrong: `os.Setenv("KEY", "v"); defer os.Setenv("KEY", original)`. This forgets
the unset case — `defer` sets the variable back to empty string rather than
unsetting it — and it can be skipped on some `t.Fatal`/panic paths, leaking the
value into later tests.

Fix: use `t.Setenv`, which restores to the exact prior state (including unset)
through `t.Cleanup`.

### Calling `t.Setenv` in a parallel test

Wrong: a test that calls both `t.Parallel()` and `t.Setenv(...)`. It panics at
runtime because env mutation is process-global.

Fix: keep the test serial, or — better — inject a `getenv func(string)(string,bool)`
so parsing is pure and the whole table can run parallel with no env mutation.

### Using `os.Getenv` where empty and unset differ

Wrong: `if os.Getenv("DATABASE_URL") == "" { useDefault() }` — a blanked-out
required secret silently falls through to a default.

Fix: use `os.LookupEnv` and branch on `ok`, so "set but empty" is a distinct,
explicit error from "unset".

### Parsing a duration as a bare number

Wrong: setting `HTTP_TIMEOUT=30` and calling `time.ParseDuration` — it fails,
because `ParseDuration` requires a unit.

Fix: require a unit (`30s`, `1500ms`) and wrap the parse error with the key and
value so the fix is obvious.

### Returning only the first config error

Wrong: bailing on the first bad variable, so the operator fixes one, restarts,
and hits the next.

Fix: accumulate all failures and return `errors.Join(errs...)`; it stays
matchable per sentinel with `errors.Is`.

### Building a DSN with `os.ExpandEnv` and a missing variable

Wrong: `os.ExpandEnv(template)` — an unset `${DB_HOST}` becomes `""`, producing
a valid-looking string that fails to connect with a confusing message.

Fix: use `os.Expand` with a mapping that records undefined names and returns an
error listing them.

### Reading env at package init and overriding with `t.Setenv`

Wrong: `var Timeout = parse(os.Getenv("HTTP_TIMEOUT"))` captured at init, then a
test tries to change it with `t.Setenv` — the value was frozen before the test
ran and never changes.

Fix: move the read into a function or `sync.OnceValue` so it is observable;
construct a fresh `OnceValue` per test if you need the cache.

### Logging the whole config without redaction

Wrong: `logger.Info("startup", "config", cfg)` or `%+v` on a struct that holds a
password — the secret lands in log aggregation.

Fix: implement `slog.LogValuer` (and `String()`) to return `"REDACTED"` for
secret fields.

### Asserting restoration with a comment instead of a cleanup

Wrong: a `// t.Setenv restores this` comment and no check — the leak surfaces as
a flake in an unrelated test later.

Fix: snapshot with `os.LookupEnv`, and assert restoration inside a `t.Cleanup`,
so hermeticity is an executable contract.

Next: [01-env-config-loader.md](01-env-config-loader.md)
