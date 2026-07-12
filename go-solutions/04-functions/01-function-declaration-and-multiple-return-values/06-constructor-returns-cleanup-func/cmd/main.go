package main

import (
	"fmt"
	"os"

	spooler "github.com/sentinel/go-learning/go-solutions/04-functions/01-function-declaration-and-multiple-return-values/06-constructor-returns-cleanup-func"
)

func main() {
	s, cleanup, err := spooler.Open(spooler.Config{})
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			fmt.Println("cleanup error:", err)
		}
	}()
	path := s.Path()
	if _, err := s.Write([]byte("job-1\n")); err != nil {
		panic(err)
	}
	fmt.Printf("spooled to a temp file, exists=%t\n", fileExists(path))
	_ = path
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
