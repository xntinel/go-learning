package main

import (
	"fmt"
	"strings"

	csvscan "github.com/sentinel/go-learning/go-solutions/06-collections-arrays-slices-and-maps/03-slice-expressions-and-sub-slicing/06-csv-line-field-slicing"
)

func main() {
	const data = "aaaa,1111\nbbbb,2222\ncccc,3333\ndddd,4444\neeee,5555\n"
	line := []byte("id,name,region")
	fields := csvscan.SplitFields(nil, line)
	fmt.Printf("split %d fields: ", len(fields))

	for i, f := range fields {
		if i > 0 {
			fmt.Print(" | ")
		}
		fmt.Print(string(f))
	}
	fmt.Println()
	views, _ := csvscan.CollectFirstFields(strings.NewReader(data), false)
	fmt.Print("retained views:")
	for _, f := range views {
		fmt.Printf(" %s", f)
	}
	fmt.Println()

	owned, _ := csvscan.CollectFirstFields(strings.NewReader(data), true)
	fmt.Print("cloned fields: ")
	for _, f := range owned {
		fmt.Printf(" %s", f)
	}
	fmt.Println()
}
