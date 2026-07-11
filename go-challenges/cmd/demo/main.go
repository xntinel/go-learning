package main

import (
	"fmt"

	"example.com/monorepoguard"
)

func main() {
	pkgs := []string{
		"example.com/app",
		"example.com/app/internal/store",
		"example.com/app/generated",
		"example.com/app/node_modules/leftpad",
	}
	ignored := []string{"./generated", "node_modules"}

	kept := monorepoguard.FilterIgnored(pkgs, "example.com/app", ignored)

	fmt.Printf("packages before ignore: %d\n", len(pkgs))
	fmt.Printf("packages after ignore:  %d\n", len(kept))
	for _, p := range kept {
		fmt.Printf("  build: %s\n", p)
	}
}
