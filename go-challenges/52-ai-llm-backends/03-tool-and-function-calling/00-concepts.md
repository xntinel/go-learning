# Tool and Function Calling — Concepts

Tool calling is the feature that turns a language model from a text generator into
an actor inside your system, and it is the point where every backend discipline
you already have — API design, input validation, authorization, idempotency,
observability — comes back doubled. The mental model that keeps you safe is
narrow and boring: tool calling is a typed RPC dispatcher whose client is
untrusted and non-deterministic. The model never runs anything. It emits a
structured intent — a tool name plus JSON arguments — and *your* process decides
whether to run the code, runs it, and hands the result back. Control and side
effects live entirely in your service. Get that framing right and the SDK is a
thin envelope; get it wrong and you have wired a hallucinating, injection-steerable
client straight into your production capabilities. This file is the conceptual
foundation for the three independent exercises that follow: a provider-neutral
typed tool registry, the agentic loop against the Anthropic Go SDK, and the same
loop against OpenAI Chat Completions.

## Concepts

### Tool calling is a protocol, not execution

The single most important sentence in this chapter: the model does not execute
your tool. When you send a request with tool definitions and the model decides a
tool is needed, the response is not a side effect — it is a message that says "I
would like to call `get_weather` with these arguments." The model has produced
text (structured as JSON) and stopped. Your code reads that intent, validates it,
runs the actual function, and feeds the result back on the next request. Every
byte of control stays on your side. This is why "the model deleted my database"
is a category error: the model can only *ask*; whether the deletion happens is a
decision your handler makes. Treat the tool-call response as an untrusted request
body that happens to have been authored by an LLM, because that is exactly what
it is.

### The loop is a state machine you own

Tool calling is inherently multi-turn, and the turns form a small state machine
that you — not the SDK — drive:

1. Send the conversation plus the tool definitions.
2. Inspect the stop reason. Anthropic sets `stop_reason` to `tool_use`; OpenAI
   sets `finish_reason` to `tool_calls`. Any other terminal reason (`end_turn`,
   `stop`) means the model produced a final answer and the loop ends.
3. For each requested tool call, execute the tool and produce a result.
4. Append the assistant turn that requested the tools, then append the results,
   and go back to step 1.

Because step 2 can keep returning "tool_use" forever, the loop must be bounded on
three axes at once: a maximum number of turns, a token/cost budget, and a
`context` deadline. A model that loops — because a tool keeps failing, or because
it is stuck re-planning — is not a hang, it is unbounded spend. The bound is not
an optimization; it is the difference between a request and a runaway bill.

### The conversation-shape invariant

The wire protocol enforces a strict pairing, and violating it is a hard 400, not
a soft degradation. Every tool request must be answered, and the request must be
recorded before its answer:

- Anthropic: every `tool_use` block must be followed, in a later user turn, by a
  `tool_result` block carrying the same `tool_use_id`. The assistant turn holding
  the `tool_use` blocks must be appended to the message list before the user turn
  holding the `tool_result` blocks.
- OpenAI: every entry in `message.tool_calls` must be answered by a `role=tool`
  message whose `tool_call_id` matches, and the assistant message (via
  `ToParam()`) that carried the tool calls must be appended before those tool
  messages.

Miss a single result, mismatch one id, or append the results without first
appending the assistant turn, and the next request is rejected. The loops in the
exercises are structured specifically to make this invariant impossible to get
wrong: append the assistant turn immediately, then build exactly one result per
request.

### Parallel tool calls

A single assistant turn can contain more than one tool request — the model may
ask for the weather in three cities at once. You must return exactly one result
per request id, and you may run the handlers concurrently, but you must aggregate
the results and preserve the id-to-result mapping. The exercises execute
sequentially for clarity, but the shape (collect all requests in the turn, emit
one result each) is what lets you parallelize later without breaking the pairing
invariant.

### JSON Schema is the contract and a prompt, not a validator

