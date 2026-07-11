# Go Curriculum Quality Rewrite - Progress Registry

Tracks the rewrite of all low-quality lessons (those with the banned
`bloom_level:` metadata marker) to the canonical standard in
`AGENT-ORDER-go-quality.md`. Runner: opencode + MiniMax-M3 with
`--dangerously-skip-permissions`, one chapter per run, audited per chapter by
the orchestrator (gofmt/vet/build/test gate + §15).

Total bad lessons at start: 420.

> AUTHORING & EXPANSION DOCS: the self-contained playbook for producing/expanding
> lessons at this quality bar lives in `challenges/go/docs/` (README + standard +
> gate + engine + expansion runbook). Read it before adding new lessons/topics.

## SESSION 2026-06-25 (cont.) — ch01 gated (no longer NEEDS-MANUAL)

01-environment-and-tooling (8 lessons) was already rewritten to the senior-backend
standard (real urlcheck/greeting/etc. artifacts, httptest, sentinels+%w, no
bloom_level / no LO/Prereq boilerplate). Gated all 8: 6 PASS out of the box; 2
real defects fixed:
- 03-go-workspace-and-project-layout: the Ex5 "Refactor" dance emitted a `func
  handler` block with no `package` clause under "Create cmd/server/main_test.go"
  (parse error), and the test had an invalid `if cond && got := f();` statement +
  an unused strings import. Fix: define `handler` as a top-level func in Ex4
  main.go (testable from the start), drop the mid-lesson refactor + the dead
  Content-Type block + strings import. PASS.
- 07-debugging-with-delve: a deliberate off-by-one in "Create internal/sum/sum.go"
  made the test fail; the corrected version used "Replace `...`:", which the gate
  does NOT honor (only Create/Save as, Append to/Add to). Fix: made the buggy
  block illustrative ("Write ..." instead of "Create `path`") and turned the
  fixed block into the real "Create internal/sum/sum.go". PASS.
ch01 now 8/8 PASS. Gate marker reminder: go_gate_append.py honors Create/Save as
+ Append to/Add to + `// path.go`/`// go.mod` headers; "Replace" is treated as
illustrative.

## SESSION 2026-06-25 (cont.) — FINISHED: ch33 + all capstones

DONE. bloom_level = 0 across challenges/go (except meta files + the separate
NEEDS-MANUAL 01-environment-and-tooling).

ch33 (28): 28/28. Capstones 38-47 (94): 94/94 gofmt-clean — final mechanical
sweep (go_gate_append.py over all 94) = 71 full PASS + 23 OFFLINE-BAR (build
fails only on external/Linux deps: cgo, github.com/cilium/ebpf, quic-go,
x/sys/unix, wazero, etc.), 0 gofmt-fails.

How the finish went: the first capstone engine run (wf_1f562fdf-612) hit the
session limit mid-way (reset 20:20 Lima) — generated 55/94 but verified none.
A second run (wf_e66a853c-7f7, generate only where bloom_level remained, verify
the rest) was stopped early for speed; instead the orchestrator ran the
mechanical gate over all 94 itself (fast, model-independent ground truth for
bar-mode), found 15 gofmt-fails + 1 ungenerated + a seccomp two-package-names
bug. A focused parallel Sonnet wave (.go-fix-gofmt.workflow.js) cleaned all 15
gofmt + fixed seccomp (now PASS). The last lesson, 46-07-concurrent-btree,
stalled TWO separate generator agents at the very first step, so it was written
by hand: a generic CLRS proactive-split B-tree guarded by a single sync.RWMutex
(parallel reads), with checkInvariants (equal leaf depth + [t-1,2t-1] occupancy)
and a TestConcurrent stress test that passes `go test -race`. Gate PASS.

Engine note: .go-quality-engine.workflow.js verifier set to model:'sonnet'
(line 121). Lesson learned: single very-large bar-mode capstone runs risk the
session limit; for bar-mode the mechanical gofmt/structure sweep by the
orchestrator is a fast, reliable substitute for 90+ LLM verifier agents.

## SESSION 2026-06-25 — finish run, all-Sonnet engine

Engine modified so the adversarial verifier also runs on Sonnet (line 121
`model: 'sonnet'`); generator + repair were already Sonnet. Gate
(go_gate_append.py) remains the model-independent ground truth.

