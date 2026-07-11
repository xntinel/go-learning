# Go Curriculum Verification Report

Verification of all lessons under `challenges/go/**/[0-9][0-9]-*/*.md`
with the gate defined in CURRICULUM-METHODOLOGY.md §14.1:

```
gofmt -l .          # must print empty
go vet ./...        # no findings
go build ./...      # compiles
go test -count=1 -race ./...   # tests pass
```

Plus the self-consistency layer (§15) for identifier presence, link
resolution, and URL reachability.

## Scope

| Metric | Count |
| --- | --- |
| Lesson directories (`NN-name/`) | 583 lessons (matches `challenges/go/go.md` count) |
| Lesson directories, empty/stale (defect) | 47 |
| Lesson files (`NN-name/NN-name.md`) with `NN-` prefix | 567 |
| Lesson files with non-`NN-` filename (defect, see §6) | 14 |
| `## Prerequisites` and `## Learning Objectives` section headers (banned by §5) | 420 each |
| `**Difficulty**:` lines (banned by §5) | 0 |
| `<details>` blocks (banned by §5) | 0 |
| Emojis or decorative unicode (banned by §0) | 0 |

## Toolchain

The local system Go is `go1.23.3 darwin/arm64`. The curriculum pins
`go 1.26` in `go.mod` directives and uses `t.Context()` (Go 1.24+), so
verification required Go 1.26:

```
$ curl -sL https://dl.google.com/go/go1.26.0.darwin-arm64.tar.gz \
    | tar -xz -C /var/folders/c7/.../opencode/go1.26 --strip-components=1
$ /var/folders/c7/.../opencode/go1.26/bin/go version
go version go1.26.0 darwin/arm64
```

All `verify_*` scripts use this toolchain via the `GO` and `GOFMT`
environment variables.

## Gate results (per chapter)

Captured by `challenges/tools/verify_chapter.py` walking
`challenges/go/<chapter>`, extracting every `Create \`*.go\`` block
into a fresh `go.mod` module under `/tmp/verify-XXXX`, and running the
four gate commands. Output is at `/tmp/verify-reports/all.json` and
`/tmp/verify-reports/fails.md`.

| Chapter | Pass | Fail | Skip | Total |
| --- | ---: | ---: | ---: | ---: |
| 01-environment-and-tooling | 6 | 2 | 0 | 8 |
| 02-variables-types-and-constants | 10 | 0 | 0 | 10 |
| 03-control-flow | 9 | 1 | 0 | 10 |
| 04-functions | 22 | 0 | 0 | 22 |
| 05-strings-runes-and-unicode | 10 | 0 | 0 | 10 |
| 06-collections-arrays-slices-and-maps | 14 | 0 | 0 | 14 |
| 07-structs-and-methods | 12 | 0 | 0 | 12 |
| 08-interfaces | 14 | 0 | 0 | 14 |
| 09-pointers | 10 | 0 | 0 | 10 |
| 10-error-handling | 14 | 0 | 0 | 14 |
| 11-packages-and-modules | 10 | 0 | 0 | 10 |
| 12-testing-ecosystem | 25 | 0 | 0 | 25 |
| 13-goroutines-and-channels | 13 | 1 | 0 | 14 |
| 14-select-and-context | 11 | 1 | 2 | 14 |
| 15-sync-primitives | 6 | 0 | 8 | 14 |
| 16-concurrency-patterns | 14 | 0 | 19 | 33 |
| 17-http-programming | 10 | 2 | 2 | 14 |
| 18-encoding-json-xml-protobuf | 0 | 0 | 12 | 12 |
| 19-io-and-filesystem | 0 | 0 | 18 | 18 |
| 20-generics | 7 | 0 | 7 | 14 |
| 21-structured-logging-with-slog | 6 | 0 | 4 | 10 |
| 22-database-patterns | 0 | 6 | 4 | 10 |
| 23-cli-applications | 4 | 5 | 1 | 10 |
| 24-design-patterns-in-go | 4 | 2 | 4 | 10 |
| 25-iterators-and-modern-go | 4 | 1 | 3 | 8 |
| 26-memory-model-and-optimization | 5 | 5 | 4 | 14 |
| 27-reflection | 5 | 2 | 3 | 10 |
| 28-unsafe-and-cgo | 4 | 3 | 3 | 10 |
| 29-code-generation-and-build-system | 6 | 4 | 0 | 10 |
| 30-production-patterns | 6 | 8 | 0 | 14 |
| 31-cloud-native-go | 0 | 0 | 11 | 11 |
| 32-concurrency-debugging-and-testing | 0 | 0 | 8 | 8 |
| 33-tcp-udp-and-networking | 1 | 2 | 25 | 28 |
| 34-runtime-scheduler | 0 | 0 | 10 | 10 |
| 35-runtime-garbage-collector | 0 | 0 | 10 | 10 |
| 36-runtime-compiler-and-assembly | 0 | 0 | 15 | 15 |
| 37-distributed-systems-fundamentals | 0 | 0 | 24 | 24 |
| 38-capstone-container-runtime | 0 | 0 | 10 | 10 |
| 39-capstone-database-engine | 0 | 0 | 10 | 10 |
| 40-capstone-language-interpreter | 0 | 0 | 8 | 8 |
| 41-capstone-message-queue | 0 | 0 | 8 | 8 |
| 42-capstone-service-mesh-data-plane | 0 | 0 | 9 | 9 |
| 43-capstone-stream-processing-engine | 0 | 0 | 8 | 8 |
| 44-capstone-http2-implementation | 0 | 0 | 6 | 6 |
| 45-capstone-distributed-key-value-store | 0 | 0 | 6 | 6 |
| 46-capstone-concurrency-deep-dive | 0 | 0 | 13 | 13 |
| 47-capstone-systems-and-kernel | 0 | 0 | 10 | 10 |
| **Total** | **263** | **45** | **305** | **613** |

