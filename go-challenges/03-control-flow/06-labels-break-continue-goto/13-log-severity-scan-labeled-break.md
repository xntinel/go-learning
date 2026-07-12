# Exercise 13: Classify each line once, stop the file on CRITICAL

**Nivel: Intermedio** — validacion rapida (un test corto).

An on-call tool scans a log file against an ordered list of severity patterns.
Once a line matches any pattern it is classified — checking the remaining
patterns against the same line is wasted work. But a CRITICAL match is a page:
the scan stops reading the file immediately, because nothing after it matters
until someone responds.

## What you'll build

```text
logscan/                     independent module: example.com/logscan
  go.mod                     go 1.24
  logscan.go                 Pattern, Alert, ScanLines
  logscan_test.go            table test: no match, order-wins, critical stop, empty input
```

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/06-labels-break-continue-goto/13-log-severity-scan-labeled-break
cd go-solutions/03-control-flow/06-labels-break-continue-goto/13-log-severity-scan-labeled-break
go mod edit -go=1.24
```

Create `logscan.go`:

```go
package logscan

import "strings"

// Pattern is one substring rule, checked in the order given. Whichever
// pattern matches FIRST decides how a line is classified; later patterns in
// the list are never checked once one has matched.
type Pattern struct {
	Severity string
	Substr   string
}

// Alert is one classified line.
type Alert struct {
	LineIdx  int
	Severity string
	Pattern  string
}

// ScanLines checks every line against patterns in order. The first pattern
// that matches a line classifies it, and scanning moves on to the NEXT
// LINE immediately (labeled continue) without checking the remaining
// patterns against the same line. A CRITICAL match, however, stops the
// ENTIRE scan (labeled break): no later line is examined at all.
func ScanLines(lines []string, patterns []Pattern) (alerts []Alert, stoppedEarly bool, scannedLines int) {
lines:
	for i, line := range lines {
		scannedLines = i + 1
		for _, p := range patterns {
			if !strings.Contains(line, p.Substr) {
				continue
			}
			alerts = append(alerts, Alert{LineIdx: i, Severity: p.Severity, Pattern: p.Substr})
			if p.Severity == "CRITICAL" {
				stoppedEarly = true
				break lines
			}
			continue lines
		}
	}
	return alerts, stoppedEarly, scannedLines
}
```

### Pattern order decides classification, and the label enforces it

The inner loop's own `continue` (skip a non-matching pattern) is plain and
unlabeled — nothing to reach beyond the current pattern check. The moment a
pattern matches, though, the decision must reach the LINES loop, not just stop
checking patterns: `continue lines` moves straight to the next line, and
`break lines` — only for CRITICAL — leaves both loops so the rest of the file
is never read. Since patterns are checked in the caller's order, a line
matching two different patterns is classified by whichever one appears
FIRST in the list, and the second is never reached.

Create `logscan_test.go`:

```go
package logscan

import "testing"

func defaultPatterns() []Pattern {
	return []Pattern{
		{Severity: "ERROR", Substr: "failed"},
		{Severity: "CRITICAL", Substr: "panic:"},
		{Severity: "WARNING", Substr: "retry"},
	}
}

func TestScanLines(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		lines            []string
		patterns         []Pattern
		wantAlerts       []Alert
		wantStoppedEarly bool
		wantScanned      int
	}{
		"no matches scans every line": {
			lines:       []string{"ok", "ok", "ok"},
			patterns:    defaultPatterns(),
			wantAlerts:  nil,
			wantScanned: 3,
		},
		"warnings and errors do not stop the scan": {
			lines:    []string{"connect failed", "will retry soon", "ok"},
			patterns: defaultPatterns(),
			wantAlerts: []Alert{
				{LineIdx: 0, Severity: "ERROR", Pattern: "failed"},
				{LineIdx: 1, Severity: "WARNING", Pattern: "retry"},
			},
			wantScanned: 3,
		},
		"first matching pattern wins, later patterns on the same line are not checked": {
			lines:    []string{"request failed then panic: recovered", "ok"},
			patterns: defaultPatterns(),
			wantAlerts: []Alert{
				{LineIdx: 0, Severity: "ERROR", Pattern: "failed"},
			},
			wantScanned: 2,
		},
		"a critical match stops the whole scan": {
			lines:    []string{"will retry soon", "panic: nil pointer", "never seen"},
			patterns: defaultPatterns(),
			wantAlerts: []Alert{
				{LineIdx: 0, Severity: "WARNING", Pattern: "retry"},
				{LineIdx: 1, Severity: "CRITICAL", Pattern: "panic:"},
			},
			wantStoppedEarly: true,
			wantScanned:      2,
		},
		"empty input scans zero lines": {
			lines:       nil,
			patterns:    defaultPatterns(),
			wantAlerts:  nil,
			wantScanned: 0,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			alerts, stopped, scanned := ScanLines(tc.lines, tc.patterns)
			if len(alerts) != len(tc.wantAlerts) {
				t.Fatalf("alerts = %v, want %v", alerts, tc.wantAlerts)
			}
			for i := range alerts {
				if alerts[i] != tc.wantAlerts[i] {
					t.Fatalf("alerts[%d] = %v, want %v", i, alerts[i], tc.wantAlerts[i])
				}
			}
			if stopped != tc.wantStoppedEarly || scanned != tc.wantScanned {
				t.Fatalf("stopped,scanned = %v,%d want %v,%d", stopped, scanned, tc.wantStoppedEarly, tc.wantScanned)
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The scanner is correct when each line produces at most one alert (the first
pattern to match, in list order) and a CRITICAL alert halts everything after
it. The "first matching pattern wins" test is the one worth studying: the line
contains substrings for both ERROR and CRITICAL patterns, but because ERROR is
listed first, that line is classified ERROR and the scan continues past it —
proving `continue lines` actually stops checking further patterns rather than
merely happening not to find one.

## Resources

- [strings.Contains](https://pkg.go.dev/strings#Contains) — the substring match used per pattern.
- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — a labeled `continue` targets the named enclosing `for`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-settlement-batch-poison-abort.md](12-settlement-batch-poison-abort.md) | Next: [14-permission-audit-triple-nested-label.md](14-permission-audit-triple-nested-label.md)
