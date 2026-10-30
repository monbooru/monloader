package webui

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The connection words webui/peer_light shows; anything else reads as
// still checking.
const (
	ConnOK           = "ok"
	ConnPaused       = "paused"
	ConnRejected     = "rejected"
	ConnDown         = "down"
	ConnUnconfigured = "unconfigured"
)

// PeerLight is what webui/peer_light reads: the footer light for the
// paired app.
type PeerLight struct {
	Peer string
	// The peer's page the name links to, when there is one.
	Href     string
	Conn     string
	Version  string
	Mismatch string
	// Why the controls that need the peer are off; empty while they are on.
	OffReason string
	StatusURL string
	PauseURL  string
	ResumeURL string
	// Sent back on each poll for the app's own use.
	Seen string
	// Polls a second after the swap as well as every 15 s.
	CheckNow bool
	// Without a pairing the light polls but shows nothing.
	Paired bool
}

// PairNotice is what webui/pair_notice reads: the nav link to the pairing
// requests waiting for the operator.
type PairNotice struct {
	Href  string
	Items []PairItem
	OOB   bool
}

type PairItem struct {
	App, Source, Version string
	Repair               bool
	ExpiresAt            time.Time
}

// Longer names are cut so the notice keeps the width its breakpoints
// were sized for.
const maxNoticeName = 16

// The pending store is a map, so the items come in no fixed order.
func (n PairNotice) sorted() []PairItem {
	return slices.SortedFunc(slices.Values(n.Items), func(a, b PairItem) int { return strings.Compare(a.App, b.App) })
}

// Key changes whenever the requests do: a new offer always carries a new
// expiry.
func (n PairNotice) Key() string {
	parts := make([]string, 0, len(n.Items))
	for _, it := range n.sorted() {
		parts = append(parts, it.App+"@"+strconv.FormatInt(it.ExpiresAt.UnixNano(), 36))
	}
	return strings.Join(parts, ",")
}

func (n PairNotice) Text() string {
	switch len(n.Items) {
	case 0:
		return ""
	case 1:
		name := n.Items[0].App
		if r := []rune(name); len(r) > maxNoticeName {
			name = string(r[:maxNoticeName-3]) + "..."
		}
		if n.Items[0].Repair {
			return name + " wants to re-pair"
		}
		return name + " wants to pair"
	}
	return fmt.Sprintf("%d pairing requests", len(n.Items))
}

func (n PairNotice) Title() string {
	parts := make([]string, 0, len(n.Items))
	for _, it := range n.sorted() {
		who := it.App
		if it.Version != "" {
			who += " " + it.Version
		}
		parts = append(parts, who+" from "+it.Source+", expires "+it.ExpiresAt.Format("15:04"))
	}
	return strings.Join(parts, "; ")
}

func MinutesLeft(t time.Time) string {
	return fmt.Sprintf("%d min", max(1, int(math.Ceil(time.Until(t).Minutes()))))
}
