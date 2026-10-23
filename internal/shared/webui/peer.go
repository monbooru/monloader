package webui

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