Notes:

- The 263 pass count is the count of lessons that pass the full gate
  on the **first** extract. The 14 unrenamed `NN-`-less files in
  chapters 04, 17, 21, 24 were not picked up by the chapter script
  (which globs for `NN-*.md`) and are reported here in addition: 13
  of them pass, 1 fails (`24-design-patterns-in-go/01-functional-options.md`).
- "Skip" means no extractable `Create \`*.go\`` block was found.
  Many of those lessons use a hint-and-test pattern, prose-only
  exercises, or external service code. The gate does not apply to
  prose-only lessons (they are not the artifact kind the §14.1 gate
  targets). The methodology §10.2 calls this a known case for
  human/agent review.
- Chapter 16 (concurrency-patterns) and 33 (tcp-udp-and-networking)
  inflate "Skip" because their lessons use `Harness \`*.go\`` /
  `Harness Server \`*.go\`` patterns the regex does not match.
  This is an extractor gap, not a lesson defect; the lessons are
  probably runnable but the harness was not built to extract them.

## Defects found

### A. Gate failures (45 lessons)

Full evidence in `/tmp/verify-reports/fails.md`; classified by failure
mode:

| Mode | Count | Representative |
| --- | ---: | --- |
| `ext-dep` (network required, rule F) | 18 | `22-database-patterns/01..06` (all use `github.com/mattn/go-sqlite3`); `23-cli-applications/04..09` (cobra, huh, spinner); `30-production-patterns/02,05,07,08,12` (yaml.v3, uuid, otelhttp); `01-environment-and-tooling/02` (fatih/color); `17-http-programming/10` (gorilla/websocket) |
| `undefined-id` (rule C: compile error) | 9 | `30-production-patterns/11-timeout-budgets` (`undefined: context`); `24-design-patterns-in-go/01-functional-options.md` (`undefined: WithPort`); `29-code-generation-and-build-system/01,05` (undefined: `generatedVersion`, `NewConfig`); `26-memory-model-and-optimization/03,08` (undefined: `fmt`); `33-tcp-udp-and-networking/02` (undefined: `fmt`); `28-unsafe-and-cgo/05,06` (cgo in test files not supported); `29-code-generation-and-build-system/02` (`fmt.Printf` format/arg mismatch; missing `String()` from stringer) |
| `unused-import` (rule C: code hygiene) | 5 | `14-select-and-context/11-graceful-shutdown-with-context` (`"os" imported and not used`); `17-http-programming/07-cookie-and-session-management` (`"time"`); `24-design-patterns-in-go/05-repository-pattern` (`"fmt"`); `25-iterators-and-modern-go/08-standard-library-iterators` (`"cmp"`); `30-production-patterns/04-health-endpoints` (`"fmt"`) |
| `main-redeclared` (rule C: same package, two `func main`) | 4 | `26-memory-model-and-optimization/01-happens-before-relationships` (mutex_chain.go, main.go); `26/09-trace-tool-goroutine-scheduling`; `28-unsafe-and-cgo/01-unsafe-pointer-and-uintptr` (patterns.go, main.go); `33-tcp-udp-and-networking/01-tcp-server-and-client` (server.go, client.go) |
| `no-main` (rule C: a test/build directory with no `func main`) | 2 | `26-memory-model-and-optimization/04-benchmarking-methodology`; `29-code-generation-and-build-system/08-plugin-system` |
| `no-packages` (harness extracted only files that are not build targets) | 2 | `27-reflection/06-deepequal-and-custom-comparison`; `27-reflection/07-reflection-performance-costs` |
| `test-fail` (legitimately a debugging lesson, see §10 known false positives) | 1 | `03-control-flow/10-control-flow-debugging-challenge` — buggy state machine; test fails by design, the fix is in Exercise 4. The gate correctly catches the bug; the lesson is sound. |
| `other` (cgo in `_test.go`, format/arg mismatch) | 3 | `28-unsafe-and-cgo/05-passing-data-go-and-c`; `28-unsafe-and-cgo/06-cgo-performance-overhead`; `29-code-generation-and-build-system/02-stringer` |

