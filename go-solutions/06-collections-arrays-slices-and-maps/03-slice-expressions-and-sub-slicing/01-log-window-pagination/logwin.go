package logwin

import (
	"errors"
	"slices"
)

var ErrInvalidOffset = errors.New("logwin: invalid offset")
var ErrInvalidLimit = errors.New("logwin: invalid limit")

func Window(lines []string, offset, limit int) ([]string, error) {
	if offset < 0 || offset > len(lines) {
		return nil, ErrInvalidOffset
	}
	if limit <= 0 {
		return nil, ErrInvalidLimit
	}
	low := offset
	high := offset + limit
	if high > len(lines) {
		high = len(lines)
	}
	if low == high {
		return []string{}, nil
	}
	return slices.Clone(lines[low:high]), nil
}
