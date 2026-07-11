# Vulnerability Scanning with govulncheck — Concepts

Running `govulncheck ./...` at a terminal and reading the output is a five-minute
task. Owning the CI gate that decides whether a release ships is a different job,
and it is the one a senior backend engineer actually does. The load-bearing skill
is turning govulncheck's reachability analysis into a deterministic, auditable
policy: parse the machine-readable stream instead of scraping human text,
separate findings that are actually called from advisories that are merely
imported, and fail the pipeline only on what is exploitable in this binary — with
a bounded, justified escape hatch so the gate stays on. This file is the
conceptual foundation for the three exercises: a stream parser, a triage gate,
and a programmatic runner with correct exit-code interpretation.

## Concepts

### Reachability is the whole point

Most software-composition-analysis (SCA) tools flag every dependency whose
version appears in an advisory: if `go.mod` names a vulnerable module, you get an
alert, whether or not your code ever touches the vulnerable code path. govulncheck
is different. It reports a vulnerability only when your code *transitively calls a
vulnerable symbol* — a specific function or method — not merely because a
vulnerable module is present. That is why it is low-noise: a CVE in a parser you
never invoke does not wake anyone at 3 a.m. The trade-off is that reachability
requires source (or, in binary mode, symbol tables) and is precise only when the
scan reaches symbol granularity. Reachability is not a severity score; it is a
statement about *your* call graph.

### The three scan levels form a precision ladder

govulncheck can answer the question at three depths, and `config.scan_level` tells
you which one you actually got:

- `module` — is a vulnerable module present in the build at all?
- `package` — is the vulnerable *package* imported?
- `symbol` — is the vulnerable *function* actually called?

Source-mode scans normally reach `symbol`. Binary-mode scans and findings that
land in the standard library often degrade to package or module granularity,
because a stripped binary lacks the information for a full symbol-level call
trace. This matters for policy: "reachable" only means "a vulnerable symbol is
called" when the achieved granularity is `symbol`. At coarser granularity a
finding says "this vulnerable code is present and might be reached", which is a
weaker claim. A gate must interpret "reachable" relative to the granularity it was
handed, not assume symbol precision it did not get.

### The exit-code contract is the number-one CI footgun

In its default text mode, govulncheck exits with code `3` when it finds
vulnerabilities and `0` when it does not (a general tool error is a different
nonzero code, typically `1`). That is what makes `govulncheck ./...` usable as a
plain shell gate. But the moment you request machine-readable output — `-json`,
`-format sarif`, or `-format openvex` — govulncheck **always exits `0`**,
regardless of how many vulnerabilities it found. The rationale is that structured
output is meant to be consumed by a program that computes its own verdict; the
tool refuses to double-signal through the exit code.

The consequence is a silent, dangerous failure mode:

```text
# WRONG: with -json the exit is always 0, so this always deploys
govulncheck -json ./... && deploy
```

A pipeline written this way ships vulnerable code every time. There are exactly
two correct shapes: gate on the text-mode exit `3`, or request `-json` and parse
the stream to compute your own exit code. This lesson builds the second shape,
because a real gate wants the structured data anyway (for triage, suppression,
and PR comments).

### The output is a stream of single-key envelopes, not one document

govulncheck `-json` does not emit one JSON document. It emits a *stream* of
newline-delimited objects, each an envelope with exactly one populated key:

```json
{"config":{"protocol_version":"v1.0.0","scan_level":"symbol", ...}}
{"progress":{"message":"Scanning your code and 312 packages ..."}}
{"osv":{"id":"GO-2024-2687","summary":"..."}}
{"finding":{"osv":"GO-2024-2687","fixed_version":"v0.23.0","trace":[ ... ]}}
```

The envelope keys are `config`, `progress`, `SBOM`, `osv`, and `finding`. `config`
is always emitted first and carries `protocol_version`, `scan_mode`, `scan_level`,
`db`, `db_last_modified`, `go_version`, and the scanner name and version. An `osv`
message is emitted for every advisory the database considers applicable to your
module set; a `finding` is emitted for each actual hit and carries the OSV id, the
`fixed_version`, and a `trace` — a slice of frames ordered from the most precise
resolved leaf (frame 0) up to your program's entry point. Frame 0 is a symbol when
the finding reached symbol granularity, but only a package or a module when it did
not, so its shape encodes the granularity rather than always naming a symbol.
Because the output is a concatenation
of objects, you must decode it *message by message* with a streaming decoder
(`json.NewDecoder(r).Decode` in a loop until `io.EOF`), not with a single
`json.Unmarshal` over the whole buffer, which fails after the first object.

