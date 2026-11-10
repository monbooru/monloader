package mapping

import (
	"slices"
	"testing"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/contract"
)

func TestMergeExtrasSendsClientTagsApart(t *testing.T) {
	pf := PushFields{PostFields: contract.PostFields{Tags: []string{"1girl", "rating:general"}}, Via: Via}
	got := MergeExtras(pf, Extras{Tags: []string{"Meta:From Alice", "photo", "photo", ""}}, false)

	if want := []string{"1girl", "rating:general"}; !slices.Equal(got.Tags, want) {
		t.Errorf("tags = %v, want the post's own %v", got.Tags, want)
	}
	if want := []string{"meta:from_alice", "photo"}; !slices.Equal(got.ClientTags, want) {
		t.Errorf("client tags = %v, want %v, the duplicate and the empty dropped", got.ClientTags, want)
	}
}

func TestMergeExtrasKeepsThePostsRating(t *testing.T) {
	rated := PushFields{PostFields: contract.PostFields{Tags: []string{"rating:explicit"}}, Rating: "explicit"}
	got := MergeExtras(rated, Extras{Tags: []string{"rating:general", "kept"}}, false)
	assertTags(t, got.ClientTags, "kept")
	assertNoTags(t, got.ClientTags, "rating:general")

	unrated := MergeExtras(PushFields{}, Extras{Tags: []string{"rating:sensitive"}}, false)
	assertTags(t, unrated.ClientTags, "rating:sensitive")
}

func TestMergeExtrasSkipsTagRules(t *testing.T) {
	m := newMapper(t, config.Default())
	m.view().profiles["newgrounds"] = Profile{Family: FamilyGeneric, TagRules: map[string]string{
		"cat":       "meta:renamed_by_the_site_rule",
		"blue_hair": "aqua_hair",
	}}
	meta := map[string]any{
		"category": "newgrounds",
		"id":       float64(1),
		"tags":     []any{"blue hair"},
	}
	pf := MergeExtras(m.Map(meta), Extras{Tags: []string{"cat"}}, true)

	assertTags(t, pf.Tags, "aqua_hair")
	assertTags(t, pf.ClientTags, "cat")
	assertNoTags(t, append(pf.Tags, pf.ClientTags...), "meta:renamed_by_the_site_rule", "blue_hair")
}

func TestMergeExtrasKeepsThePostsOwnValues(t *testing.T) {
	pf := PushFields{PostFields: contract.PostFields{Source: "danbooru", Commentary: "the artist's own words"}, Via: Via}
	got := MergeExtras(pf, Extras{Commentary: "shared by alice", Source: "discord", Via: "my-sender"}, true)

	if got.Commentary != "the artist's own words" {
		t.Errorf("commentary = %q, want the post's own", got.Commentary)
	}
	if got.Source != "danbooru" {
		t.Errorf("source = %q, want danbooru: a claimed label must not be overridden", got.Source)
	}
	if got.Via != "my-sender" {
		t.Errorf("via = %q, want my-sender: the mapper's value is a constant, not the post's", got.Via)
	}
}

func TestMergeExtrasFillsWhatThePostLacks(t *testing.T) {
	pf := PushFields{PostFields: contract.PostFields{Source: "cdn.example"}, Via: Via}
	got := MergeExtras(pf, Extras{Commentary: "shared by alice", Source: "discord"}, false)

	if got.Commentary != "shared by alice" {
		t.Errorf("commentary = %q, want the client's", got.Commentary)
	}
	if got.Source != "discord" {
		t.Errorf("source = %q, want discord: nothing claimed the host", got.Source)
	}
}

func TestSourceClaimed(t *testing.T) {
	cfg := config.Default()
	cfg.HostLabels = []config.HostLabel{{Host: "wiki.example", Label: "example wiki"}}
	m := newMapper(t, cfg)

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"danbooru", true},
		{"wiki.example", true},
		{"img.wiki.example", true},
		{"cdn.example", false},
		{"donmai.us", true},
	} {
		if got := m.SourceClaimed(tc.name); got != tc.want {
			t.Errorf("SourceClaimed(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
