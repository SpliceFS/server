package config

import (
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
	for _, tc := range tests {
		var got Rate
		if err := got.UnmarshalText([]byte(tc.in)); err != nil {
			t.Errorf("UnmarshalText(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("UnmarshalText(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestRateUnmarshalTextErrors(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "10", "/m", "10/", "x/m", "-1/m", "+1/m", "1.5/m", "10/d", "10/min", "10/m/x", "99999999999/s"} {
		r := Rate{Count: 7, Per: time.Hour}
		err := r.UnmarshalText([]byte(in))
		if err == nil {
			t.Errorf("UnmarshalText(%q) succeeded with %+v", in, r)
			continue
		}
		if !strings.Contains(err.Error(), `"`+in+`"`) || !strings.Contains(err.Error(), "<count>/<s|m|h>") {
			t.Errorf("UnmarshalText(%q) error %q does not name the value and the form", in, err)
		}
		if r != (Rate{Count: 7, Per: time.Hour}) {
			t.Errorf("UnmarshalText(%q) changed the value to %+v", in, r)
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
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.in, got, tc.want)
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
