package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadLine(t *testing.T) {
	for _, tc := range []struct {
		in, want, rest string
		err            error
	}{
		{in: "abc\n", want: "abc"},
		{in: "abc\r\n", want: "abc"},
		{in: "ab\bc\n", want: "ac"},
		{in: "\b\bx\n", want: "x"},
		{in: "abc\nnext\n", want: "abc", rest: "next\n"},
		{in: "abc", want: "abc"},
		{in: "", err: io.EOF},
	} {
		r := strings.NewReader(tc.in)
		got, err := readLine(r)
		if !errors.Is(err, tc.err) || string(got) != tc.want {
			t.Errorf("readLine(%q) = %q, %v; want %q, %v", tc.in, got, err, tc.want, tc.err)
		}
		if rest, _ := io.ReadAll(r); string(rest) != tc.rest {
			t.Errorf("readLine(%q) left %q unread, want %q", tc.in, rest, tc.rest)
		}
	}
}