Each tool carries a JSON Schema describing its arguments. That schema does two
jobs and neither of them is "guarantee the arguments are valid." First, it is the
wire contract both SDKs serialize into the tool definition. Second — and this is
the part backend engineers underweight — it is *prompt material*: the field names,
descriptions, enums, and required list are the model's only documentation of your
tool, and their quality directly drives how reliably the model selects the tool
and fills the arguments. A field described as "the ISO-8601 start date, inclusive"
is filled correctly; a field named `start` with no description is guessed. But the
schema is advisory to the model, not enforced by it: the model can and does emit
malformed JSON, omit required fields, invent fields you never declared, and pass
the wrong types. So the schema improves selection; it never removes the need to
decode and validate server-side. Always parse into a typed struct and check the
semantics yourself.

### The threat model: arguments are attacker-influenceable input

This is the concept that separates a demo from a backend. The client driving your
tools is a model, and the model is steerable by any untrusted content it reads —
a web page, a document, an email, a prior tool result. That is prompt injection,
and it makes tool arguments adversarial input in the precise sense a request body
from the open internet is adversarial. Treat every invocation like an
unauthenticated request: allowlist which tools exist, authorize each call inside
the handler against the caller's real identity (the model asking is never
authorization), validate and bound every argument, never interpolate a
model-supplied string into SQL, a shell command, or a filesystem path, and run
effectful tools with least privilege. High-impact or destructive tools —
anything that spends money, deletes data, or sends a message — get a
human-in-the-loop confirmation, because the confused-deputy risk is real: a
prompt-injected model will happily call `delete_account` with attacker-chosen
arguments if you let it.

The canonical anti-pattern lives in an official SDK example, which reaches into
the decoded arguments with `args["location"].(string)`. On a well-behaved
response that works; on a hostile or malformed one — the key missing, or the
value a number — the type assertion panics and takes down the request goroutine.
A backend never does this. You decode into a typed struct and handle the error;
you do not assert on a map you did not validate.

### Error handling: recoverable failures are results, not crashes

There are two kinds of failure inside the loop and they are handled oppositely.
A recoverable failure — bad arguments, a record not found, a rate temporarily
unavailable — is returned to the model as a tool result with an `is_error` flag
set. The model reads the error as content and self-corrects: it retries with a
valid argument, or apologizes, or picks another path. This is the whole reason
tool errors are first-class in both protocols. An unrecoverable failure — an
authentication error against your own backend, infrastructure down, a context
cancellation — aborts the loop and propagates as a Go error, because the model
cannot fix it and retrying only burns budget. The rule that follows: never panic
inside the loop. A failed `json.Unmarshal` becomes an `is_error` result, not a
stack trace.

### Idempotency and side effects

An agent does not call your tool exactly once. It retries after a timeout,
repeats a step while re-planning, and sometimes issues the same call twice in one
turn because two branches of its reasoning both needed it. A mutating tool must
therefore be safe under repetition: accept an idempotency key so a retried
"create" returns the original result instead of a duplicate, or design the
operation to converge (set a value rather than increment it). Give each blocking
handler its own timeout, too, so one slow tool cannot stall the entire loop while
the context deadline ticks.

### tool_choice and its trade-offs

Both providers let you steer whether the model uses tools. The modes are: `auto`
(the model decides), `any`/`required` (the model must call some tool),
a forced specific tool (the model must call exactly that one), and `none` (no
tools this turn). Forcing is useful for structured-extraction flows where you
know a tool must run, but it has a cost: a forced model cannot answer directly,
and if it has nothing sensible to pass it will fabricate arguments to satisfy the
call. Default to `auto` and reach for forcing only when the control flow genuinely
requires it.

### Provider differences are wire-shape, not concept

The two SDKs model the same protocol with different envelopes, and seeing the
difference is what motivates an internal abstraction. Anthropic uses typed
content blocks: the response `Content` is a list you iterate, switching on
`tool_use` blocks; results go back as `tool_result` blocks inside a user message;
tool choice is a union of `auto`/`any`/`tool`/`none`. OpenAI uses a
`message.tool_calls` array whose `Arguments` is a raw JSON *string*; results go
back as `role=tool` messages keyed by `tool_call_id`; `finish_reason` drives the
loop. Same state machine, different structs. If both providers share one internal
tool registry and dispatcher — which the exercises build — then swapping or
load-balancing providers is a thin adapter, not a rewrite.

### Observability at the integration boundary

