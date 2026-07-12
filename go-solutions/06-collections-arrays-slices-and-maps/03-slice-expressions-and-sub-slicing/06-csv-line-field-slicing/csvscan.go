package csvscan

import (
	"bufio"
	"bytes"
	"io"
)

func SplitFields(dst [][]byte, line []byte) [][]byte {
	dst = dst[:0]
	start := 0
	for {
		i := bytes.IndexByte(line[start:], ',')
		if i < 0 {
			return append(dst, line[start:])
		}
		dst = append(dst, line[start:start+i])
		start += i + 1
	}
}

func CollectFirstFields(r io.Reader, clone bool) ([][]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 16), 16)
	var out [][]byte
	for sc.Scan() {
		line := sc.Bytes()
		field := line
		if i := bytes.IndexByte(line, ','); i >= 0 {
			field = line[:i]
		}
		if clone {
			field = bytes.Clone(field)
		}
		out = append(out, field)
	}
	return out, sc.Err()
}
