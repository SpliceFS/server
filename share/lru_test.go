package share

import "testing"

func TestLRU(t *testing.T) {
	t.Parallel()
	c := newLRU(2)
	k := func(n string) digestKey { return digestKey{path: n} }
	c.put(k("a"), "1")
	c.put(k("b"), "2")
	if d, ok := c.get(k("a")); !ok || d != "1" { // a is now most recent
		t.Fatalf("get(a) = %q, %v", d, ok)
	}
	c.put(k("c"), "3") // evicts b
	if _, ok := c.get(k("b")); ok {
		t.Error("b was not evicted")
	}
	c.put(k("a"), "4") // update in place
	if d, _ := c.get(k("a")); d != "4" {
		t.Errorf("get(a) = %q, want 4", d)
	}
	if c.order.Len() != 2 || len(c.entries) != 2 {
		t.Errorf("size = %d/%d, want 2", c.order.Len(), len(c.entries))
	}
	if _, ok := c.get(k("zzz")); ok {
		t.Error("unknown key found")
	}
}
