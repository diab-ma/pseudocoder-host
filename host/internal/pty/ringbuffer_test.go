package pty

import "testing"

func TestRingBufferWriteOrder(t *testing.T) {
	rb := NewRingBuffer(3)

	rb.Write("A")
	rb.Write("B")
	got := rb.Lines()
	if len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Fatalf("unexpected lines after 2 writes: %#v", got)
	}

	rb.Write("C")
	got = rb.Lines()
	if len(got) != 3 || got[0] != "A" || got[1] != "B" || got[2] != "C" {
		t.Fatalf("unexpected lines after 3 writes: %#v", got)
	}

	rb.Write("D")
	got = rb.Lines()
	if len(got) != 3 || got[0] != "B" || got[1] != "C" || got[2] != "D" {
		t.Fatalf("unexpected lines after overwrite: %#v", got)
	}
}

func TestRingBufferClear(t *testing.T) {
	rb := NewRingBuffer(2)
	rb.Write("A")
	rb.Write("B")
	rb.Clear()

	if rb.Size() != 0 {
		t.Fatalf("expected size 0 after clear, got %d", rb.Size())
	}
	if got := rb.Lines(); len(got) != 0 {
		t.Fatalf("expected no lines after clear, got %#v", got)
	}

	rb.Write("C")
	got := rb.Lines()
	if len(got) != 1 || got[0] != "C" {
		t.Fatalf("unexpected lines after clear/write: %#v", got)
	}
}
