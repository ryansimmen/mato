package ui

import (
	"bytes"
	"errors"
	"testing"
)

func TestTextWriter_WritesOutput(t *testing.T) {
	var buf bytes.Buffer

	tw := NewTextWriter(&buf)
	tw.Print("hello")
	tw.Printf(" %s", "world")
	tw.Println("!")

	if err := tw.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
	if got := buf.String(); got != "hello world!\n" {
		t.Fatalf("output = %q, want %q", got, "hello world!\n")
	}
}

func TestTextWriter_StopsAfterFirstError(t *testing.T) {
	wantErr := errors.New("write failed")
	w := &failingWriter{err: wantErr}

	tw := NewTextWriter(w)
	tw.Print("first")
	tw.Printf(" %s", "second")
	tw.Println("third")

	if !errors.Is(tw.Err(), wantErr) {
		t.Fatalf("Err() = %v, want %v", tw.Err(), wantErr)
	}
	if w.calls != 1 {
		t.Fatalf("write calls = %d, want 1", w.calls)
	}
}

type failingWriter struct {
	calls int
	err   error
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.calls++
	return 0, w.err
}