CH33 (28 lessons) DONE: engine run wf_09207f8e-3f5 -> 27/28 OK, 78 agents.
The 1 FAIL (24-quic-transport-protocol, OFFLINE-BAR, hit the 2-round cap) was
fixed by hand: split the test into a hermetic framing file (bytes/errors/fmt/
strings/testing) + a `//go:build online` integration file
(quictransport_online_test.go, context/crypto/tls/fmt/sync/testing); removed the
Go-1.22-redundant `tc := tc`; switched the oversized-frame assertion to
errors.Is(err, ErrFrameTooLarge). gofmt -l clean on extracted code; build fails
only on the external quic-go module (expected OFFLINE-BAR). ch33 bloom_level = 0.

CAPSTONES 38-47 (94 lessons, all mode "bar") LAUNCHED: engine run
wf_1f562fdf-612 (task wd3j2to14), in background. On completion, hand-fix any
FAIL the same way (surgical, per the openIssues list in the result), then
bloom_level should be 0 across challenges/go except the meta files
(.worker-*, AGENT-ORDER-go-quality.md, QUALITY-PROGRESS.md, which mention the
marker as text). NEEDS-MANUAL still outstanding: 01-environment-and-tooling.

## SESSION 2026-06-24 — generator/adversarial-verifier/repair engine

Upgraded orchestration to the full methodology engine (CURRICULUM-METHODOLOGY.md
§9-§11): generator (Sonnet) -> independent adversarial verifier (Opus, runs the
real gate + rubric §14.1 A-E + §15, evidence-anchored) -> surgical repair
(Sonnet, capped at 2 rounds). Reusable workflow: challenges/go/.go-quality-engine.workflow.js
(args.chapters=[{name,lessons:[{path,mode:"gate"|"bar",generate}]}]). Shared
generator order: challenges/go/.worker-order-wave.md.

WAVE 1 GENERATION DONE (Sonnet subagents, 0 bloom_level remaining): 27 (03-10),
28 (06-10), 29 (01-10), 32 (01-08), 34 (01-10), 35 (01-10) = 51 lessons.
NOTE: Sonnet generators stop early — had to re-nudge with the explicit remaining
file list; they also self-report PASS but the adversary still finds rubric defects.

WAVE 1 ENGINE-VERIFIED: 51/51 OK.
- 28 (5): all OK; 28-08 = OFFLINE-BAR (cgo). 29 (10): all OK.
- 27 (8), 32 (8), 34 (10), 35 (10): 36 lessons, 35 OK on the engine pass.
  32-08 chaos-testing was the 1 FAIL escalated past the 2-round cap: the
  adversary correctly found TestReproducibleChaos asserted identical aggregate
  counts across seeded concurrent runs, which is unsatisfiable (shared rng across
  concurrent goroutines + cancel race). Fixed by hand: the test now asserts the
  injector's single-goroutine decision sequence is reproducible, and the concept
  prose was scoped accordingly. Re-gated PASS.
Adversary value confirmed: many lessons the generators self-reported PASS needed
1-2 repair rounds for rubric A-E/§15 defects; one had a real design bug.

GOTCHA: Workflow `args` arrives as a STRING; the script JSON.parse's it
(handled in .go-quality-engine.workflow.js).

BATCH 2 DONE: 30 (14) + 37 (24) = 38/38 OK. 30-07/08 (otel) = OFFLINE-BAR; rest
PASS. 37 all PASS (10-merkle + 11-service-discovery hit a generation API
"connection closed" error but the repair stage recovered them to PASS; verified
independently: no bloom_level, full canonical shape, gate PASS).

Helper: challenges/go/.gen-engine-args.py <chapter-dirs...> emits engine args with
the gate/bar classification baked in (BAR map per chapter; capstones all bar).

BATCH 3 IN PROGRESS: engine on 31 (11; only 09 gate, rest bar) + 36 (15; asm
09/10/11 + pgo 05 = bar, rest gate) = 26 lessons (run wf_122fd4ec-ed3).

NEXT after batch 3: 33 (28; grpc 13/14, quic 24/25, vpn 26, stun 27, bpf 28 =
bar, rest gate); then capstones 38-47 (116, all mode "bar") in ~30-lesson subbatches.
PROGRESS: 89/239 done (37%). Remaining after batch 3 lands: 124 (33 = 28;
38-47 = 116, minus the 28 once 33 done -> capstones are the bulk).

## PAUSE SNAPSHOT (resume here) — 2026-06-21

PROGRESS: ~150+ lessons DONE-verified. The big win this session was a TOOLING fix
(see "CRITICAL TOOLING FIX" below): the old gate gave false fails, so several
chapters previously logged as "0/N broken" were actually fine. Re-audited with
challenges/tools/go_gate_append.py (PRIMARY gate, go1.26 via GOTOOLCHAIN=auto).