Tools are where the non-deterministic model touches your deterministic systems,
which makes them the highest-value thing to instrument. Log, per call, the tool
name, the (validated, redacted) arguments, the result or error, the latency, and
a correlation id that ties the whole loop together. Emit structured traces so a
single agent run is one trace with a span per tool call. Without this, "the agent
did something weird in production" is unfalsifiable; with it, it is a trace you
can read.

### Schema authoring: hand-written versus reflected

You can write the JSON Schema by hand as a `map[string]any`, which gives total
control over descriptions and constraints at the cost of duplicating the shape of
your Go struct. Or you can generate it from the struct with a reflection-based
library, which stays DRY but cedes fine control and adds a dependency. Neither is
wrong; the trade-off is control versus duplication. The registry exercise writes
schemas by hand so the contract is explicit and visible, which is the right
default when the schema doubles as a prompt.

## Common Mistakes

### Appending the tool result without the assistant turn

Wrong: executing the tools and appending only the `tool_result` / `role=tool`
messages, skipping the assistant turn that requested them. The API rejects a tool
result with no preceding tool request — an immediate 400.

Fix: append the assistant turn (`message.ToParam()`) first, then the results, on
every iteration. The loops here do this as the first two statements after a
response.

### Answering only some parallel calls, or mismatching ids

Wrong: a turn asks for three tools and you return two results, or you swap the
ids. One missing or wrong id fails the whole request.

Fix: iterate every tool request in the turn and emit exactly one result per id,
carrying the original `tool_use_id` / `tool_call_id` verbatim.

### Asserting on unvalidated arguments

Wrong: `args["location"].(string)` on a decoded map, exactly as an SDK example
shows. A hostile or malformed response makes the assertion panic and crashes the
request.

Fix: decode into a typed struct, check `err`, then validate semantics (required
fields, bounds, enums). Never type-assert on model-controlled data you have not
validated.

### No loop bound

Wrong: `for { call; if tool_use { execute; continue }; break }` with no cap. A
model that keeps calling tools loops until something else times out, at full
token cost.

Fix: bound the loop on turns, tokens, and a `context` deadline. Return a sentinel
error when the turn budget is exceeded.

### Panicking on decode errors inside the loop

Wrong: letting a `json.Unmarshal` error propagate as a panic or an aborting Go
error for what is really a recoverable bad-argument condition.

Fix: turn it into a tool result with `is_error` set so the model can retry with
corrected arguments. Reserve aborting errors for genuinely unrecoverable
failures (auth, infra, cancellation).

### Treating the model as the executor

Wrong: assuming the model "ran" the tool, or feeding the result back in the wrong
role or turn.

Fix: internalize that the model only emits intent; your code executes and returns
the result in the exact role the protocol requires (a user turn of `tool_result`
blocks for Anthropic, `role=tool` messages for OpenAI).

### Passing a bare string where the SDK wants the optional wrapper

Wrong: setting `ToolParam.Description` or `FunctionDefinitionParam.Description` to
a plain `string`. Those fields are optional-value types.

Fix: wrap with `anthropic.String(...)` / `openai.String(...)`, the helpers that
build the SDK's optional string.

### Reading only the first content block and ignoring the stop reason

Wrong: reading `message.Content[0]` or `Choices[0].Message.Content` and never
checking `stop_reason` / `finish_reason`, so tool-use turns (which may carry no
text at all) are silently dropped.

Fix: branch on the stop reason first; when it signals tool use, iterate all
blocks / all tool calls.

### Registering too many, vaguely-described tools

Wrong: exposing thirty tools with one-word descriptions and blaming the model for
choosing wrong. Naming and descriptions are part of the contract; a bloated,
thinly-documented tool surface degrades selection accuracy.

Fix: keep the tool set small and each description precise. The schema is the spec
the model reads.

### No per-tool authorization or blast-radius control

Wrong: any tool the model can name, it can run with the arguments it chose — so a
prompt-injected model becomes a confused deputy invoking a destructive tool.

Fix: authorize each call inside the handler against the real caller, apply least
privilege, and gate high-impact tools behind human confirmation.

Next: [01-tool-registry-and-schema.md](01-tool-registry-and-schema.md)
