package share

import "testing"

func TestLRU(t *testing.T) {
	t.Parallel()
	cache := newLRU(2)
	keyOf := func(name string) digestKey { return digestKey{path: name} }
	cache.put(keyOf("a"), "1")
	cache.put(keyOf("b"), "2")
	if digest, ok := cache.get(keyOf("a")); !ok || digest != "1" { // a is now most recent
		t.Fatalf("get(a) = %q, %v", digest, ok)
	}
	cache.put(keyOf("c"), "3") // evicts b
	if _, ok := cache.get(keyOf("b")); ok {
		t.Error("b was not evicted")
	}
	cache.put(keyOf("a"), "4") // update in place
	if digest, _ := cache.get(keyOf("a")); digest != "4" {
		t.Errorf("get(a) = %q, want 4", digest)
	}
	if cache.order.Len() != 2 || len(cache.entries) != 2 {
		t.Errorf("size = %d/%d, want 2", cache.order.Len(), len(cache.entries))
	}
	if _, ok := cache.get(keyOf("zzz")); ok {
		t.Error("unknown key found")
	}
}

// TestLRUReplacesAnElementThatIsNotAnEntry covers a list that was changed from outside:
// the cache treats such an element as a miss and never panics on it.
func TestLRUReplacesAnElementThatIsNotAnEntry(t *testing.T) {
	t.Parallel()
	cache := newLRU(1)
	foreign := digestKey{path: "foreign"}
	cache.entries[foreign] = cache.order.PushFront("not an entry")
	if digest, ok := cache.get(foreign); ok {
		t.Errorf("get(foreign) = %q, want a miss", digest)
	}
	cache.put(foreign, "1")
	if digest, ok := cache.get(foreign); !ok || digest != "1" {
		t.Errorf("get(foreign) after put = %q, %v", digest, ok)
	}
	// The limit is 1, so this put evicts the oldest element, which is the foreign one.
	cache.put(digestKey{path: "other"}, "2")
	if cache.order.Len() != 1 {
		t.Errorf("order holds %d elements, want 1", cache.order.Len())
	}
}