Sample captured failure (raw output, no memory):

```
$ python3 challenges/tools/verify_lesson.py \
    challenges/go/30-production-patterns/11-timeout-budgets/11-timeout-budgets.md
lesson: challenges/go/30-production-patterns/11-timeout-budgets/11-timeout-budgets.md
  gofmt: OK
  vet: FAIL
    # timeout-budget
    # [timeout-budget]
    vet: ./main.go:91:47: undefined: context
  build: FAIL
    # timeout-budget
    ./main.go:91:47: undefined: context
  test: FAIL
    FAIL	timeout-budget [build failed]
verdict: FAIL
```

### B. Broken cross-references (FIXED in this pass)

Found 6 broken `## What's Next` cross-references. The link target
existed in the chapter under a different name; the prose used the
chapter-title-only path which the file system could not resolve.

| Lesson | Before | After |
| --- | --- | --- |
| `08-interfaces/14-interface-based-middleware-chain/14-interface-based-middleware-chain.md` | `../09-pointers-and-memory-layout/01-pointers-and-addresses/01-pointers-and-addresses.md` | `../../09-pointers/01-pointer-basics/01-pointer-basics.md` |
| `09-pointers/10-designing-pointer-safe-apis/10-designing-pointer-safe-apis.md` | `../01-error-handling-basics/01-error-handling-basics.md` | `../../10-error-handling/01-error-interface-and-basic-patterns/01-error-interface-and-basic-patterns.md` |
| `10-error-handling/14-error-observability/14-error-observability.md` | `../01-package-declaration-and-imports/01-package-declaration-and-imports.md` | `../../11-packages-and-modules/01-package-declaration-and-imports/01-package-declaration-and-imports.md` |
| `11-packages-and-modules/10-monorepo-module-strategy/10-monorepo-module-strategy.md` | `../01-testing-fundamentals/01-testing-fundamentals.md` | `../../12-testing-ecosystem/01-your-first-test/01-your-first-test.md` |
| `12-testing-ecosystem/25-building-a-test-suite/25-building-a-test-suite.md` | `../01-goroutines-basics/01-goroutines-basics.md` | `../../13-goroutines-and-channels/01-your-first-goroutine/01-your-first-goroutine.md` |
| `13-goroutines-and-channels/16-goroutine-debugging-under-load/16-goroutine-debugging-under-load.md` | `../01-select-basics/01-select-basics.md` | `../../14-select-and-context/01-select-statement-basics/01-select-statement-basics.md` |

Re-ran `self_consistency.check_links` on each — all now resolve.
The self-consistency issue count went from 120 to 114.

### C. Stale empty lesson directories (NOT FIXED)

`find challenges/go -mindepth 2 -maxdepth 2 -type d -empty` returns
**47 empty lesson directories**. These are leftovers from earlier
renames. They break the `NN-name/` convention and inflate directory
counts without content. Examples:

