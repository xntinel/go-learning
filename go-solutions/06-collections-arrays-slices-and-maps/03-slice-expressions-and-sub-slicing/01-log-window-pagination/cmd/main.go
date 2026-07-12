package main

import (
	"fmt"

	logwin "github.com/sentinel/go-learning/go-solutions/06-collections-arrays-slices-and-maps/03-slice-expressions-and-sub-slicing/01-log-window-pagination"
)

func main() {
	lines := []string{
		"08:00 boot",
		"08:01 ready",
		"08:02 request /health",
		"08:03 request /orders",
		"08:04 shutdown",
	}
	page, err := logwin.Window(lines, 1, 3)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("page (offset 1, limit 3): %d lines\n", len(page))
	for _, l := range page {
		fmt.Println(" ", l)
	}
	page[0] = "REDACTED"
	fmt.Println("source line 1 still:", lines[1])

	tail, _ := logwin.Window(lines, 5, 10)
	fmt.Printf("page at end: %d lines (non-nil: %v)\n", len(tail), tail != nil)
}
