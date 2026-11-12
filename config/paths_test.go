package config

import (
	"errors"
	"os"
	"testing"
)

func TestResolveDir(t *testing.T) {
	t.Parallel()
	home := func() (string, error) { return "/home/u", nil }
	tests := []struct {
		name, built, fallback, want string
	}{
		{"system default", "", "/etc/splicefs/server", "/etc/splicefs/server"},
		{"home default", "", "~/.splicefs/server", "/home/u/.splicefs/server"},
		{"build setting wins", "/opt/x", "/etc/splicefs/server", "/opt/x"},
		{"build setting in the home directory", "~/x/y", "/etc/splicefs/server", "/home/u/x/y"},
		{"a tilde alone is a name", "~x", "/etc", "~x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveDir(tt.built, tt.fallback, home)
			if err != nil || got != tt.want {
				t.Errorf("resolveDir = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestResolveDirWithoutHome(t *testing.T) {
	t.Parallel()
	noHome := errors.New("no home")
	failing := func() (string, error) { return "", noHome }
	_, err := resolveDir("", "~/.splicefs/server", failing)
	const want = "find the home directory for ~/.splicefs/server: no home"
	if !errors.Is(err, noHome) || err.Error() != want {
		t.Errorf("err = %v", err)
	}
	// A directory outside the home directory needs no home.
	if got, err := resolveDir("/x", "", failing); err != nil || got != "/x" {
		t.Errorf("resolveDir = %q, %v", got, err)
	}
}

func TestDefaultDirs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldConfig, oldState := defaultConfigDir, defaultStateDir
	t.Cleanup(func() { defaultConfigDir, defaultStateDir = oldConfig, oldState })

	defaultConfigDir, defaultStateDir = "", ""
	wantConfig, _ := resolveDir("", osConfigDir, os.UserHomeDir)
	wantState, _ := resolveDir("", osStateDir, os.UserHomeDir)
	if got, err := DefaultConfigDir(); err != nil || got != wantConfig {
		t.Errorf("DefaultConfigDir = %q, %v, want %q", got, err, wantConfig)
	}
	if got, err := DefaultStateDir(); err != nil || got != wantState {
		t.Errorf("DefaultStateDir = %q, %v, want %q", got, err, wantState)
	}

	defaultConfigDir, defaultStateDir = "/c", "/s"
	if got, _ := DefaultConfigDir(); got != "/c" {
		t.Errorf("DefaultConfigDir = %q, want the build setting", got)
	}
	if got, _ := DefaultStateDir(); got != "/s" {
		t.Errorf("DefaultStateDir = %q, want the build setting", got)
	}
}
