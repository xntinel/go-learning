# 04 — Expansion Runbook (adding exercises & topics)

Step-by-step to add new lessons, chapters, or whole topics at the same quality
bar. Pairs with `01-quality-standard.md` (what), `02-gate-tool.md` (proof), and
`03-engine-and-methodology.md` (how to scale).

## Current chapter map (47 chapters)

| # | chapter | lessons | # | chapter | lessons |
|---|---|---|---|---|---|
| 01 | environment-and-tooling | 8 | 25 | iterators-and-modern-go | 8 |
| 02 | variables-types-and-constants | 10 | 26 | memory-model-and-optimization | 14 |
| 03 | control-flow | 10 | 27 | reflection | 10 |
| 04 | functions | 12 | 28 | unsafe-and-cgo | 10 |
| 05 | strings-runes-and-unicode | 10 | 29 | code-generation-and-build-system | 10 |
| 06 | collections-arrays-slices-and-maps | 14 | 30 | production-patterns | 14 |
| 07 | structs-and-methods | 12 | 31 | cloud-native-go | 11 |
| 08 | interfaces | 14 | 32 | concurrency-debugging-and-testing | 8 |
| 09 | pointers | 10 | 33 | tcp-udp-and-networking | 31 |
| 10 | error-handling | 14 | 34 | runtime-scheduler | 12 |
| 11 | packages-and-modules | 10 | 35 | runtime-garbage-collector | 10 |
| 12 | testing-ecosystem | 25 | 36 | runtime-compiler-and-assembly | 15 |
| 13 | goroutines-and-channels | 16 | 37 | distributed-systems-fundamentals | 24 |
| 14 | select-and-context | 16 | 38 | capstone-container-runtime | 10 |
| 15 | sync-primitives | 14 | 39 | capstone-database-engine | 13 |
| 16 | concurrency-patterns | 28 | 40 | capstone-language-interpreter | 15 |
| 17 | http-programming | 14 | 41 | capstone-message-queue | 13 |
| 18 | encoding-json-xml-protobuf | 16 | 42 | capstone-service-mesh-data-plane | 11 |
| 19 | io-and-filesystem | 25 | 43 | capstone-stream-processing-engine | 10 |
| 20 | generics | 14 | 44 | capstone-http2-implementation | 9 |
| 21 | structured-logging-with-slog | 9 | 45 | capstone-distributed-key-value-store | 10 |
| 22 | database-patterns | 10 | 46 | capstone-concurrency-deep-dive | 16 |
| 23 | cli-applications | 10 | 47 | capstone-systems-and-kernel | 16 |
| 24 | design-patterns-in-go | 9 | | | |

Expansion wave SCAFFOLDED 2026-06-26 (stubs only, content pending — see
`06-trending-topics-ranking-2026.md` and `../QUALITY-PROGRESS.md`):
48 modern-go-language-and-stdlib (16), 49 application-security-crypto-supplychain
(11), 50 messaging-and-event-driven (10), 51 rpc-and-api-design (7),
52 ai-llm-backends (8), 53 wasm-and-extensibility (6),
54 cloud-native-platform-and-orchestration (10). go.md rows 584-651.

Candidate directions to add or DEEPEN (senior-backend relevant). Several overlap
existing chapters — confirm against the map above and with the user before
generating, and prefer deepening over duplicating:
- Partially covered already (extend, don't duplicate): observability/OTel (30,
  31), gRPC end-to-end (33 has streaming/interceptors), structured logging (21),
  fuzzing & profiling/pprof (touched in 12, 26, 32, 36), database patterns (22).
- Thinner / candidate new topics: message-broker clients (Kafka/NATS),
  caching & rate-limiting, auth & app-side crypto (JWT/TLS/mTLS), config &
  secrets, resilience (retries/circuit breakers/backoff), API design
  (REST/JSON:API/gRPC-gateway), WebAssembly host/runtime, embedding & plugins.

Always confirm scope (which topics, how many lessons, gate vs bar) with the user
before generating.

## Step-by-step: add lessons to an EXISTING chapter

1. **Pick the topics** and decide lesson order (each lesson builds one testable
   artifact around one idea). Aim for the depth of the gold lessons.
2. **Scaffold the files** (the engine generates content, but the folder + a stub
   .md must exist so the path resolves):
   ```bash
   ch=challenges/go/NN-chapter
   for slug in MM-new-lesson MM2-another; do
     mkdir -p "$ch/$slug"
     printf '# X. Title\n' > "$ch/$slug/$slug.md"   # H1 with the right number
   done
   ```
