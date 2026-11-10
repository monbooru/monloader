package webui

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"
)

func TestPairNoticeTextNamesOneRequestAndCountsSeveral(t *testing.T) {
	exp := time.Now().Add(5 * time.Minute)
	for _, c := range []struct {
		items []PairItem
		want  string
	}{
		{nil, ""},
		{[]PairItem{{App: "monedit", ExpiresAt: exp}}, "monedit wants to pair"},
		{[]PairItem{{App: "monedit", Repair: true, ExpiresAt: exp}}, "monedit wants to re-pair"},
		{[]PairItem{{App: "abcdefghijklmnopqrstuvwxyz", ExpiresAt: exp}}, "abcdefghijklm... wants to pair"},
		{[]PairItem{{App: "a", ExpiresAt: exp}, {App: "b", ExpiresAt: exp}, {App: "c", ExpiresAt: exp}}, "3 pairing requests"},
	} {
		if got := (PairNotice{Items: c.items}).Text(); got != c.want {
			t.Errorf("Text(%+v) = %q, want %q", c.items, got, c.want)
		}
	}
}

func TestPairNoticeKeyIgnoresOrderAndFollowsANewOffer(t *testing.T) {
	exp := time.Date(2026, 10, 24, 16, 42, 0, 0, time.Local)
	a, b := PairItem{App: "a", ExpiresAt: exp}, PairItem{App: "b", ExpiresAt: exp}
	if (PairNotice{Items: []PairItem{a, b}}).Key() != (PairNotice{Items: []PairItem{b, a}}).Key() {
		t.Error("the key depends on the store's order")
	}
	again := PairItem{App: "a", ExpiresAt: exp.Add(time.Second)}
	if (PairNotice{Items: []PairItem{a}}).Key() == (PairNotice{Items: []PairItem{again}}).Key() {
		t.Error("a repeat offer kept the key")
	}
	if (PairNotice{}).Key() != "" {
		t.Error("no request has a key")
	}
}

func TestPairNoticeTitleNamesTheSourceAndTheExpiry(t *testing.T) {
	exp := time.Date(2026, 10, 24, 16, 42, 0, 0, time.Local)
	n := PairNotice{Items: []PairItem{{App: "b", Source: "10.0.0.2", ExpiresAt: exp}, {App: "a", Version: "v1.2", Source: "10.0.0.1", ExpiresAt: exp}}}
	if got, want := n.Title(), "a v1.2 from 10.0.0.1, expires 16:42; b from 10.0.0.2, expires 16:42"; got != want {
		t.Errorf("Title = %q, want %q", got, want)
	}
}

func TestPairNoticeRendersHiddenWithNothingWaiting(t *testing.T) {
	tmpl := template.Must(template.New("").Funcs(Funcs()).ParseFS(Templates, "pair_notice.html"))
	render := func(n PairNotice) string {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "webui/pair_notice", n); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	empty := render(PairNotice{Href: "/settings", OOB: true})
	if !strings.Contains(empty, `id="pair-notice"`) || !strings.Contains(empty, " hidden>") || strings.Contains(empty, "[!") {
		t.Errorf("empty notice = %s", empty)
	}
	if !strings.Contains(empty, `hx-swap-oob="true"`) {
		t.Errorf("an out-of-band notice has no swap marker: %s", empty)
	}
	one := render(PairNotice{Href: "/settings", Items: []PairItem{{App: "monedit", ExpiresAt: time.Now().Add(time.Minute)}}})
	for _, want := range []string{`href="/settings"`, `aria-label="monedit wants to pair"`, "[! monedit wants to pair]", "[! pair]", "[!]"} {
		if !strings.Contains(one, want) {
			t.Errorf("notice lacks %q: %s", want, one)
		}
	}
	if strings.Contains(one, "hidden") || strings.Contains(one, "hx-swap-oob") {
		t.Errorf("a page-render notice is hidden or out of band: %s", one)
	}
}

func TestMinutesLeftRoundsUpAndNeverReadsZero(t *testing.T) {
	for d, want := range map[time.Duration]string{4*time.Minute + 10*time.Second: "5 min", 20 * time.Second: "1 min", -time.Minute: "1 min"} {
		if got := MinutesLeft(time.Now().Add(d)); got != want {
			t.Errorf("MinutesLeft(+%s) = %q, want %q", d, got, want)
		}
	}
}