### One OSV yields many findings; a gate must group and take the max

This is the subtlety that trips up first attempts at a gate. govulncheck does not
emit one finding per vulnerability. It emits findings *as it does work*, so a
single OSV usually produces several: one at module level when it sees the
vulnerable module is required, one at package level when it sees the package is
imported, and one or more at symbol level when it resolves an actual call. They
arrive least-precise first (module, then package, then symbol). A single
*reachable* vulnerability therefore shows up as roughly three findings, only one
of which is symbol-level.

The consequence for a gate is concrete. If you count raw `finding` messages you
inflate the number of problems — three findings for one reachable CVE reads as
three CVEs. Worse, if you triage each raw finding independently, the same OSV lands
in two buckets at once: its symbol-level finding is reachable and blocks, while its
module-level finding is unreachable and is filed as informational, so one advisory
is simultaneously "blocking" and "merely present". The fix is to **group findings
by OSV id and reduce each group to a single vulnerability that carries the maximum
granularity** any finding for that OSV reached — symbol if any finding was
symbol-level, else package, else module. Count and triage that deduplicated,
one-per-OSV view, never the raw finding stream. This grouping step is the piece a
CI-gate author must implement themselves; the tool does not do it for you.

### The message types are internal; the JSON schema is the contract

It is tempting to import govulncheck's own message structs to parse the stream.
You cannot: they live under `golang.org/x/vuln/internal`, and the `internal`
directory rule makes them unimportable outside that module. This is deliberate.
The stable, supported contract is (1) the documented JSON schema and (2) the
`golang.org/x/vuln/scan` programmatic API. So you define your own structs against
the schema — pinning `protocol_version` so you notice a breaking change — rather
than reaching into internal packages that can move without notice.

### The programmatic runner mirrors os/exec

`golang.org/x/vuln/scan` lets you drive govulncheck from Go without shelling out.
`scan.Command(ctx, args...)` returns a `*scan.Cmd` that mirrors `os/exec.Cmd`:
you set `Stdout`, `Stderr`, and `Env`, then call `Start` and `Wait`. To retrieve
the underlying process exit code, `errors.As` the error returned by `Wait` against
an `interface{ ExitCode() int }`. One elegant detail: `Cmd.Stdin`, if set, is
expected to be prior `govulncheck -json` output, which lets you re-render a
captured scan into another format (SARIF, OpenVEX) without rescanning the code.
Because you will run with `-json`, remember the exit-0 contract: `Wait` returns
nil even when the scan found vulnerabilities, so your verdict must come from the
parsed stream, not from the exit code.

### No CVSS by design; triage is yours

The Go security team deliberately omits CVSS-style severity numbers from the
database. Impact is context-dependent: a parser denial-of-service is critical when
it faces untrusted input and negligible behind a trusted-config boundary. A single
imported number would be misleading, so triage must be driven by reachability plus
*your* exposure model, not by a severity label the tool refuses to invent.

### A gate needs a bounded escape hatch

A gate that hard-fails on every advisory — including module-level, imported-but-
uncalled ones — produces alert fatigue, and fatigued teams comment out the gate.
That is worse than a strict gate with an escape hatch. The production pattern is a
suppression allowlist keyed by OSV id, where each entry carries a mandatory
justification and an expiry date, and the gate **fails closed** when a suppression
expires: an expired suppression re-activates the finding and blocks the build.
This keeps the gate strict while giving a bounded, auditable window to remediate a
finding that cannot be fixed today (no upstream patch yet, a pinned transitive
dependency, a scheduled migration). Static allowlists with no expiry are the
anti-pattern: a one-time suppression silently becomes a permanent acceptance.

### Database freshness and reproducibility

