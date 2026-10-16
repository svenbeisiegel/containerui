package main

import (
	"errors"
	"io"
)

// readLine reads a line typed on a terminal one byte at a time, so nothing
// after the newline is consumed. It drops carriage returns and treats a
// backspace as erasing the previous byte. At EOF it returns what was read,
// or io.EOF if that is nothing.
func readLine(r io.Reader) ([]byte, error) {
	var line []byte
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n > 0 {
			switch b[0] {
			case '\n':
				return line, nil
			case '\r':
			case '\b':
				if len(line) > 0 {
					line = line[:len(line)-1]
				}
			default:
				line = append(line, b[0])
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return line, nil
			}
			return nil, err
		}
	}
}