- `challenges/go/44-capstone-http2-implementation/01-frame-parsing-and-serialization/`
- `challenges/go/44-capstone-http2-implementation/03-stream-multiplexing-flow-control/`
- `challenges/go/44-capstone-http2-implementation/05-connection-and-stream-error-handling/`
- `challenges/go/45-capstone-distributed-key-value-store/02-replication-configurable-consistency/`
- `challenges/go/45-capstone-distributed-key-value-store/08-full-distributed-kv-store/`
- `challenges/go/46-capstone-concurrency-deep-dive/03-hazard-pointer-memory-reclamation/`
- `challenges/go/46-capstone-concurrency-deep-dive/07-concurrent-b-tree/`
- `challenges/go/46-capstone-concurrency-deep-dive/12-ring-buffer-lock-free-reads/`
- `challenges/go/47-capstone-systems-and-kernel/10-building-a-go-language-server/`
- `challenges/go/47-capstone-systems-and-kernel/12-go-to-webassembly-compiler/`
- `challenges/go/47-capstone-systems-and-kernel/13-interactive-go-debugger/`
- ... (and 36 more across the same chapters plus 18, 19, 33, 34, 40, 41, 42, 43)

Full list captured with `find challenges/go -mindepth 2 -maxdepth 2
-type d -empty`. None of them were removed in this pass — they are a
data-hygiene fix the user should handle.

### D. Unrenamed lesson files (NOT FIXED)

14 lesson files lack the `NN-` filename prefix (rule D in §14.1).
They are the files the chapter-level glob missed and were verified
manually. They pass the gate (13 of 14) but the file name breaks
inbound links and the convention.

| Lesson | File |
| --- | --- |
| 04-functions/03-variadic-functions | `variadic-functions.md` |
| 04-functions/04-first-class-functions-and-closures | `first-class-functions-and-closures.md` |
| 04-functions/05-anonymous-functions | `anonymous-functions.md` |
| 04-functions/06-function-types-and-callbacks | `function-types-and-callbacks.md` |
| 04-functions/07-recursive-functions-and-stack-depth | `recursive-functions-and-stack-depth.md` |
| 04-functions/08-init-functions-and-package-initialization | `init-functions-and-package-initialization.md` |
| 04-functions/09-closure-gotchas-loop-variable-capture | `closure-gotchas-loop-variable-capture.md` |
| 04-functions/10-higher-order-functions | `higher-order-functions.md` |
| 04-functions/11-defer-stacking-and-resource-cleanup | `defer-stacking-and-resource-cleanup.md` |
| 04-functions/12-functional-options-pattern | `functional-options-pattern.md` |
| 17-http-programming/14-http-client-retry-circuit-breaker-tracing | `14-http-client-retry-circuit-breaker.md` |
| 21-structured-logging-with-slog/05-slog-with-for-logger-enrichment | `05-slog-with-logger-enrichment.md` |
| 24-design-patterns-in-go/01-functional-options-deep-dive | `01-functional-options.md` (also FAILS the gate: `undefined: WithPort`) |
| 24-design-patterns-in-go/03-strategy-pattern-via-interfaces | `03-strategy-pattern.md` |

### E. Banned `## Prerequisites` / `## Learning Objectives` sections (NOT FIXED, corpus-wide)

The methodology §5 lists the canonical skeleton:

```
## Concepts
## Exercises
## Common Mistakes
## Verification
## Summary
## What's Next
## Resources
```

420 lessons still carry a `## Prerequisites` section and 420 carry a
`## Learning Objectives` section. Each of these belongs to the
methodology's "rejected" categories (Prerequisites is an info-card
field, not a lesson body section; Learning Objectives is the very
"Learning Objectives" the rule explicitly bans). This is a corpus-wide
mechanical find-and-remove pass; not done in this validation because
it does not block the gate and the user may want to preserve the
information under a different name (e.g. fold it into the lesson's
`## Concepts` intro).

## Self-consistency layer (§15)

Ran `challenges/tools/self_consistency.py --no-urls` across 573
lessons (the tool requires `NN-NN-*.md`; the 14 unrenamed files in
section D were not scanned).

- 459 OK
- 114 FAIL

| Issue type | Count | Status |
| --- | ---: | --- |
| `id:` (identifier named in prose not present in code) | 108 | Mostly false positives — the regex flags stdlib methods called through imports (`URL.Query`, `Header.Get`, `Body.Close`, `Row.Scan`, `DB.BeginTx`, `Value.Set`, `Mutex.Unlock`, `Tag.Get`, etc.). The check is too aggressive: it cannot distinguish "stdlib call" from "missing identifier". A context-aware rewrite of the check is required to make this useful. |
| `link:` (cross-reference target missing) | 6 | All 6 fixed in section B above. |

