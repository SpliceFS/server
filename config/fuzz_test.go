package config

import (
	"math"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func FuzzRateUnmarshalText(f *testing.F) {
	for _, s := range []string{
		"10/m", "1/s", "0/h", "600/h", "007/m", "2147483647/s", "2147483648/s",
		"", "10", "/m", "10/", "x/m", "-1/m", "+1/m", "1.5/m", "10/d", "10/min", "10/m/x",
		"99999999999/s", "10/M", " 10/m", "10/m ", "1e3/s", "0x10/s",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		r := Rate{Count: 7, Per: time.Hour}
		err := r.UnmarshalText([]byte(in))
		if err != nil {
			// A failed parse must leave the target alone so callers never see a half value.
			if r != (Rate{Count: 7, Per: time.Hour}) {
				t.Fatalf("value changed to %+v on error for %q", r, in)
			}
			return
		}
		// A count is never negative and fits an int on every platform, so limiter math
		// cannot wrap around.
		if r.Count < 0 || r.Count > math.MaxInt32 {
			t.Fatalf("%q parsed to count %d", in, r.Count)
		}
		if r.Per != time.Second && r.Per != time.Minute && r.Per != time.Hour {
			t.Fatalf("%q parsed to period %v", in, r.Per)
		}
		// What a rate writes must read back as the same rate, or saved files would drift.
		var back Rate
		if err := back.UnmarshalText([]byte(r.String())); err != nil || back != r {
			t.Fatalf("round trip of %q via %q gave %+v, %v", in, r.String(), back, err)
		}
	})
}

func FuzzParseProxy(f *testing.F) {
	for _, s := range []string{
		"192.0.2.1", "192.0.2.0/24", "2001:db8::1", "2001:db8::/32", "::1", "0.0.0.0/0",
		"", "/", "1.2.3.4/33", "1.2.3.4/-1", "1.2.3/24", "256.0.0.1", "fe80::1%eth0",
		"fe80::1%eth0/64", "1.2.3.4/", "::ffff:1.2.3.4", "::ffff:1.2.3.4/120", " 1.2.3.4",
		"1.2.3.4/24/8", "01.2.3.4",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, entry string) {
		prefix, err := parseProxy(entry)
		if err != nil {
			if prefix.IsValid() {
				t.Fatalf("parseProxy(%q) failed but returned %v", entry, prefix)
			}
			return
		}
		// A trusted proxy list decides whose forwarded headers are believed, so an entry
		// must always become a real prefix, and a bare address only its own address.
		if !prefix.IsValid() {
			t.Fatalf("parseProxy(%q) = invalid prefix", entry)
		}
		if !strings.Contains(entry, "/") && prefix.Bits() != prefix.Addr().BitLen() {
			t.Fatalf("parseProxy(%q) = %v covers more than one address", entry, prefix)
		}
		if _, err := netip.ParseAddr(prefix.Addr().String()); err != nil {
			t.Fatalf("parseProxy(%q) address %v does not parse back", entry, prefix.Addr())
		}
	})
}

func FuzzNormalizeBasePath(f *testing.F) {
	for _, s := range []string{
		"/", "/api", "/api/", "/api//", "///", "", "api", " /api", "/a/b/", "/%2e%2e", "/a b",
		"\x00", "/\x00",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got, err := normalizeBasePath(in)
		if err != nil {
			// A base path without a leading slash would be joined to routes wrongly.
			if strings.HasPrefix(in, "/") {
				t.Fatalf("normalizeBasePath(%q) refused a path that starts with a slash", in)
			}
			return
		}
		// The routes are added after the base path, so it must not end with a slash.
		if got != "" && (!strings.HasPrefix(got, "/") || strings.HasSuffix(got, "/")) {
			t.Fatalf("normalizeBasePath(%q) = %q", in, got)
		}
		// Normalizing twice must change nothing, or reloads could drift.
		if again, err := normalizeBasePath(got); got != "" && (err != nil || again != got) {
			t.Fatalf("normalizeBasePath(%q) = %q, then %q, %v", in, got, again, err)
		}
	})
}
