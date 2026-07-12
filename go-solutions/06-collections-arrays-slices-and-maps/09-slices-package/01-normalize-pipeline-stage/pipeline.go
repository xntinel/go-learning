package pipeline

import (
	"cmp"
	"slices"
	"strings"
)

type Record struct {
	Key   string
	Value int
}

func Process(records []Record) []Record {
	out := slices.Clone(records)
	slices.SortFunc(out, func(a, b Record) int {
		if c := cmp.Compare(strings.ToLower(a.Key), strings.ToLower(b.Key)); c != 0 {
			return c
		}
		return cmp.Compare(a.Value, b.Value)
	})
	out = slices.CompactFunc(out, func(a, b Record) bool {
		return strings.EqualFold(a.Key, b.Key)
	})
	slices.Reverse(out)
	return out
}