DONE (verified PASS): 14 (14/14), 15 (14/14), 16 (28/28), 18 (12/12), 21 (9/9),
23 (10/10), 24 (9/9), 17 (14/14 — fixed 17-01 method-405/catch-all + fmt import;
the rest were tooling false-fails), 25 (8/8 — was a false "0/8").

DONE this session too (fix-ch19 subagent completed all 7 of its fixes; re-gated
PASS by Opus after it shut down):
- 19-io-and-filesystem: 18/18 effective PASS (19-06 is an embed artifact the gate
  cannot build — lesson is correct; all others incl. fixed 19-01 PASS).
- 20-generics: 14/14 PASS (01/02/03/04 fmt-import + gofmt fixed).
- 26-memory-model-and-optimization: 14/14 PASS (10/11 gofmt fixed).

REDO PARTIAL (Sonnet subagent redo-ch22, shut down mid-task):
- 22-database-patterns: REAL bug (module path had a `.md` suffix; demo imports a
  path no file provides). ~3/10 rewritten to a stdlib-only in-process fake
  database/sql driver that gates OFFLINE (01 PASS); ~7 still carry the `.md` bug.
  ON RESUME: finish the remaining ~7 the same way (no CGO sqlite / no networked
  driver) and re-gate all 10 with go_gate_append.py.

PARTIAL, UNGATED (gpt-5.5 ran out mid-chapter — treat as untrusted, likely buggy):
- 27-reflection: 2/10 rewritten (8 still have bloom_level).
- 28-unsafe-and-cgo: 5/10 rewritten (5 still have bloom_level). cgo lessons will
  not gate offline — apply the capstone bar (structural + gofmt/vet on extractable).

NEEDS-MANUAL: 01-environment-and-tooling (3rd-party deps, build tags, deliberate
go-vet demos; does not fit the library+gate model).

NOT STARTED: 29-37, capstones 38-47.

RUNNER STATUS: gpt-5.5 refreshed at session start, did 19/25/20/26 drafts + partial
27/28, then EXHAUSTED again ("se acabó"). It is FAST but does NOT actually run the
gate — every gpt-5.5 chapter shipped real defects (unused imports, missing fmt,
gofmt, .md module path) and must be independently audited + patched. Claude Sonnet
subagents are reliable IF told to gate with go_gate_append.py. MiniMax-M3 stalls.
NEXT ACTION on resume: (1) re-gate 19/20/22/26 and finish leftover fixes; (2) finish
27/28; (3) continue 29-37 then capstones. Use Sonnet (gpt-5.5 likely still empty).

CRITICAL TOOLING FIX (2026-06-21): the old subdir/marker assemblers gave MASSIVE
FALSE FAILS. They could not handle (a) `Append to`/`Add to` incremental files
(dropped the continuation block -> "undefined"/"unused import"), (b) the module
path declared via a `Create `go.mod`` block or `// go.mod` header (defaulted to
example.com/lesson -> demo imports failed), (c) `// path.go` comment-header file
style. NEW PRIMARY GATE: challenges/tools/go_gate_append.py handles all of these.
Re-audit with it before trusting any old "0/N" verdict. Examples corrected:
ch17 0/14 -> 14/14 (was all artifact); ch25 0/8 -> 8/8; ch19 03 false-fail.
REAL defects it still correctly catches: ch22 `.md`-in-module-path bug (redo),
ch19-01 missing bufio import, ch20 missing fmt imports + gofmt, ch26 gofmt.

VERIFICATION TOOLING (works, keep using):
- Go 1.26 toolchain: the temp PATH (/var/folders/.../T/opencode/go1.26/bin) is GONE.
  System go is 1.23.3 but `GOTOOLCHAIN=auto` (the default) auto-downloads go1.26
  whenever a go.mod requires `go 1.26`. Just `export GOTOOLCHAIN=auto` and run the
  gate; first build pulls toolchain@v0.0.1-go1.26.0. (corpus uses 1.24+ like
  t.Context; go1.23 gives false failures).
- challenges/tools/go_gate_append.py = PRIMARY (handles Create/Append/Add markers,
  `// path.go` and `// go.mod` headers, bare-basename appends, go.mod module path).
  Use this first: `GOTOOLCHAIN=auto python3 challenges/tools/go_gate_append.py <md> <work>`.
