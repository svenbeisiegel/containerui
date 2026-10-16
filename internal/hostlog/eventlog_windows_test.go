package hostlog

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestEventLogRead reads the real System channel of this machine.
func TestEventLogRead(t *testing.T) {
	src := New()
	if err := src.Available(); err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	q := Query{Selector: "System", Since: "1h"}
	if err := Validate(ctx, src, q); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := src.Read(ctx, q, &buf); err != nil {
		t.Fatal(err)
	}

	// Following starts with the tail and ends with the context.
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	buf.Reset()
	err := src.Read(ctx, Query{Selector: "System", Tail: 3, Follow: true}, &buf)
	if err != context.DeadlineExceeded {
		t.Fatalf("follow ended with %v", err)
	}
	if n := bytes.Count(buf.Bytes(), []byte("\n")); n == 0 {
		t.Errorf("no tail lines")
	}
}
