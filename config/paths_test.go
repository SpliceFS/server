package config

import "testing"

func TestDefaultDirs(t *testing.T) {
	tests := []struct {
		prefix, config, state string
	}{
		{"", "/etc/splicefs/server", "/var/lib/splicefs/server"},
		{"/opt/homebrew", "/opt/homebrew/etc/splicefs/server", "/opt/homebrew/var/lib/splicefs/server"},
		{"/opt/homebrew/", "/opt/homebrew/etc/splicefs/server", "/opt/homebrew/var/lib/splicefs/server"},
	}
	for _, tt := range tests {
		t.Run(tt.prefix, func(t *testing.T) {
			old := prefix
			t.Cleanup(func() { prefix = old })
			prefix = tt.prefix
			if got := DefaultConfigDir(); got != tt.config {
				t.Errorf("DefaultConfigDir = %q, want %q", got, tt.config)
			}
			if got := DefaultStateDir(); got != tt.state {
				t.Errorf("DefaultStateDir = %q, want %q", got, tt.state)
			}
		})
	}
}
