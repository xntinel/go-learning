# 03 — The Engine & Methodology

How to produce lessons at scale: the generate -> adversarial-verify -> repair
pipeline, its tools, and the batching rules. Methodology source:
`challenges/CURRICULUM-METHODOLOGY.md` §9-§11 (engine), §14.1 (rubric), §15.

## The pipeline (per lesson)

```
generate (Sonnet)  ->  adversarial verify (Sonnet, independent)  ->  surgical repair (Sonnet)
   writes the .md       runs the REAL gate + rubric §14.1 A-E + §15,    blocker/major issues only,
   self-gates           returns {ok, gateResult, issues[]} w/ evidence  2-round cap then escalate
```

- The **verifier is adversarial and blind**: it assumes a defect exists, derives
  pass criteria itself from the rubric, runs the gate via Bash, and cites the
  exact line/quote/gate-output for every verdict. Its value is real — it catches
  defects generators self-report as PASS.
- **Repair is surgical**: only the listed blocker/major issues, no full rewrite.
  Capped at 2 rounds; a still-failing lesson escalates to the orchestrator
  (hand-fix or hand-write).

## Tool: the engine workflow

`../.go-quality-engine.workflow.js` — run via the `Workflow` tool with
`{scriptPath: ".../.go-quality-engine.workflow.js"}` (it is NOT in the named
registry, so pass `scriptPath`, not `name`). All three stages run on **Sonnet**
(verifier model set on the `agent()` call). The gate stays the model-independent
ground truth.

> The `Workflow` tool requires explicit user opt-in (multi-agent orchestration is
> not auto-invoked). The user must ask for it ("use a workflow" / "run the
> engine" / ultracode on). If you cannot run the engine, you can still produce
> lessons one at a time with the `Agent` tool (Sonnet) + the gate, or by hand —
> the standard, gate, and criteria are identical either way.
> The verifier output schema (`VERIFY_SCHEMA`) and repair schema live inside the
> workflow script itself; edit them there if the rubric evolves.

Args shape (passed as the Workflow `args` JSON value):

```json
{ "chapters": [
  { "name": "NN-chapter", "lessons": [
    { "path": "challenges/go/NN-chapter/MM-lesson/MM-lesson.md",
      "mode": "gate", "generate": true }
  ] }
] }
```

- `mode`: `"gate"` (must print PASS) or `"bar"` (offline capstone bar — gofmt+vet
  on extractable + §15; build/test deferred).
- `generate`: `true` to (re)write then verify; `false` to verify-only an
  already-written lesson.
- The engine pipelines lessons (no barrier); each runs generate -> up to 3
  verify rounds with repair between. Returns `{total, ok, passed[], failed[]}`.

## Tool: the args helper

`../.gen-engine-args.py <chapter-dir> [<chapter-dir> …]` emits the args JSON with
mode classification baked in. It carries a `BAR` map (per chapter, the lesson
`NN` prefixes that can't build offline — grpc/quic/otel/aws-sdk/asm/pgo/cgo) and
auto-marks all capstone chapters (38-47) as `bar`. Everything else defaults to
`gate`. Extend the `BAR` map when adding chapters with external/Linux deps.

```bash
python3 challenges/go/.gen-engine-args.py 17-http-programming > /tmp-in-repo/args.json
```

To resume a partial run (e.g. after a session limit), build args where
`generate = (lesson still has bloom_level / is missing)` and `generate=false`
for the rest, so finished lessons are only re-verified, not regenerated.

## Tool: the focused-fix workflow

`../.go-fix-gofmt.workflow.js` — a template for a single parallel Sonnet wave that
applies targeted fixes (gofmt cleanups, a specific bug, one missing generation)
without a full re-verify pipeline. Edit its arrays for the lessons/fixes at hand
and run via `Workflow({scriptPath})`. Much faster than re-running the engine for
a handful of known defects.

## Batching rules (avoid the session limit)

- **Keep batches ~25-30 gate-mode lessons.** A single very-large run (90+
  lessons) WILL hit the Claude session limit mid-run and verify nothing it
  generated — wasted work.
- **For bar-mode, prefer the mechanical sweep** (see `02-gate-tool.md`) over 90+
  LLM verifiers: faster, model-independent, and the gofmt/structure bar is the
  real bar for bar-mode anyway.
- **If a generator agent stalls** on a hard lesson (no transcript writes for
  several minutes — happens on the hardest concurrency/systems lessons), kill it
  and hand-write the lesson against the standard + gate.
- After each batch: update `../QUALITY-PROGRESS.md` (files touched, per-lesson
  PASS/FAIL, second-pass list, broken `What's Next` links).

## Cost / throughput reference

- ~1.2M tokens per ~14 lessons (verify+repair only); ~4.4M per ~38 (full).
- A 28-lesson gate-mode chapter ran ~85 min wall-clock, ~78 agents, on the
  engine with concurrency ~10-14.

## Gotchas

- Workflow `args` may arrive as a STRING -> the engine `JSON.parse`s it (handled).
- Sonnet chapter-generators (one Agent over a whole chapter) STOP EARLY; the
  per-lesson engine does not — drive per lesson.
- Generation API "connection closed" / output-token-max errors on a lesson are
  usually recovered by the repair stage or a re-run; the rare hard stall needs a
  hand-write.
- Bar-mode darwin diagnostics (`syscall.CLONE_NEWUTS undefined`, `Cloneflags`,
  external `BrokenImport`) are EXPECTED, not defects.
