# Structured Test Metadata with T.Attr and T.Output — Concepts

At CI scale, freeform test logs are unqueryable noise. You cannot answer "which
owner's tests are slowest", "which flaky tests blew their SLO budget", or "what
scores did the LLM-judge tests record" by grepping `t.Log` lines: the format is
whatever each engineer typed, the file/line prefix is the logging call site (not
the fact you care about), and under `-parallel` the lines from different tests
interleave. Go 1.25 adds two primitives that turn a test binary into a structured
data source. `T.Attr` attaches stable key/value facts to a test that a machine can
read back through `go test -json`. `T.Output` hands a subsystem — an slog logger, a
subprocess, anything that writes bytes — a correctly-attributed, per-test,
line-buffered stream instead of racing on `os.Stdout`. This lesson builds both
sides of the pipeline you would own in production: the producer that emits validated
attributes and streams diagnostics, and the consumer that parses the JSON event
stream into a report a gate policy can act on.

## Concepts

### What T.Attr is, exactly

The new methods `T.Attr`, `B.Attr`, and `F.Attr` (all added in Go 1.25) emit an
attribute — an arbitrary key and value associated with the current test — to the
test log. The verified signature is:

```
func (c *T) Attr(key, value string)
```

`*B` and `*F` carry the same method, and it is part of the `testing.TB` interface,
so any code that already accepts a `testing.TB` can emit attributes. Both arguments
are `string`. That is the first design consequence: a numeric metric — an elapsed
budget, an eval score, a byte count — must be serialized with `strconv.Itoa`,
`strconv.FormatFloat`, or `fmt.Sprintf` before it can be emitted. There is no
`Attr(key string, value any)` overload; passing a non-string simply does not
compile.

### The hard constraints, and why they gate

The documentation states two constraints without ceremony, and both are load-bearing:
the key must not contain whitespace, and the value must not contain newlines or
carriage returns. These are not stylistic preferences. In verbose text mode,
`t.Attr("owner", "payments")` in a test named `TestF` renders as a single line:

```
=== ATTR  TestF owner payments
```