The `id:` class of issues requires per-lesson judgement to triage and
is not a hard defect signal; the corpus-level pattern (every HTTP
lesson flags `Header.Get`, every DB lesson flags `Row.Scan`) is
diagnostic of the check, not the lessons.

URL check was disabled with `--no-urls`; the lessons link to
`pkg.go.dev`, `go.dev`, and a handful of academic papers. A separate
pass with network is required to validate them.

## Pre-existing in-progress work

The working tree already had 8 lessons in flight when validation
started. They were re-validated and all pass the gate (the one
exception is `03-control-flow/10` which is a debugging lesson, see
the table above):

- `challenges/go/03-control-flow/10-control-flow-debugging-challenge/10-control-flow-debugging-challenge.md` (intentional failure)
- `challenges/go/04-functions/01-function-declaration-and-multiple-return-values/01-function-declaration-and-multiple-return-values.md`
- `challenges/go/04-functions/02-named-return-values/02-named-return-values.md`
- `challenges/go/05-strings-runes-and-unicode/08-unicode-normalization-and-collation/08-unicode-normalization-and-collation.md`
- `challenges/go/16-concurrency-patterns/05-generator-pattern/05-generator-pattern.md`
- `challenges/go/16-concurrency-patterns/06-errgroup-basic-usage/06-errgroup-basic-usage.md`
- `challenges/go/16-concurrency-patterns/07-errgroup-with-context/07-errgroup-with-context.md`
- `challenges/go/16-concurrency-patterns/15-bounded-parallelism/15-bounded-parallelism.md`
- `challenges/go/16-concurrency-patterns/17-error-group-parallel-error-handling/17-error-group-parallel-error-handling.md`

## Summary

- **Total lessons** in `challenges/go`: 583 .md files in 47 chapters
  (`go.md` index claim matches).
- **Pass the gate on first run**: 263 (45.2%).
- **Fail the gate on first run**: 45 (7.7%), of which 1 is a
  legitimate debugging lesson whose failure is the lesson's point.
- **No extractable `Create \`*.go\`` block**: 275 lessons. Many of
  these use non-`Create` filename headers (`Harness`, `Update`, or
  bare code blocks in hints/solutions) which the regex does not
  match; the harness should be extended to cover them before this
  class is treated as "skip".
- **Cross-references**: 6 broken; all fixed in this pass.
- **Hygiene defects** left for the user: 47 empty lesson
  directories, 14 unrenamed `NN-`-less files, 420 lessons with
  banned `## Prerequisites` and `## Learning Objectives` sections.
- **Tools written**: `challenges/tools/verify_lesson.py`,
  `verify_chapter.py`, `verify_all.py`, `capture_fails.py`,
  `capture_lesson.py`. Untracked; the user decides whether to keep
  them.

## Recommended next actions (not done in this pass)

1. Remove the 47 empty lesson directories (one `rmdir` per dir).
2. Rename the 14 unrenamed files to `NN-name/NN-name.md` and update
   the lesson's front-matter to match. `24-design-patterns-in-go/01-functional-options-deep-dive/01-functional-options.md`
   also has a real compile bug (`undefined: WithPort`) to fix.
3. Address the 18 `ext-dep` failures: either run `go mod tidy` with
   network in CI and check the resulting `go.sum` into the repo, or
   gate the network-dependent code behind `//go:build online` and
   add hermetic `httptest`/`fstest.MapFS` alternatives.
4. Fix the 9 `undefined-id` and 5 `unused-import` lessons (compile
   errors). These are small surgical edits; see the table in
   §A.
5. Decide whether the 4 `main-redeclared` lessons are supposed to be
   separate programs (in which case they need separate modules) or
   if the duplicate `func main()` is a copy-paste error.
6. Drop `## Prerequisites` and `## Learning Objectives` from the 420
   lessons that still carry them (or fold the info into
   `## Concepts` intros).
7. Improve the `self_consistency` `id:` check to ignore stdlib
   identifiers used through imports; today it is a false-positive
   source and not a useful signal.
8. Extend the extractor to handle `Harness \`*.go\`` /
   `Update \`*.go\`` patterns so the chapters with 10+ "skip" counts
   (16, 18, 19, 33, 31, 32, 34, 35, 36, 37, 38, 39, 40, 41, 43, 44,
   45, 46, 47) actually exercise the gate.
