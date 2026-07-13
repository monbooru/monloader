package mapping

import (
	"reflect"
	"testing"
)

func TestMapPTRTags(t *testing.T) {
	in := []string{
		"blue_sky",             // bare -> general
		"creator:some_artist",  // -> artist
		"character:reimu",      // -> character
		"series:touhou",        // -> copyright
		"person:jane_doe",      // -> person
		"species:fox",          // -> general
		"medium:watercolor",    // -> medium
		"year:2024",            // -> year
		"rating:explicit",      // -> lifted rating tag
		"page:5",               // bookkeeping -> dropped
		"title:some doujin",    // bookkeeping -> dropped
		"weirdns:kept_general", // unrecognized namespace -> subtag as general
	}
	got := MapPTRTags(in)
	want := []string{
		"artist:some_artist",
		"blue_sky",
		"character:reimu",
		"copyright:touhou",
		"fox",
		"kept_general",
		"medium:watercolor",
		"person:jane_doe",
		"rating:explicit",
		"year:2024",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MapPTRTags = %v\n         want %v", got, want)
	}
}

func TestMapPTRTagsRatingLifted(t *testing.T) {
	// A safe rating maps to general; an unmapped rating value is dropped.
	got := MapPTRTags([]string{"rating:safe", "rating:nonsense"})
	if !reflect.DeepEqual(got, []string{"rating:general"}) {
		t.Errorf("rating mapping = %v, want [rating:general]", got)
	}
}

func TestMapPTRTagsNormalizesAndDedups(t *testing.T) {
	// Spaces fold to underscores; the alias and ideal spellings collapse.
	got := MapPTRTags([]string{"blue eyes", "blue_eyes", "creator:Some Artist"})
	want := []string{"artist:some_artist", "blue_eyes"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MapPTRTags = %v, want %v", got, want)
	}
}

func TestPTRNamespacesFor(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"blue_eyes", []string{"blue_eyes", "blue eyes"}},
		{"artist:some_name", []string{"creator:some_name", "creator:some name", "artist:some_name", "artist:some name"}},
		{"character:reimu", []string{"character:reimu"}},
		{"year:2024", []string{"year:2024"}},
		{"copyright:touhou_project", []string{
			"series:touhou_project", "series:touhou project",
			"copyright:touhou_project", "copyright:touhou project",
			"studio:touhou_project", "studio:touhou project",
		}},
	}
	for _, tc := range cases {
		if got := PTRNamespacesFor(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("PTRNamespacesFor(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
