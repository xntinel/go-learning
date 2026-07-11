package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	curriculumDir = "go-challenges"
	solutionsDir  = "go-solutions"
	readmePath    = "README.md"
	markerStart   = "<!-- PROGRESS:START -->"
	markerEnd     = "<!-- PROGRESS:END -->"
)

var excludeTop = map[string]bool{
	"docs":                      true,
	"cmd":                       true,
	"go.md":                     true,
	"go-concepts.md":            true,
	"AGENT-ORDER-go-quality.md": true,
	"QUALITY-PROGRESS.md":       true,
	"VERIFICATION-REPORT.md":    true,
}

type status int

const (
	unsolved status = iota
	attempted
	solved
)

type exercise struct {
	relPath string
	status  status
}

func main() {
	var removed []string
	for _, root := range []string{solutionsDir, "tools"} {
		r, err := cleanBinaries(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, "progress: clean binaries:", err)
			os.Exit(2)
		}
		removed = append(removed, r...)
	}
	if len(removed) > 0 {
		fmt.Printf("progress: removed %d stray binary(ies)\n", len(removed))
	}

	exercises, err := enumerateCurriculum()
	if err != nil {
		fmt.Fprintln(os.Stderr, "progress: enumerate curriculum:", err)
		os.Exit(2)
	}

	for i := range exercises {
		exercises[i].status = classify(filepath.Join(solutionsDir, exercises[i].relPath))
	}

	changed, err := updateReadme(exercises)
	if err != nil {
		fmt.Fprintln(os.Stderr, "progress: update readme:", err)
		os.Exit(2)
	}
	if !changed {
		fmt.Println("progress: no changes")
		return
	}

	if err := commitReadme(); err != nil {
		fmt.Fprintln(os.Stderr, "progress: git commit:", err)
		os.Exit(2)
	}
	fmt.Println("progress: README.md updated and committed — run `git push` again")
	os.Exit(1)
}

func commitReadme() error {
	if out, err := exec.Command("git", "add", readmePath).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	if out, err := exec.Command("git", "commit", "-m", "chore: update progress").CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	return nil
}

// cleanBinaries elimina binarios compilados sueltos: archivos ejecutables sin
// extension que quedaron junto a un .go (residuo de un `go build` manual sin -o).
func cleanBinaries(root string) ([]string, error) {
	var removed []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != "" {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Mode()&0o111 == 0 {
			return nil
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			return nil
		}
		hasGo := false
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".go" {
				hasGo = true
				break
			}
		}
		if !hasGo {
			return nil
		}
		if err := os.Remove(path); err == nil {
			removed = append(removed, path)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return removed, nil
	}
	return removed, err
}

func enumerateCurriculum() ([]exercise, error) {
	var out []exercise
	err := filepath.WalkDir(curriculumDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(curriculumDir, path)
		if rel == "." {
			return nil
		}
		top := strings.SplitN(rel, string(os.PathSeparator), 2)[0]
		if strings.HasPrefix(top, ".") || excludeTop[top] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".md" || filepath.Base(path) == "00-concepts.md" {
			return nil
		}
		out = append(out, exercise{relPath: strings.TrimSuffix(rel, ".md")})
		return nil
	})
	return out, err
}

func classify(dir string) status {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return unsolved
	}
	if !hasSignificantCode(dir) {
		return unsolved
	}
	// go vet type-checks main and non-main packages alike and never writes
	// a binary, so it works uniformly without needing -o or a main func.
	cmd := exec.Command("go", "vet", "./...")
	cmd.Dir = dir
	if _, err := cmd.CombinedOutput(); err != nil {
		return attempted
	}
	return solved
}

func hasSignificantCode(dir string) bool {
	found := false
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(data), "\n") {
			l := strings.TrimSpace(line)
			if l == "" || strings.HasPrefix(l, "//") || strings.HasPrefix(l, "package ") {
				continue
			}
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

type treeNode struct {
	name     string
	children []*treeNode
	childIdx map[string]int
}

func buildTree(paths []string) *treeNode {
	root := &treeNode{childIdx: map[string]int{}}
	for _, p := range paths {
		cur := root
		for _, seg := range strings.Split(p, "/") {
			name := humanizeSegment(seg)
			idx, ok := cur.childIdx[name]
			if !ok {
				idx = len(cur.children)
				cur.childIdx[name] = idx
				cur.children = append(cur.children, &treeNode{name: name, childIdx: map[string]int{}})
			}
			cur = cur.children[idx]
		}
	}
	return root
}

func renderTree(b *strings.Builder, n *treeNode, prefix string) {
	for i, c := range n.children {
		last := i == len(n.children)-1
		connector, childPrefix := "├── ", prefix+"│   "
		if last {
			connector, childPrefix = "└── ", prefix+"    "
		}
		fmt.Fprintf(b, "%s%s%s\n", prefix, connector, c.name)
		renderTree(b, c, childPrefix)
	}
}

// humanizeSegment convierte "01-check-package-and-error-contract" en
// "Check Package And Error Contract" para mostrarlo en el README.
func humanizeSegment(s string) string {
	if idx := strings.IndexByte(s, '-'); idx > 0 {
		allDigits := true
		for _, c := range s[:idx] {
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			s = s[idx+1:]
		}
	}
	words := strings.Fields(strings.ReplaceAll(s, "-", " "))
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func updateReadme(exercises []exercise) (bool, error) {
	var solvedList []string
	solvedCount, attemptedCount := 0, 0
	for _, e := range exercises {
		switch e.status {
		case solved:
			solvedCount++
			solvedList = append(solvedList, e.relPath)
		case attempted:
			attemptedCount++
		}
	}
	sort.Strings(solvedList)

	pct := 0.0
	if len(exercises) > 0 {
		pct = 100 * float64(solvedCount) / float64(len(exercises))
	}

	var b strings.Builder
	fmt.Fprintln(&b, markerStart)
	fmt.Fprintf(&b, "**Progress: %d / %d exercises solved (%.1f%%)**", solvedCount, len(exercises), pct)
	if attemptedCount > 0 {
		fmt.Fprintf(&b, " — %d attempted, not compiling", attemptedCount)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b)
	if len(solvedList) > 0 {
		fmt.Fprintln(&b, "### Solved exercises")
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "```")
		renderTree(&b, buildTree(solvedList), "")
		fmt.Fprintln(&b, "```")
	}
	fmt.Fprint(&b, markerEnd)
	block := b.String()

	raw, err := os.ReadFile(readmePath)
	if err != nil {
		return false, err
	}
	content := string(raw)

	start := strings.Index(content, markerStart)
	end := strings.Index(content, markerEnd)
	var newContent string
	if start == -1 || end == -1 {
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		newContent = content + "\n" + block + "\n"
	} else {
		newContent = content[:start] + block + content[end+len(markerEnd):]
	}

	if newContent == content {
		return false, nil
	}
	return true, os.WriteFile(readmePath, []byte(newContent), 0o644)
}
