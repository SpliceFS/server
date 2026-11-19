package digestcache

import (
	"math"
	"strings"
	"testing"

	"github.com/SpliceFS/server/internal/filestat"
)

const (
	headDigest = "0b1a2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9"
	tailDigest = "f9e8d7c6b5a4938271605f4e3d2c1b0a9f8e7d6c5b4a39281706f5e4d3c2b1a0"
)

// sample is the entry of the design document's example.
func sample() Entry {
	return Entry{
		Path:    "photos/2024/a.jpg",
		Size:    52133,
		MtimeNs: 1763452800123456789,
		CtimeNs: 1763452801000000000,
		BtimeNs: 0,
		Inode:   81273,
		Head:    headDigest,
		Tail:    tailDigest,
	}
}

// sampleText is sample as the design document shows it, field for field.
func sampleText() string {
	return "version: 1\n" +
		"path: photos/2024/a.jpg\n" +
		"size: 52133\n" +
		"mtime_ns: 1763452800123456789\n" +
		"ctime_ns: 1763452801000000000\n" +
		"btime_ns: 0\n" +
		"inode: 81273\n" +
		"head: " + headDigest + "\n" +
		"tail: " + tailDigest + "\n"
}

func TestEncodeWritesTheFieldsOfTheDesign(t *testing.T) {
	t.Parallel()
	if got := string(sample().Encode()); got != sampleText() {
		t.Errorf("Encode =\n%s\nwant\n%s", got, sampleText())
	}
}

func TestEntryRoundTrip(t *testing.T) {
	t.Parallel()
	digits := strings.Repeat("1", 64)
	// Each row changes the example in one way.
	tests := map[string]func(*Entry){
		"the example": func(*Entry) {},
		"zero stamp": func(e *Entry) {
			e.Size, e.MtimeNs, e.CtimeNs, e.BtimeNs, e.Inode = 0, 0, 0, 0, 0
		},
		"largest numbers": func(e *Entry) {
			e.Size, e.MtimeNs, e.CtimeNs, e.BtimeNs = math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64
			e.Inode = math.MaxUint64
		},
		"time before the epoch":  func(e *Entry) { e.MtimeNs = math.MinInt64 },
		"digests of digits only": func(e *Entry) { e.Head, e.Tail = digits, digits },
		"digests that read as a number": func(e *Entry) {
			e.Head, e.Tail = "1e"+strings.Repeat("0", 62), "0"+strings.Repeat("1", 63)
		},
		"escaped bytes":      func(e *Entry) { e.Path = "caf%C3%A9/%FF/100%25" },
		"non-ASCII names":    func(e *Entry) { e.Path = "\u00e9t\u00e9/\u65e5\u672c\u8a9e/\U0001F600" },
		"space and tab":      func(e *Entry) { e.Path = " leading/trailing /in\ttab" },
		"line breaks":        func(e *Entry) { e.Path = "a\nb/c\rd/e\u0085f/g\u2028h/i\u2029j" },
		"control characters": func(e *Entry) { e.Path = "a\x00b/c\x01d/e\x7ff/g\ufeffh" },
		"yaml syntax in names": func(e *Entry) {
			e.Path = "a: b/# c/- d/'e'/\"f\"/{g}/[h]/&i/*j/!k/|l/>m/%25n/@o/`p"
		},
		"names that read as other values": func(e *Entry) { e.Path = "null/true/123/1e3/0x1f/~/---/..." },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entry := sample()
			change(&entry)
			got, err := Decode(entry.Encode())
			if err != nil || got != entry {
				t.Errorf("Decode(Encode(%+v)) = %+v, %v", entry, got, err)
			}
		})
	}
}

func TestDecodeReadsTheFieldsOfTheDesign(t *testing.T) {
	t.Parallel()
	got, err := Decode([]byte(sampleText()))
	if err != nil || got != sample() {
		t.Errorf("Decode = %+v, %v, want %+v", got, err, sample())
	}
}

