export const meta = {
  name: 'go-quality-engine',
  description: 'Generator -> adversarial verifier -> surgical repair pipeline for Go lessons (methodology §9-§11)',
  phases: [
    { title: 'Generate' },
    { title: 'Verify+Repair' },
  ],
}

// args = {
//   chapters: [ { name, lessons: [ { path, mode: "gate"|"bar", generate: bool } ] } ]
// }
// mode "gate": full §14.1 gate must print PASS.
// mode "bar":  offline capstone bar — gofmt+vet on extractable + §15; build/test deferred.

const ORDER = 'challenges/go/.worker-order-wave.md'
const STANDARD = 'challenges/go/AGENT-ORDER-go-quality.md'
const METHOD = 'challenges/CURRICULUM-METHODOLOGY.md'

const VERIFY_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['ok', 'gateResult', 'issues'],
  properties: {
    ok: { type: 'boolean', description: 'true only if the lesson meets the bar: gate PASS (or clean OFFLINE-BAR) AND no blocker/major rubric issue remains' },
    gateResult: { type: 'string', enum: ['PASS', 'FAIL', 'OFFLINE-BAR', 'NOGO'] },
    gateDetail: { type: 'string', description: 'raw gate output tail or why offline' },
    issues: {
      type: 'array',
      items: {
        type: 'object',
        additionalProperties: false,
        required: ['criterion', 'severity', 'evidence', 'fix'],
        properties: {
          criterion: { type: 'string', description: 'A/B/C/D/E (§14.1), §15, form-rule, or link' },
          severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
          evidence: { type: 'string', description: 'concrete quote / line / gate output proving the defect' },
          fix: { type: 'string', description: 'the surgical change that resolves it' },
        },
      },
    },
  },
}

const REPAIR_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['gateAfter', 'changed'],
  properties: {
    gateAfter: { type: 'string', enum: ['PASS', 'FAIL', 'OFFLINE-BAR', 'NOGO'] },
    changed: { type: 'boolean' },
    note: { type: 'string' },
  },
}

function safe(p) {
  return p.replace(/[^A-Za-z0-9]/g, '_')
}

function gateCmd(path) {
  return `export GOTOOLCHAIN=auto && python3 challenges/tools/go_gate_append.py ${path} challenges/go/.verify/engine/${safe(path)}`
}

