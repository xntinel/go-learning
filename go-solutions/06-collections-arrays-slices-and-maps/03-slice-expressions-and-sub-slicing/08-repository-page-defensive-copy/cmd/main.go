package main

import (
	"fmt"

	repo "github.com/sentinel/go-learning/go-solutions/06-collections-arrays-slices-and-maps/03-slice-expressions-and-sub-slicing/08-repository-page-defensive-copy"
)

func main() {
	seed := []repo.Item{
		{ID: 0, Name: "item-0"},
		{ID: 1, Name: "item-1"},
		{ID: 2, Name: "item-2"},
		{ID: 3, Name: "item-3"},
	}
	r := repo.New(seed)

	page := r.Page(1, 2)
	fmt.Printf("page: %v (len=%d cap=%d)\n", page, len(page), cap(page))

	page[0].Name = "HACKED"
	page = append(page, repo.Item{ID: 99, Name: "injected"})

	after := r.Page(1, 2)
	fmt.Printf("cache after handler abuse: %v (len=%d)\n", after, r.Len())
}
