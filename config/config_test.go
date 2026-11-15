package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/common/units"
)

func TestValidateShareNegativeMinAge(t *testing.T) {
	t.Parallel()
	errs := validateShare("media", Share{Path: "/srv/media", MinAge: units.Duration(-time.Second)})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "shares.media.min_age") {
		t.Errorf("validateShare = %v", errs)
	}
}

func TestRateUnmarshalText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want Rate
	}{
		{"10/m", Rate{10, time.Minute}},
		{"1/s", Rate{1, time.Second}},
		{"0/h", Rate{0, time.Hour}},
		{"600/h", Rate{600, time.Hour}},
	}
	for _, tt := range tests {
		var got Rate
		if err := got.UnmarshalText([]byte(tt.in)); err != nil {
			t.Errorf("UnmarshalText(%q): %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("UnmarshalText(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestRateUnmarshalTextErrors(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"", "10", "/m", "10/", "x/m", "-1/m", "+1/m", "1.5/m", "10/d", "10/min", "10/m/x",
		"99999999999/s",
	}
	for _, input := range inputs {
		before := Rate{Count: 7, Per: time.Hour}
		rate := before
		err := rate.UnmarshalText([]byte(input))
		if err == nil {
			t.Errorf("UnmarshalText(%q) succeeded with %+v", input, rate)
			continue
		}
		if !errors.Is(err, ErrInvalidRate) || !strings.Contains(err.Error(), `"`+input+`"`) {
			t.Errorf("UnmarshalText(%q) error %q does not name the value and the form", input, err)
		}
		if rate != before {
			t.Errorf("UnmarshalText(%q) changed the value to %+v", input, rate)
		}
	}
}

func TestRateString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   Rate
		want string
	}{
		{Rate{10, time.Minute}, "10/m"},
		{Rate{3, time.Second}, "3/s"},
		{Rate{60, time.Hour}, "60/h"},
		{Rate{5, 90 * time.Second}, "5/1m30s"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("%+v.String() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRateMarshalText(t *testing.T) {
	t.Parallel()
	got, err := Rate{10, time.Minute}.MarshalText()
	if err != nil || string(got) != "10/m" {
		t.Errorf("MarshalText = %q, %v", got, err)
	}
}