func TestDecodeRejects(t *testing.T) {
	t.Parallel()
	valid := sampleText()
	replace := func(old, replacement string) string {
		if !strings.Contains(valid, old) {
			t.Fatalf("the example has no %q", old)
		}
		return strings.Replace(valid, old, replacement, 1)
	}
	tests := map[string]string{
		"empty":              "",
		"only a comment":     "# nothing\n",
		"not YAML":           "version: [1\n",
		"a list":             "- version: 1\n",
		"a number":           "1\n",
		"binary":             "\x00\x01\x02\xff",
		"no version":         strings.Replace(valid, "version: 1\n", "", 1),
		"version 0":          replace("version: 1", "version: 0"),
		"version 2":          replace("version: 1", "version: 2"),
		"version a word":     replace("version: 1", "version: one"),
		"unknown field":      valid + "extra: 1\n",
		"duplicate field":    valid + "size: 1\n",
		"a second document":  valid + "---\n" + valid,
		"size negative":      replace("size: 52133", "size: -1"),
		"size a word":        replace("size: 52133", "size: large"),
		"size a fraction":    replace("size: 52133", "size: 1.5"),
		"size too large":     replace("size: 52133", "size: 9223372036854775808"),
		"mtime a word":       replace("mtime_ns: 1763452800123456789", "mtime_ns: now"),
		"ctime too large":    replace("ctime_ns: 1763452801000000000", "ctime_ns: 9223372036854775808"),
		"btime a word":       replace("btime_ns: 0", "btime_ns: never"),
		"inode negative":     replace("inode: 81273", "inode: -1"),
		"inode too large":    replace("inode: 81273", "inode: 18446744073709551616"),
		"head missing":       strings.Replace(valid, "head: "+headDigest+"\n", "", 1),
		"head empty":         replace("head: "+headDigest, "head: ''"),
		"head too short":     replace("head: "+headDigest, "head: "+headDigest[1:]),
		"head too long":      replace("head: "+headDigest, "head: "+headDigest+"0"),
		"head not hex":       replace("head: "+headDigest, "head: "+strings.Repeat("g", 64)),
		"head in upper case": replace("head: "+headDigest, "head: "+strings.ToUpper(headDigest)),
		"head with a prefix": replace("head: "+headDigest, "head: 0x"+headDigest[2:]),
		"head with spaces":   replace("head: "+headDigest, "head: ' "+headDigest[2:]+" '"),
		"head a list":        replace("head: "+headDigest, "head: ["+headDigest+"]"),
		"tail missing":       strings.Replace(valid, "tail: "+tailDigest+"\n", "", 1),
		"tail too short":     replace("tail: "+tailDigest, "tail: "+tailDigest[1:]),
		"tail not hex":       replace("tail: "+tailDigest, "tail: "+strings.Repeat("-", 64)),
		"tail in upper case": replace("tail: "+tailDigest, "tail: "+strings.ToUpper(tailDigest)),
		"path a list":        replace("path: photos/2024/a.jpg", "path: [photos]"),
	}
	for name, text := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, err := Decode([]byte(text)); err == nil || got != (Entry{}) {
				t.Errorf("Decode accepted it: %+v, %v", got, err)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	t.Parallel()
	entry := sample()
	same := filestat.Stamp{
		Size:  entry.Size,
		Mtime: entry.MtimeNs,
		Ctime: entry.CtimeNs,
		Btime: entry.BtimeNs,
		Dev:   7,
		Ino:   entry.Inode,
		Mode:  0o644,
	}
	tests := map[string]struct {
		path  string
		stamp func(filestat.Stamp) filestat.Stamp
		want  bool
	}{
		"same path and stamp": {entry.Path, func(s filestat.Stamp) filestat.Stamp { return s }, true},
		"another path":        {"photos/2024/b.jpg", func(s filestat.Stamp) filestat.Stamp { return s }, false},
		"the path unescaped":  {"photos/2024/a.jpg/", func(s filestat.Stamp) filestat.Stamp { return s }, false},
		"another size":        {entry.Path, func(s filestat.Stamp) filestat.Stamp { s.Size++; return s }, false},
		"another mtime":       {entry.Path, func(s filestat.Stamp) filestat.Stamp { s.Mtime++; return s }, false},
		"another ctime":       {entry.Path, func(s filestat.Stamp) filestat.Stamp { s.Ctime++; return s }, false},
		"another btime":       {entry.Path, func(s filestat.Stamp) filestat.Stamp { s.Btime++; return s }, false},
		"another inode":       {entry.Path, func(s filestat.Stamp) filestat.Stamp { s.Ino++; return s }, false},
		// ZFS, btrfs and network file systems give a file system another device number after
		// a reboot, which must not make every entry stale.
		"another device": {entry.Path, func(s filestat.Stamp) filestat.Stamp { s.Dev++; return s }, true},
		"another mode":   {entry.Path, func(s filestat.Stamp) filestat.Stamp { s.Mode = 0o600; return s }, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := entry.Match(tt.path, tt.stamp(same)); got != tt.want {
				t.Errorf("Match(%q, %+v) = %v, want %v", tt.path, tt.stamp(same), got, tt.want)
			}
		})
	}
}