3. **Wire navigation**: the previous lesson's `## What's Next` must link to the
   new one, and the new one links forward. Links are relative `.md` paths and
   MUST resolve (`test -f` the target).
4. **Classify gate vs bar**: testable offline -> `gate`; cgo/external module/
   Linux-only/asm/profile -> `bar` (add the `NN` prefix to the `BAR` map in
   `../.gen-engine-args.py`).
5. **Generate + verify** via the engine in batches of ~25-30 (see
   `03-engine-and-methodology.md`). Build args by hand or with the helper.
6. **Verify**: confirm `bloom_level` count is 0 and the gate/sweep is clean for
   the new lessons.
7. **Register**: update `../QUALITY-PROGRESS.md` and any chapter index.

## Step-by-step: add a NEW chapter or topic

1. Choose `NN-chapter-name` (next free number, or renumber deliberately — note
   that renumbering ripples into `What's Next` links and any index).
2. Create `NN-chapter-name/MM-lesson/MM-lesson.md` stubs (zero-padded `NN`/`MM`).
3. Draft a one-line learning arc per lesson (what artifact each builds) so the
   engine has a clear target; or write a `.worker-order-<chapter>.md` brief like
   the existing `../.worker-order-wave.md`.
4. Same generate -> verify -> register loop as above.
5. If the chapter has external/Linux deps, set its lessons to `bar` and validate
   with the mechanical gofmt sweep, not LLM verifiers.

## Lesson layout: split into files (current convention)

Lessons are authored as a directory of focused files, not one big `.md`, so they
can grow:

```
NN-name/
  NN-name.md      home index: H1 (`# N. Title`), intro, file tree, links to the
                  parts, Summary, What's Next, Resources
  concepts.md     ## Concepts (### subsections) + ## Common Mistakes
  ex-01-<slug>.md one file per exercise, each with "## What this teaches"
  ex-02-<slug>.md (explain + expand the facet) then "## The code" (Create/Append)
  ...             the last exercise file also carries ## Verification
```

Rules: the home keeps the `# N. Title` H1 (inbound links resolve to `NN-name.md`).
Code blocks live only in the `ex-NN` files; `concepts.md`/home have none. Files
gate as one unit via `go_gate_lesson.py` (sorted concatenation — Create before
Append; see `02-gate-tool.md`). Each exercise must teach AND test one facet, so
adding a topic = adding one `ex-NN` file plus an index link. The original
single-file lessons can be converted with `challenges/tools/split_go_lesson.py`.

## Definition of done (per lesson)

- No `bloom_level:` / banned sections; `# N. Title` intact.
- §5 skeleton present; `## Concepts` teaches the "why".
- Real `*_test.go` + `Example //Output` + `cmd/demo/main.go`; sentinels + `%w`
  + `errors.Is`; TAB-indented; no emojis; English.
- Gate: `PASS` (gate-mode) or clean gofmt + §15 (bar-mode).
- §15 holds: prose matches code; outputs match real runs.
- `What's Next` and `Resources` links resolve.

## Hard-won lessons (read before a big run)

1. Keep batches ~25-30. Large runs hit the Claude **session limit** mid-way and
   verify nothing they generated. If it happens, resume with `generate=false`
   for already-written lessons.
2. For bar-mode, run the **mechanical gofmt sweep yourself** (parallel `xargs`)
   instead of 90+ LLM verifiers — faster and model-independent.
3. If an agent **stalls** on a hard lesson (no transcript writes for minutes),
   kill it and **hand-write** the lesson against the standard + gate. (Example:
   `46-07-concurrent-btree` stalled two agents; hand-written as a CLRS
   proactive-split B-tree under `sync.RWMutex` with `TestConcurrent` passing
   `-race`.)
4. **Never trust a generator's self-report** — the gate is truth.
5. Common real defects to expect and fix: missing `package` clause in an
   assembled block; an unrecognized `Replace` marker shipping buggy code;
   Go-1.22 `tc := tc` redundancy; unused imports; struct-field gofmt alignment;
   network in the default test path (move behind `//go:build online`).

## Where to look when extending the tooling

- Quality criteria: `../AGENT-ORDER-go-quality.md`, `01-quality-standard.md`.
- Gate internals/markers: `../../tools/go_gate_append.py`, `02-gate-tool.md`.
- Engine/args/focused-fix: `../.go-quality-engine.workflow.js`,
  `../.gen-engine-args.py`, `../.go-fix-gofmt.workflow.js`,
  `03-engine-and-methodology.md`.
- History & state: `../QUALITY-PROGRESS.md`.
