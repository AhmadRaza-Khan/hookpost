package ids

import (
	"strings"
	"testing"
	"time"
)

func TestNewPrefixAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := New("msg")
		if !strings.HasPrefix(id, "msg_") || len(id) != len("msg_")+26 {
			t.Fatalf("bad id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestNewSortsByTime(t *testing.T) {
	a := New("ep")
	time.Sleep(2 * time.Millisecond)
	b := New("ep")
	if a >= b {
		t.Fatalf("want %q < %q", a, b)
	}
}
