package shellsession

import "testing"

func TestScrollback_OffsetsAndSince(t *testing.T) {
	sb := newScrollback(1024)
	if got := sb.append([]byte("hello")); got != 5 {
		t.Fatalf("end after append = %d, want 5", got)
	}
	sb.append([]byte(" world"))

	data, from, to := sb.since(0)
	if string(data) != "hello world" || from != 0 || to != 11 {
		t.Fatalf("since(0) = %q from=%d to=%d", data, from, to)
	}

	data, from, to = sb.since(6)
	if string(data) != "world" || from != 6 || to != 11 {
		t.Fatalf("since(6) = %q from=%d to=%d", data, from, to)
	}

	data, from, to = sb.since(11)
	if len(data) != 0 || from != 11 || to != 11 {
		t.Fatalf("since(end) = %q from=%d to=%d", data, from, to)
	}
}

func TestScrollback_EvictionAdvancesStart(t *testing.T) {
	sb := newScrollback(8)
	sb.append([]byte("abcdef")) // end=6, retained "abcdef"
	sb.append([]byte("ghijkl")) // end=12, over cap 8 → drop 4, retain last 8 "efghijkl"
	if start := sb.startOffset(); start != 4 {
		t.Fatalf("start = %d, want 4 after eviction", start)
	}
	if end := sb.endOffset(); end != 12 {
		t.Fatalf("end = %d, want 12", end)
	}

	data, from, to := sb.since(0)
	if string(data) != "efghijkl" || from != 4 || to != 12 {
		t.Fatalf("since(evicted) = %q from=%d to=%d", data, from, to)
	}
}

func TestScrollback_Tail(t *testing.T) {
	sb := newScrollback(1024)
	sb.append([]byte("0123456789"))
	data, from, to := sb.tail(4)
	if string(data) != "6789" || from != 6 || to != 10 {
		t.Fatalf("tail(4) = %q from=%d to=%d", data, from, to)
	}
	data, _, _ = sb.tail(100)
	if string(data) != "0123456789" {
		t.Fatalf("tail(over) = %q, want whole buffer", data)
	}
}
