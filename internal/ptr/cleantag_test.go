package ptr

import (
	"strings"
	"testing"
)

func TestCleanTagParity(t *testing.T) {
	cases := []struct{ in, want string }{
		{"blonde hair", "blonde hair"},
		{"Blonde_Hair", "blonde_hair"},
		{"blonde  hair", "blonde hair"},
		{"  spaced out  ", "spaced out"},
		{"Creator:Some Name", "creator:some name"},
		{"series:  touhou  project ", "series:touhou project"},
		{"creator:x", "creator:x"},
		{"ns:", "ns:"},
		{":D", "::d"},
		{":weird:stuff", ":weird:stuff"},
		{"system:everything", "everything"},
		{"-leading-dash", "leading-dash"},
		{"a\tb", "ab"},
		{"new\nline", "newline"},
		{"blue_eyes\u200c", "blue_eyes"},
		{"\u65e5\u672c\u200c", "\u65e5\u672c\u200c"},
		{"abc\u3164", "abc"},
		{"\ud55c\uad6d\u3164", "\ud55c\uad6d\u3164"},
		{"double::colon", "double::colon"},
		{"UPPER:Case", "upper:case"},
		{":", ""},
		{"   ", ""},
		{"zero\u200bwidth", "zerowidth"},
		{"bom\ufefftag", "bomtag"},
		{"rtl\u202eovr", "rtlovr"},
		{"nbsp\u00a0joined", "nbsp joined"},
		{"ideographic\u3000space", "ideographic space"},

		{strings.Repeat("x", 1030), strings.Repeat("x", 1024)},
		{"creator:" + strings.Repeat("y", 1030), "creator:" + strings.Repeat("y", 1016)},
	}
	for _, tc := range cases {
		if got := cleanTag(tc.in); got != tc.want {
			t.Errorf("cleanTag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTagOK(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"blonde hair", true},
		{"creator:some name", true},
		{"::d", true},
		{"Blonde_Hair", false},
		{"blonde  hair", false},
		{"ns:", false},
		{"", false},
		{"system:everything", false},
	}
	for _, tc := range cases {
		if got := TagOK(tc.in); got != tc.ok {
			t.Errorf("TagOK(%q) = %v, want %v", tc.in, got, tc.ok)
		}
	}
}
