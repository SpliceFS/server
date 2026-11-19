package share

import "testing"

// BenchmarkDigestsMemoryHit measures a request for digests the in-memory cache holds, the
// most common request.
func BenchmarkDigestsMemoryHit(b *testing.B) {
	fix := newFixture(b)
	fix.write(b, "d/f", "content", epoch())
	share := fix.open(b, &Options{})
	if _, _, err := share.Digests("d/f"); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, _, err := share.Digests("d/f"); err != nil {
			b.Fatal(err)
		}
	}
}