function generatePrompt(item) {
  return `Work from repo root. Read ${ORDER} and ${STANDARD} in full, then rewrite this ONE lesson to the canonical standard (§5 shape, real *_test.go + Example//Output + cmd/demo, TAB-indented Go, no emojis, English): ${item.path}
Research authoritative sources (go.dev, pkg.go.dev) before writing; never invent APIs.
${item.mode === 'gate'
    ? `Then self-gate to PASS:\n${gateCmd(item.path)}\nFix until it prints PASS.`
    : `This lesson cannot build offline (cgo / //go:embed assets / external module). Apply the CAPSTONE BAR: full §5 rewrite with realistic substantial code, validated by §15 + gofmt + go vet on extractable code. Run gofmt/vet on what is extractable.`}
Return one line: the gate result.`
}

function verifyPrompt(item) {
  return `You are an ADVERSARIAL, INDEPENDENT verifier (methodology §10.2). Assume the lesson has a defect and try to find it. Be blind to any generator's reasoning; derive the pass criteria yourself from the rubric, then judge against EVIDENCE (cite the exact line/quote/gate output for every verdict).

Lesson: ${item.path}

Step 1 — GROUND TRUTH. Run the real gate yourself via Bash and read its full output:
${gateCmd(item.path)}
${item.mode === 'gate'
    ? `For mode "gate" the lesson PASSES ground truth ONLY if this prints exactly "PASS". A FAIL/NOGO is a blocker.`
    : `This lesson is mode "bar" (cannot build offline). The gate may FAIL/NOGO on build/test — that alone is NOT a blocker. Instead extract the Go that IS standalone and run gofmt -l / go vet on it; gofmt diffs or vet findings on extractable code ARE blockers. Record gateResult "OFFLINE-BAR".`}

Step 2 — RUBRIC §14.1 A-E + §15 (read ${STANDARD} and ${METHOD} §14.1/§15 first). Check, citing evidence:
A internal consistency (prose <-> code <-> output all match; intro promise implemented; no leftovers).
B correctness/realism (no stdlib reinvention; every stdlib signature real; real cmd/demo; real tests asserting contracts with errors.Is + %w; httptest/fstest where relevant; no network in tests).
C hygiene (no dead imports / var _ = X / identical branches / dead code; gofmt/vet/build clean).
D navigation (file is NN-name/NN-name.md; every link resolves — verify "What's Next" with: test -f the relative target; Resources 3-5 real on-topic).
E extensions solvable with the code shown.
§15 self-consistency: every "the code does X" sentence matches the code; every Expected-output block matches a real run; no fabricated output/line numbers.
Form rules: no emojis/decorative symbols, English, no Difficulty/Prerequisites/Learning Objectives/<details>, keeps "# N. Title".

Set ok=true ONLY if ground truth passes (PASS, or clean OFFLINE-BAR) AND there is no blocker or major issue. List every issue with severity, evidence, and the surgical fix. Do not edit anything — you only judge.`
}

function repairPrompt(item, issues) {
  const list = issues.map((x, i) => `${i + 1}. [${x.severity}] ${x.criterion}: ${x.evidence} -> FIX: ${x.fix}`).join('\n')
  return `Work from repo root. SURGICAL repair of ONE lesson (methodology §11): apply ONLY targeted edits that resolve the listed issues — do NOT rewrite the whole file. Read ${STANDARD} for the standard. Keep TAB-indented Go, no emojis, English.

Lesson: ${item.path}

Issues to fix:
${list}

After editing, re-run the gate and report its result:
${gateCmd(item.path)}
${item.mode === 'bar' ? 'For mode "bar" the build/test may fail offline; ensure gofmt -l and go vet are clean on extractable code.' : 'It must print PASS.'}
Return the gate result after your fix.`
}

async function processLesson(item) {
  const phase = item.chapter
  const label = item.path.split('/').slice(-1)[0]

  if (item.generate) {
    await agent(generatePrompt(item), { phase: 'Generate', label: `gen:${label}`, model: 'sonnet', agentType: 'general-purpose' })
  }

  let last = null
  for (let round = 0; round <= 2; round++) {
    const v = await agent(verifyPrompt(item), { phase: 'Verify+Repair', label: `verify:${label}#${round}`, model: 'sonnet', schema: VERIFY_SCHEMA, agentType: 'general-purpose' })
    last = v
    if (!v || v.ok) {
      return { path: item.path, chapter: item.chapter, ok: !!(v && v.ok), rounds: round, gate: v && v.gateResult, openIssues: [] }
    }
    if (round === 2) break
    const blockers = v.issues.filter(x => x.severity !== 'minor')
    if (blockers.length === 0) {
      // only minor issues remain; accept
      return { path: item.path, chapter: item.chapter, ok: true, rounds: round, gate: v.gateResult, openIssues: v.issues }
    }
    await agent(repairPrompt(item, blockers), { phase: 'Verify+Repair', label: `repair:${label}#${round}`, model: 'sonnet', schema: REPAIR_SCHEMA, agentType: 'general-purpose' })
  }
  return { path: item.path, chapter: item.chapter, ok: false, rounds: 2, gate: last && last.gateResult, openIssues: (last && last.issues) || [] }
}

let parsed = args
if (typeof parsed === 'string') {
  try { parsed = JSON.parse(parsed) } catch (e) { parsed = null }
}
log(`args typeof=${typeof args}; parsed has chapters=${!!(parsed && parsed.chapters)}; raw=${JSON.stringify(args).slice(0, 200)}`)
const chapters = (parsed && parsed.chapters) || []
const items = []
for (const ch of chapters) {
  for (const l of ch.lessons) {
    items.push({ chapter: ch.name, path: l.path, mode: l.mode || 'gate', generate: !!l.generate })
  }
}

if (items.length === 0) {
  return { debug: true, argsType: typeof args, hasChapters: !!(parsed && parsed.chapters), rawArgs: JSON.stringify(args) }
}

log(`Engine over ${items.length} lessons across ${chapters.length} chapters`)
phase('Verify+Repair')
const results = await pipeline(items, processLesson)

const ok = results.filter(r => r && r.ok)
const failed = results.filter(r => r && !r.ok)
log(`DONE: ${ok.length}/${results.length} OK, ${failed.length} still failing`)

return {
  total: results.length,
  ok: ok.length,
  failed: failed.map(f => ({ path: f.path, gate: f.gate, openIssues: f.openIssues })),
  passed: ok.map(o => ({ path: o.path, rounds: o.rounds, gate: o.gate })),
}