govulncheck queries `vuln.go.dev` by default, so results change over time as new
advisories land — a commit that was clean at merge can be vulnerable a week later.
Record `db_last_modified` and the scanner version from `config` for the audit
trail, and decide the policy question explicitly: does CI fail an already-released
commit when a new advisory drops (a nightly rescan of `main`), or only re-gate at
merge time? Pin the govulncheck version so a toolchain bump does not silently
change verdicts and break reproducibility.

### SARIF complements, it does not replace

`-format sarif` emits findings in the format GitHub code scanning ingests, so they
surface as annotations in the Security tab and are tracked over time. That is the
right output for visibility and trend tracking, but it exits `0` like all
structured formats and is not, by itself, a merge blocker. Emit SARIF for the
security tab *and* run the reachability gate that blocks the merge; the two are
complementary, not alternatives.

## Common Mistakes

### Relying on the exit code with -json

Wrong: `govulncheck -json ./... && deploy`, assuming a nonzero exit stops the
pipeline. With `-json` the exit is always `0`, so vulnerable builds ship silently.

Fix: either gate on text-mode exit `3`, or request `-json` and compute the verdict
from the parsed stream. Never trust the exit code in a structured-output run.

### Treating any nonzero exit as "vulnerabilities found"

Wrong: mapping every `exit != 0` to "vulnerable". Exit `3` specifically means
vulnerabilities were found in text mode; other nonzero codes mean the scan itself
failed — a network error, a build error, a missing toolchain — and must be handled
as an infrastructure failure, not as a clean pass and not as a vuln finding.

Fix: classify the exit code: `0` clean, `3` vulnerable, anything else scan-failed.

### Blocking on every advisory

Wrong: failing CI on every `osv`/`finding`, including module-level advisories for
code you never call. This is exactly the alert fatigue that gets gates disabled.

Fix: block on reachable (symbol-level) findings; report the rest as informational.

### Counting raw findings instead of vulnerabilities

Wrong: treating each `finding` message as a distinct vulnerability, and triaging
them one at a time. Because govulncheck emits a module-, package-, and symbol-level
finding for the same OSV, this triple-counts a single reachable CVE and can file
that one OSV as both blocking (its symbol finding) and informational (its module
finding) at the same time.

Fix: group findings by OSV id, reduce each group to one entry at the maximum
granularity reached, and count and triage that deduplicated view.

### Importing the internal message types

Wrong: `import "golang.org/x/vuln/internal/govulncheck"` to get the `Finding`
struct. It is unimportable outside that module.

Fix: define your own structs against the public JSON schema and pin
`protocol_version`.

### Parsing the whole output with one Unmarshal

Wrong: `json.Unmarshal(all, &doc)` over the entire `-json` output. The stream is a
concatenation of objects; `Unmarshal` errors after the first one.

Fix: loop a `json.Decoder`, decoding one envelope at a time until `io.EOF`.

### Scraping the human-readable text

Wrong: running default text mode and matching its layout with regexes. The text
output is for humans and its format is not a stable contract.

Fix: use `-json` (for your own gate) or `-sarif` (for code scanning).

### Assuming binary mode gives symbol precision

Wrong: treating a binary-mode result as if every finding were symbol-level. A
binary generally lacks the information for full call traces, so findings are
coarser.

Fix: read `config.scan_level` and interpret "reachable" relative to it.

### Suppressions with no expiry

Wrong: a static allowlist entry that never expires. The vulnerability is silently
accepted forever, and no one revisits it.

Fix: require a justification and an expiry on every suppression, and fail closed
when it expires so the finding re-activates.

### Getting -test wrong for your threat model

Wrong: forgetting that test-only dependencies are skipped by default when your
threat model includes test tooling, or conversely scanning tests and blocking a
release on a test-only advisory that never ships.

Fix: choose `-test` deliberately based on what your artifact actually deploys.

### Not pinning the scanner or recording the DB version

Wrong: an unpinned govulncheck and no record of `db_last_modified`, making CI
results non-reproducible and the audit trail incomplete.

Fix: pin the tool version and record the scanner and database versions from
`config` alongside the verdict.

Next: [01-parse-govulncheck-stream.md](01-parse-govulncheck-stream.md)