The consumer (`go tool test2json`) reconstructs the key and value by splitting that
line on spaces: it cuts the test name off the front, then cuts the remainder once
more into key and value. A key containing a space would be split in the wrong place;
a value containing a newline would spill onto a second line that the parser reads as
an unrelated event. The metadata would not merely look ugly — it would corrupt the
machine-readable stream. This is why untrusted metadata (a branch name, a user
string, a model's free-text output) must be validated or serialized before it ever
reaches `Attr`, not after a red CI teaches you the same lesson. Exercise 1 builds
exactly that validating gate.

The third property is subtler: attributes are emitted immediately, but they are
"intended to be treated as unordered". Never write code or an assertion that depends
on the order two attributes were emitted. If you need a stable, reproducible order —
and you do, for golden-output tests — impose it yourself by sorting keys, which is
why the exercise-1 emitter sorts before calling the sink.

### The machine-readable contract

Text mode is for humans. The reason `Attr` is worth more than a formatted `t.Log`
line is the JSON mode. With `-json`, an attribute surfaces as a new event whose
`Action` is `"attr"`, carrying `Key` and `Value` fields. The `test2json` event
struct (in `cmd/internal/test2json`) grew those two fields for precisely this:

```
Action  string
Package string
Test    string
Key     string   // set when Action == "attr"
Value   string   // set when Action == "attr"
Elapsed *float64 // set on "pass" and "fail"
```

`Elapsed` is a pointer so that "no elapsed time recorded" is distinguishable from
"zero seconds" — a distinction a consumer must respect. This struct is the stable
contract that separates `Attr` from freeform logging: a dashboard can group by
`Key=="owner"`, sum `Elapsed` per owner, and enforce "every test must declare an
owner" without parsing anyone's prose. Exercise 3 is that consumer.

### T.Output and why it is not t.Log

`T.Output` (Go 1.25) returns an `io.Writer`:

```
func (c *T) Output() io.Writer
```

Its documented semantics are specific. It writes to the same stream as `TB.Log`, and
the output is indented like `Log` lines — so it is attributed to the owning test and
stays grouped under it even with `-parallel`. But unlike `Log`, it does not add a
source location and does not add newlines. It is internally line-buffered; a call to
`TB.Log` or the end of the test implicitly flushes the buffer, followed by a newline.

Those differences are the whole point. `t.Log` prepends the file and line of the
call site — which is wrong when the caller is a logging library: you would see the
library's internal line, not your code. `t.Log` also forces its own newline framing,
mangling output whose formatting you want preserved (an slog record, a subprocess's
stdout). `os.Stdout` interleaves unattributed bytes across parallel tests. `Output`
gives you a writer that preserves the subsystem's own bytes, indents them under the
right test, and does not lie about where they came from. That makes it the correct
sink to inject into an slog handler (Exercise 2) or to hand a subprocess as its
stdout.

### The lifetime hazard

`Output` carries a rule that punishes a common concurrency bug: after a test function
and all its parents return, neither `Output` nor the returned writer's `Write` method
may be called. A goroutine that outlives the test and writes to that writer is not a
harmless late log line — it violates the contract and will panic (or lose output).
This ties directly to correct concurrent-test hygiene: any goroutine you start that
writes through `Output` must be joined — with a `sync.WaitGroup`, an `errgroup`, or a
channel — before the test function returns. The same trap catches a `t.Cleanup` that
writes through `Output` after parents have already returned. If you internalized the
synctest lesson's "join every goroutine before the bubble ends", this is the same
discipline for a different reason.

### The full pipeline

The producer and consumer meet at `test2json`. Running `go test -json` (equivalently
`go test -v | go tool test2json`) yields a newline-delimited stream of JSON event
objects. Tools like `gotestsum` and `gotestfmt`, and any custom CI gate, consume that
stream. Understanding the `Action` values is what lets you build the consumer side:
`start`, `run`, `pause`, `cont`, `pass`, `fail`, `skip`, `output`, and now `attr`.
The event also carries `Package`, `Test`, and `Elapsed`. A robust consumer keys each
result by its fully-qualified name (`Package` + `Test`), folds `attr` events into a
per-test attribute map, records the terminal `pass`/`fail`/`skip` action as the
status, and ignores actions it does not model rather than crashing on them.

### Trade-offs and maturity

These primitives are deliberately narrow. Attributes are per-test, not per-assertion;
they are strings only; they are unordered; and `Attr` never fails a test — a missing
or wrong attribute is a fact, not an assertion, and is only caught by a downstream
consumer. If you want "every test must declare an owner" enforced, you must build the
gate that enforces it (Exercise 3); `Attr` alone will happily emit nothing and stay
green. Tool support is still maturing: not every CI dashboard understands the `attr`
action yet, and both `Attr` and `Output` require Go 1.25 or newer. Design consumers
to ignore attributes they do not recognize, and design producers to degrade
gracefully where the sink is older tooling.

### An adjacent 1.25 hardening

In the same release, `testing.AllocsPerRun` now panics if parallel tests are running,
because its result is inherently flaky under concurrency and the old best-effort
behavior hid real bugs. It is the same philosophy behind `Attr` and `Output`: make
test output structured and deterministic rather than best-effort and racy.

## Common Mistakes

### Passing a non-string value to Attr

Wrong: reaching for `t.Attr("slo_ms", 250)` and expecting a numeric attribute. `Attr`
takes `value string`; this does not compile. Fix: serialize first —
`t.Attr("slo_ms", strconv.Itoa(250))` — and serialize floats with
`strconv.FormatFloat` so the downstream parser sees a stable representation.

### Whitespace in keys or newlines in values

Wrong: `t.Attr("case id", "line1\nline2")`. The space in the key and the newline in
the value break the space-split and line-split that `test2json` performs on the
`=== ATTR` line, corrupting the stream. Fix: validate before emitting — reject any
key containing whitespace and any value containing `\n` or `\r`, and return an error
instead of silently corrupting the log.

### Depending on attribute order

Wrong: asserting that `owner` appears before `case_id` because you emitted it first.
Attributes are explicitly unordered on the wire. Fix: if you need a stable order for
a golden test, sort the keys yourself before emitting, and have the consumer key by
name rather than position.

### Writing to Output after the test returns

Wrong: starting a goroutine that logs through `t.Output()` and letting it run past
the end of the test, or writing through `Output` from a `t.Cleanup` that fires after
parents returned. `Output` may not be called after the test and all its parents
return; this panics. Fix: join every such goroutine before the test function returns.

### Confusing Output with t.Log

Wrong: assuming `Output` adds a `file:line` prefix or a trailing newline per `Write`.
It does neither — it is line-buffered and flushes on the next `Log` or at test end.
Expecting per-write newlines leads to surprising concatenated lines. Fix: let the
subsystem format its own bytes; if you need an immediate flush, a `Log` call (or the
test ending) provides it.

### Using fmt.Println or os.Stdout for test diagnostics

Wrong: `fmt.Println` inside a parallel test. Its output is unattributed and
interleaves with other tests. Fix: route diagnostics through `t.Output()` so they are
indented, per-test, and non-interleaved.

### Treating Attr as an assertion

Wrong: emitting `t.Attr("owner", owner)` and assuming a missing owner will fail the
test. `Attr` never fails anything. Fix: build the consumer-side gate (Exercise 3) that
reads the JSON stream and reports tests missing the required attribute; that is where
enforcement lives.

Next: [01-attr-metadata-emitter.md](01-attr-metadata-emitter.md)