- challenges/tools/go_gate_subdir.py + go_gate_marker.py = legacy fallbacks only
  (kept for the rare lesson the primary marks NOGO). On a real FAIL inspect
  manually for genuine artifacts: embed (//go:embed needs asset files the gate
  does not create -> artifact), external deps (CGO/network -> artifact).
- Standard/order: challenges/go/AGENT-ORDER-go-quality.md
- Global playbook: ~/.claude/MULTI-AGENT-COLLABORATION-PLAYBOOK.md

## Status legend
- DONE: rewritten + gate passes + audited
- RUNNING: opencode run in flight
- PENDING: not started
- POLISH: rewritten but has minor follow-ups

## Block 0 - pilot (DONE)
| Chapter | Lessons | Status | Notes |
|---|---|---|---|
| 24-design-patterns-in-go | 9 | DONE | gate OK on all 9. Follow-ups: 05 lighter test coverage; 02 concept snippets use spaces (cosmetic). |

## RUNNER NOTE
opencode/MiniMax-M3 stalls under parallelism (>2 concurrent) and ships import
bugs. All OpenAI models are blocked by the ChatGPT-account auth ("model not
supported when using Codex with a ChatGPT account"). CURRENT RUNNER: opencode + openai/gpt-5.5 (works headless with the ChatGPT
account; codex/5.1/5.2 are blocked, gpt-5.5 is not). Concurrency 2. ch14/ch16
finishing on Claude Sonnet subagents (already in flight). Everything audited by
Opus with /tmp/gate.py + the Go 1.26 toolchain at
/var/folders/.../T/opencode/go1.26/bin (corpus uses 1.24+ features like
t.Context, so go1.23 gives false failures).

Model id: openai/gpt-5.5  (also available: gpt-5.5-fast, gpt-5.5-pro, gpt-5.4).

## Block 1 (58 lessons)
| Chapter | Lessons | Status | Notes |
|---|---|---|---|
| 01-environment-and-tooling | 7 | NEEDS-MANUAL | Special: tooling chapter (3rd-party deps golang.org/x/text, build tags, deliberate go-vet demos). Does not fit the library+gate model; review by hand. Fully rewritten by MiniMax. |
| 14-select-and-context | 14 | DONE | Sonnet subagent ch14: gate 14/14 PASS (verified by Opus, clean, no import bugs). |
| 15-sync-primitives | 14 | DONE | gate 14/14 PASS after orchestrator fixed 6 import/fmt defects (01,07,08,10,12,14). Polish: 01 lacks an Example. |
| 16-concurrency-patterns | 28 | DONE | Sonnet subagent ch16: gate 28/28 PASS (verified by Opus, whole chapter incl. MiniMax leftovers). |

## Block 2 (58 lessons) - RUNNING (gpt-5.5)
| Chapter | Lessons | Status | Notes |
|---|---|---|---|
| 17-http-programming | 14 | DONE | 14/14 PASS. The old "0/14" was a gate false-fail (Append-to + go.mod-block module path). Opus fixed 17-01 (removed dup demo block, added fmt import, fixed 405/catch-all design). |
| 18-encoding-json-xml-protobuf | 12 | DONE | gpt-5.5: 12/12 (18-03 multi-block artifact, lesson OK). |
| 19-io-and-filesystem | 18 | DONE | 18/18 PASS (06 = embed artifact, lesson OK). 01 bufio import fixed by fix-ch19, re-gated PASS. |
| 20-generics | 14 | DONE | 14/14 PASS. 01/02/03/04 fmt-import + gofmt fixed by fix-ch19, re-gated PASS. |

## Block 3 (51 lessons) - RUNNING (gpt-5.5)
| Chapter | Lessons | Status | Notes |
|---|---|---|---|
| 21-structured-logging-with-slog | 9 | DONE | gpt-5.5: 9/9 PASS (verified by Opus). |
| 22-database-patterns | 10 | REDO-PARTIAL | REAL bug (.md in module path). Sonnet redo-ch22 rewrote ~3/10 to a stdlib-only fake database/sql driver (01 gated PASS); ~7 still have `go mod init ...md`. Shut down mid-task. Finish the remaining 7 + re-gate on resume. |
| 23-cli-applications | 10 | DONE | gpt-5.5: 10/10 PASS via marker assembler (verified by Opus). |
| 25-iterators-and-modern-go | 8 | DONE | 8/8 PASS. The "0/8" was a gate false-fail (Append-to incremental files). Verified with go_gate_append.py. |
| 26-memory-model-and-optimization | 14 | DONE | 14/14 PASS. 10/11 gofmt fixed by fix-ch19, re-gated PASS. |

