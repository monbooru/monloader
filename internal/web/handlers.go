package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/api"
	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/logx"
	"github.com/monbooru/monloader/internal/shared/webui"
)

func (s *Server) addScreen(w http.ResponseWriter, r *http.Request) {
	data := s.base(r, "add", s.titleName())
	data["MD5LookupOff"] = s.md5LookupOff()
	data["ItemChoices"] = s.itemsChoices(0)
	s.render(w, "add", data)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		api.NotFound(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	s.render(w, "notfound", s.base(r, "", "not found"))
}

func (s *Server) enqueueForm(w http.ResponseWriter, r *http.Request) {
	if msg := s.monbooruBlocked(); msg != "" {
		flashFragment(w, "err", msg)
		return
	}
	target := config.NormalizeURL(strings.TrimSpace(r.FormValue("url")))
	if target == "" {
		flashFragment(w, "err", "Enter a URL or an MD5 hash.")
		return
	}
	if md5, ok := hashImportTarget(target); ok {
		if len(s.mapper.LookupSites()) == 0 {
			flashFragment(w, "err", "MD5 lookup is off: no site in the lookup chain. Add one in Settings.")
			return
		}
		s.queue.EnqueueHashImport(md5, queue.Options{})
	} else if looksLikeSHA256(target) {
		flashFragment(w, "err", sha256AddHint(s.ptr.Status()))
		return
	} else if !config.IsHTTPURL(target) {
		flashFragment(w, "err", "Enter a valid http(s) URL or an MD5 hash.")
		return
	} else {
		items, ok := parseItems(r.FormValue("items"))
		if !ok {
			flashFragment(w, "err", "Pick how many posts to fetch.")
			return
		}
		s.queue.Enqueue(target, queue.Options{}.WithItems(items))
	}
	// A redirect would reload the queue screen and drop its
	// expand/collapse state.
	if onQueueScreen(r) {
		w.Header().Set("HX-Retarget", "#queue-rows")
		w.Header().Set("HX-Reswap", "innerHTML")
		s.queueRows(w, r)
		return
	}
	w.Header().Set("HX-Redirect", "/queue")
}

// The controls are disabled client-side too; this guards a stale page.
func (s *Server) monbooruBlocked() string {
	if !s.monbooruConfigured() {
		return "No monbooru address set, so downloads are off. Set it in Settings."
	}
	if s.cfg.Current().Monbooru.Unconfigured() {
		return "Not paired with monbooru, so downloads are off. Pair it in Settings."
	}
	if s.monbooruPaused() {
		return "The monbooru connection is paused, so downloads are off. Click the dot in the footer to resume."
	}
	return ""
}

// Lower-cased before the prefix trim: a hash pasted upper-case carries an
// upper-case prefix.
func hashImportTarget(s string) (string, bool) {
	s = strings.TrimPrefix(strings.ToLower(s), "md5:")
	if config.IsHexHash(s, 32) {
		return s, true
	}
	return "", false
}

func looksLikeSHA256(s string) bool {
	return config.IsHexHash(strings.TrimPrefix(strings.ToLower(s), "sha256:"), 64)
}

func sha256AddHint(st ptr.Status) string {
	switch {
	case !st.Enabled:
		return "A SHA-256 looks tags up from the PTR, which is off - enable it on the PTR page."
	case st.Queryable:
		return "A SHA-256 is for looking tags up from the PTR (via monbooru), not for importing here - paste an MD5 or a URL."
	case st.State == ptr.StateError:
		return "A SHA-256 looks tags up from the PTR, whose sync stopped on an error - see the PTR page."
	case st.State == ptr.StatePaused:
		return "A SHA-256 looks tags up from the PTR, whose sync is paused - resume it on the PTR page."
	default:
		return "A SHA-256 looks tags up from the PTR, which answers once it has finished syncing - follow it on the PTR page."
	}
}

type itemsChoice struct {
	Value    string
	Label    string
	Selected bool
}

var itemsPresets = []int{1, 5, 10, 20, 50, 100, 200, 500, 1000}

// A pick past the cap is offered because the queue fetches it in cap-sized windows.
func (s *Server) itemsChoices(current int) []itemsChoice {
	limit := s.cfg.Current().Downloader.MaxItemsPerJob
	nums := slices.Clone(itemsPresets)
	for _, n := range []int{limit, current} {
		if n > 0 && !slices.Contains(nums, n) {
			nums = append(nums, n)
		}
	}
	slices.Sort(nums)
	out := make([]itemsChoice, 0, len(nums)+1)
	for _, n := range nums {
		c := itemsChoice{Value: strconv.Itoa(n), Label: "up to " + strconv.Itoa(n), Selected: n == current}
		if n == limit {
			c.Value, c.Selected = "", current == 0 || current == limit
		}
		out = append(out, c)
	}
	return append(out, itemsChoice{Value: "all", Label: "all", Selected: current == queue.ItemsAll})
}

// "" keeps the cap.
func parseItems(v string) (int, bool) {
	switch v {
	case "":
		return 0, true
	case "all":
		return queue.ItemsAll, true
	}
	n, err := strconv.Atoi(v)
	return n, err == nil && n > 0
}

func (s *Server) md5LookupOff() bool {
	for _, src := range s.mapper.LookupChain() {
		if !src.Similarity && s.mapper.SourceQueryable(src, s.sim.Missing) {
			return false
		}
	}
	return true
}

func retargetFlash(w http.ResponseWriter, target, msg string) {
	w.Header().Set("HX-Retarget", target)
	w.Header().Set("HX-Reswap", "innerHTML")
	flashFragment(w, "err", msg)
}

func flashFragment(w http.ResponseWriter, kind, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<span class="flash-%s">%s</span>`, kind, html.EscapeString(msg))
}

func (s *Server) monbooruWebBase() string {
	return s.cfg.Current().Monbooru.WebBase()
}

func (s *Server) monbooruStatus(w http.ResponseWriter, r *http.Request) {
	// The pairing panel renders on Settings alone, and an approval must not
	// wait for the operator to open it. A paused link stays unprobed.
	if att := s.getPairAttempt(); att != nil && !s.monbooruPaused() {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		s.claimMonbooruPair(ctx, att)
		cancel()
	}
	status, version := s.monbooruStatusCached(r.Context())
	s.renderConnLight(w, status, version)
	// The light is the one poll every screen runs, so it carries the nav
	// notice too, swapped only when the pending set moved.
	if n := s.pairNotice(); r.FormValue("seen") != n.Key() {
		n.OOB = true
		s.render(w, "webui/pair_notice", n)
	}
}

func (s *Server) setMonbooruPaused(paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.cfg.Update(func(c *config.Config) error { c.Monbooru.Paused = paused; return nil }); err != nil {
			logx.Errorf("monbooru: could not persist the link hold: %v", err)
		}
		// What was stored, not what was asked: a failed save leaves the old
		// state in force. A live link renders checking, whose swap probes a
		// second later rather than at the next 15 s poll, on a fresh check.
		status := webui.ConnPaused
		if !s.monbooruPaused() {
			status = connChecking
			s.statusMu.Lock()
			s.monbooruConn = ""
			s.statusMu.Unlock()
		}
		s.renderConnLight(w, status, "")
	}
}

func (s *Server) renderConnLight(w http.ResponseWriter, status, version string) {
	// The poll must answer a resolved state: "checking" arms the load
	// trigger, so each swap would poll again a second later.
	s.render(w, "webui/peer_light", s.monbooruLight(status, version))
}

func (s *Server) monbooruLight(conn, version string) webui.PeerLight {
	return webui.PeerLight{
		Peer: contract.AppMonbooru, Href: s.monbooruWebBase(), Conn: conn, Version: version,
		Mismatch: s.monbooruAPIMismatch(), OffReason: monbooruOffReason(conn),
		StatusURL: "/internal/monbooru-status",
		PauseURL:  "/internal/monbooru/pause", ResumeURL: "/internal/monbooru/resume",
		CheckNow: conn == connChecking, Paired: s.hasPairedToken(contract.AppMonbooru),
		Seen: s.pairNotice().Key(),
	}
}
