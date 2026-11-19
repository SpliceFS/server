package digestcache

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"unicode/utf8"
)

// suffix is written out here, so that the tests check the design and not the code.
const suffix = ".splicefs_state"

// hashed is the name rule 4 makes of name, worked out here from the design and not from the
// code under test.
func hashed(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "%_" + hex.EncodeToString(sum[:])
}

func TestEscape(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		path string
		want string
	}{
		"plain names":                    {"photos/2024/a.jpg", "photos/2024/a.jpg"},
		"a percent sign":                 {"100%/a%b", "100%25/a%25b"},
		"an escape written out":          {"%25/%C3", "%2525/%25C3"},
		"a byte of invalid UTF-8":        {"caf\xe9/a", "caf%E9/a"},
		"valid UTF-8 stays":              {"\u00e9/\u65e5", "\u00e9/\u65e5"},
		"a name that ends in the suffix": {"a.splicefs_state/b.splicefs_state", "a%2Esplicefs_state/b%2Esplicefs_state"},
		"a long name is not hashed here": {strings.Repeat("a", 300), strings.Repeat("a", 300)},
		"a folder":                       {"a/", "a/"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := Escape(tt.path); got != tt.want {
				t.Errorf("Escape(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestEntryPath(t *testing.T) {
	t.Parallel()
	// 255 is the longest name a file system takes. The suffix counts with the escaped name.
	fits := strings.Repeat("a", 255-len(suffix))
	tooLong := fits + "a"
	invalidBytes := func(count int) string { return strings.Repeat("\xff", count) }
	tests := map[string]struct {
		path string
		want string
	}{
		"a file":                       {"a.jpg", "a.jpg.splicefs_state"},
		"a file in folders":            {"photos/2024/a.jpg", "photos/2024/a.jpg.splicefs_state"},
		"a percent sign (rule 1)":      {"100%", "100%25.splicefs_state"},
		"an escape written out":        {"%25", "%2525.splicefs_state"},
		"invalid UTF-8 (rule 2)":       {"caf\xe9", "caf%E9.splicefs_state"},
		"hex digits in upper case":     {"\xab\xcd", "%AB%CD.splicefs_state"},
		"an overlong form":             {"\xc0\xaf", "%C0%AF.splicefs_state"},
		"an encoded surrogate":         {"\xed\xa0\x80", "%ED%A0%80.splicefs_state"},
		"a cut character":              {"a\xc3b", "a%C3b.splicefs_state"},
		"valid UTF-8 stays":            {"\u00e9/\u65e5/\U0001F600", "\u00e9/\u65e5/\U0001F600" + suffix},
		"the replacement character":    {"\ufffd", "\ufffd" + suffix},
		"named like an entry (rule 3)": {"a.splicefs_state", "a%2Esplicefs_state.splicefs_state"},
		"the last dot only": {
			"a.splicefs_state.splicefs_state",
			"a.splicefs_state%2Esplicefs_state.splicefs_state",
		},
		"a folder named like one":     {"a.splicefs_state/b", "a%2Esplicefs_state/b.splicefs_state"},
		"only the suffix":             {".splicefs_state", "%2Esplicefs_state.splicefs_state"},
		"almost the suffix":           {"a.splicefs_stat/splicefs_state", "a.splicefs_stat/splicefs_state.splicefs_state"},
		"the longest name that fits":  {fits, fits + suffix},
		"one byte too long (rule 4)":  {tooLong, hashed(tooLong) + suffix},
		"a long folder name":          {"x/" + tooLong + "/y", "x/" + hashed(tooLong) + "/y.splicefs_state"},
		"escapes that just fit":       {invalidBytes(80), strings.Repeat("%FF", 80) + suffix},
		"escapes add up":              {invalidBytes(81), hashed(invalidBytes(81)) + suffix},
		"percent signs add up":        {strings.Repeat("%", 100), hashed(strings.Repeat("%", 100)) + suffix},
		"bytes count, not characters": {strings.Repeat("\u00e9", 128), hashed(strings.Repeat("\u00e9", 128)) + suffix},
		"a name of 255 bytes":         {strings.Repeat("a", 255), hashed(strings.Repeat("a", 255)) + suffix},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := EntryPath(tt.path); got != tt.want {
				t.Errorf("EntryPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// pathSeeds are the starting inputs of the fuzz targets: names that sit next to each other in
// the rules, and names around the length limit.
func pathSeeds() []string {
	return []string{
		"a", "a/b", "a.splicefs_state", "a%2Esplicefs_state", "a.splicefs_state/b", "a%2Esplicefs_state/b",
		"%", "%25", "%2525", "\xff", "%FF", "%ff", "%C3", "\xc3", "\u00e9", "%_", "%_" + strings.Repeat("0", 64),
		"", "/", "a//b", "a/b/", ".splicefs_state", ".splicefs_state.splicefs_state",
		strings.Repeat("a", 240), strings.Repeat("a", 241), strings.Repeat("%", 100), strings.Repeat("\xff", 81),
	}
}

// No two different paths may share an entry, and no entry may be the folder of another: a file
// system cannot hold a file and a folder of one name.
func FuzzEntryPathInjective(f *testing.F) {
	for _, first := range pathSeeds() {
		for _, second := range pathSeeds() {
			f.Add(first, second)
		}
	}
	f.Fuzz(func(t *testing.T, first, second string) {
		entry, other := EntryPath(first), EntryPath(second)
		if first != second && entry == other {
			t.Fatalf("EntryPath(%q) and EntryPath(%q) are both %q", first, second, entry)
		}
		if strings.HasPrefix(other, entry+"/") {
			t.Fatalf("EntryPath(%q) = %q is a folder of EntryPath(%q) = %q", first, entry, second, other)
		}
	})
}

// A file system takes at most 255 bytes in a name, and macOS takes only names that are valid
// UTF-8, whatever the name in the share was.
func FuzzEntryPathNamesFit(f *testing.F) {
	for _, seed := range pathSeeds() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, path string) {
		got := EntryPath(path)
		if !strings.HasSuffix(got, suffix) || strings.Count(got, "/") != strings.Count(path, "/") {
			t.Fatalf("EntryPath(%q) = %q", path, got)
		}
		for name := range strings.SplitSeq(got, "/") {
			if len(name) > 255 || !utf8.ValidString(name) {
				t.Fatalf("EntryPath(%q) has the name %q", path, name)
			}
		}
	})
}
