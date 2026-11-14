package share

import "testing"

func TestLRU(t *testing.T) {
	t.Parallel()
	cache := newLRU(2)
	keyOf := func(name string) digestKey { return digestKey{path: name} }
	pairOf := func(value string) FileDigests { return FileDigests{Head: value, Tail: "tail" + value} }
	cache.put(keyOf("a"), pairOf("1"))
	cache.put(keyOf("b"), pairOf("2"))
	if digests, ok := cache.get(keyOf("a")); !ok || digests != pairOf("1") { // a is now most recent
		t.Fatalf("get(a) = %+v, %v", digests, ok)
	}
	cache.put(keyOf("c"), pairOf("3")) // evicts b
	if _, ok := cache.get(keyOf("b")); ok {
		t.Error("b was not evicted")
	}
	cache.put(keyOf("a"), pairOf("4")) // update in place
	if digests, _ := cache.get(keyOf("a")); digests != pairOf("4") {
		t.Errorf("get(a) = %+v, want 4", digests)
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
	if digests, ok := cache.get(foreign); ok {
		t.Errorf("get(foreign) = %+v, want a miss", digests)
	}
	cache.put(foreign, FileDigests{Head: "1", Tail: "1"})
	if digests, ok := cache.get(foreign); !ok || digests.Head != "1" {
		t.Errorf("get(foreign) after put = %+v, %v", digests, ok)
	}
	// The limit is 1, so this put evicts the oldest element, which is the foreign one.
	cache.put(digestKey{path: "other"}, FileDigests{Head: "2", Tail: "2"})
	if cache.order.Len() != 1 {
		t.Errorf("order holds %d elements, want 1", cache.order.Len())
	}
}
