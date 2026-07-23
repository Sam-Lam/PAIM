package archive

import "testing"

func TestDeriveLabel(t *testing.T) {
	cases := []struct {
		name   string
		folder string
		want   string
	}{
		{"generic_dcim_excluded", "DCIM", ""},
		{"dcim_lowercase_excluded", "dcim", ""},
		{"camera_roll_fuji_excluded", "100_FUJI", ""},
		{"camera_roll_canon_excluded", "100CANON", ""},
		{"camera_roll_msdcf_excluded", "101MSDCF", ""},
		{"misc_excluded", "MISC", ""},
		{"private_excluded", "PRIVATE", ""},
		{"avchd_excluded", "AVCHD", ""},
		{"thmbnl_excluded", "THMBNL", ""},
		{"year_folder_excluded", "2019", ""},
		{"plain_label_kept", "old memories", "old memories"},
		{"date_prefixed_label_stripped", "2019-06-12 Yosemite", "Yosemite"},
		{"date_prefixed_multiword", "2019-06-12 Summer Beach Trip", "Summer Beach Trip"},
		{"pure_date_none", "2019-06-12", ""},
		{"pure_date_trailing_space_none", "2019-06-12 ", ""},
		{"label_with_separators_sanitized", "Trip/../etc", "Trip..etc"},
		{"label_trimmed", "  Beach Day  ", "Beach Day"},
		{"empty_none", "", ""},
		{"three_digits_only_kept", "100", "100"}, // not a camera-roll pattern (needs letters)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveLabel(c.folder); got != c.want {
				t.Errorf("DeriveLabel(%q) = %q, want %q", c.folder, got, c.want)
			}
		})
	}
}