## Block 4 (55 lessons)
| Chapter | Lessons | Status | Notes |
|---|---|---|---|
| 27-reflection | 10 | PARTIAL-UNGATED | gpt-5.5 did 2/10 then ran out; ungated/untrusted. Finish + audit on resume. |
| 28-unsafe-and-cgo | 10 | PARTIAL-UNGATED | gpt-5.5 did 5/10 then ran out; ungated. cgo lessons won't gate offline (capstone bar). Finish + audit on resume. |
| 29-code-generation-and-build-system | 10 | PENDING | |
| 30-production-patterns | 14 | PENDING | |
| 31-cloud-native-go | 11 | PENDING | |

## Block 5 (46 lessons)
| Chapter | Lessons | Status | Notes |
|---|---|---|---|
| 32-concurrency-debugging-and-testing | 8 | PENDING | |
| 33-tcp-udp-and-networking | 28 | PENDING | |
| 34-runtime-scheduler | 10 | PENDING | |

## Block 6 (49 lessons)
| Chapter | Lessons | Status | Notes |
|---|---|---|---|
| 35-runtime-garbage-collector | 10 | PENDING | |
| 36-runtime-compiler-and-assembly | 15 | PENDING | |
| 37-distributed-systems-fundamentals | 24 | PENDING | |

## Block 7 - capstones (46 lessons, structural + §15 only)
| Chapter | Lessons | Status | Notes |
|---|---|---|---|
| 38-capstone-container-runtime | 10 | PENDING | not compilable offline |
| 39-capstone-database-engine | 10 | PENDING | |
| 40-capstone-language-interpreter | 8 | PENDING | |
| 41-capstone-message-queue | 8 | PENDING | |
| 42-capstone-service-mesh-data-plane | 10 | PENDING | not compilable offline |

## Block 8 - capstones (48 lessons, structural + §15 only)
| Chapter | Lessons | Status | Notes |
|---|---|---|---|
| 43-capstone-stream-processing-engine | 8 | PENDING | |
| 44-capstone-http2-implementation | 6 | PENDING | |
| 45-capstone-distributed-key-value-store | 8 | PENDING | |
| 46-capstone-concurrency-deep-dive | 13 | PENDING | |
| 47-capstone-systems-and-kernel | 13 | PENDING | |

## Expansion wave - trending topics (chapters 48-54, SCAFFOLDED 2026-06-26)

68 lesson stubs scaffolded (dirs + numbered H1 stubs + chained `What's Next` +
per-chapter `.worker-order-NN.md` briefs + `go.md` index rows 584-651). Content
NOT yet generated. Ranking and rubric: `docs/06-trending-topics-ranking-2026.md`.
Generate via the engine in `gate`/`bar` batches per `docs/04-expansion-runbook.md`.

| Chapter | Lessons | Status | Notes |
|---|---|---|---|
| 48-modern-go-language-and-stdlib | 16 | IN PROGRESS 6/16 | 01-06 DONE, adversarially reviewed, then CONVERTED to the modular runbook format (docs/07-modular-lesson-format.md): each lesson = 00-concepts.md (prose + Common Mistakes, no index) + NN-<slug>.md independent self-contained Go modules (own go.mod, code, cmd/demo with Expected-output, *_test.go), each gating ALONE. Module counts: 01 os.Root=1, 02 synctest=5 (ttl/janitor/sliding/deadline/retry), 03 unique=1, 04 weak=2, 05 WaitGroup.Go=2, 06 aliases=1 = 12 modules. All 12 gate PASS go1.26. go.md rows 01-06 -> 00-concepts.md; ch47 link + cross-lesson links fixed. 07-16 still SCAFFOLDED (old stub homes, convert with the runbook) |
| 49-application-security-crypto-supplychain | 11 | SCAFFOLDED | 01-04 gate; 05-11 bar (external/tooling) |
| 50-messaging-and-event-driven | 10 | SCAFFOLDED | bar; 06-07 outbox/inbox gate via SQLite |
| 51-rpc-and-api-design | 7 | SCAFFOLDED | bar (connect/grpc-gateway/buf/gqlgen) |
| 52-ai-llm-backends | 8 | SCAFFOLDED | bar (network); MCP + SDK + pgvector RAG |
| 53-wasm-and-extensibility | 6 | SCAFFOLDED | 01-03 gate (wazero pure-Go); 04-06 bar |
| 54-cloud-native-platform-and-orchestration | 10 | SCAFFOLDED | bar; Docker SDK, kubebuilder, KEDA, gocloud, Redis (cache/locks/rate-limit/streams) |
