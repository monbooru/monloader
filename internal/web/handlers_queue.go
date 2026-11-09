package web

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monloader/internal/queue"
)

func onQueueScreen(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("HX-Current-URL"))
	return err == nil && u.Path == "/queue"
}

func (s *Server) queueScreen(w http.ResponseWriter, r *http.Request) {
	data := s.base(r, "queue", "Queue - "+s.titleName())
	s.fillQueue(r, data)
	data["ItemChoices"] = s.itemsChoices(0)
	data["Actions"] = s.queueActions()
	data["MD5LookupOff"] = s.md5LookupOff()
	s.render(w, "queue", data)
}

type queueActionsView struct {
	HasPending   bool
	HasFinished  bool
	HasSucceeded bool
}

func (s *Server) queueActions() queueActionsView {
	counts := s.queueCounts()
	return queueActionsView{
		HasPending:   counts.Queued > 0,
		HasFinished:  counts.Finished > 0,
		HasSucceeded: s.queue.ClearableSucceeded() > 0,
	}
}

// Without OmitItems the snapshot deep-copies every job's items.
func (s *Server) queueCounts() queueStats {
	jobs, _ := s.queue.List(queue.ListOptions{OmitItems: true})
	var st queueStats
	// Counted per row, as the queue page shows them, so both screens
	// agree on a continued search.
	for _, g := range groupJobs(jobs) {
		switch g.Worst.Status {
		case queue.JobQueued:
			st.Queued++
		case queue.JobRunning:
			st.Running++
		default:
			st.Finished++
		}
	}
	return st
}

func (s *Server) queueActionsFragment(w http.ResponseWriter, r *http.Request) {
	s.render(w, "queue_actions", s.queueActions())
}

func (s *Server) queueRows(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{}
	s.fillQueue(r, data)
	s.render(w, "queue_rows", data)
}

func (s *Server) queueRowItems(w http.ResponseWriter, r *http.Request) {
	root, err := strconv.ParseInt(r.PathValue("root"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jobs, _ := s.queue.List(queue.ListOptions{OmitItems: true})
	for _, g := range groupJobs(jobs) {
		if g.Root == root {
			s.fillItems(&g)
			s.render(w, "queue_items_capped", map[string]any{"Items": g.Items, "MonbooruURL": s.monbooruWebBase(), "Site": g.Lead.Site, "ImageID": g.Lead.ImageID, "Kind": g.Lead.Kind, "JobURL": g.Lead.URL, "PageURL": g.Lead.PageURL})
			return
		}
	}
	http.NotFound(w, r)
}

type jobGroup struct {
	Root    int64
	Lead    *queue.Job
	Summary queue.Summary
	// The row reports on its most demanding window, so a clean
	// continuation cannot bury an earlier one that needs re-running.
	Worst *queue.Job
	// Not the lead: a cancel restamps its window as the newest activity.
	// Any capped window is a safe target; the follow-up starts past the
	// series' furthest reach.
	Continue *queue.Job
	IDs      []int64
	Items    []queue.Item
	// Summed from the snapshots: a finished row's Items stay empty until
	// it is expanded.
	Count int
	// Items of a live row the poll did not send; opening its "+N more" asks
	// for them.
	More       int
	pending    bool
	pushing    bool
	NextWindow int
	widest     int
	Watchable  bool
	Watched    bool
}

var rowStatusOrder = []queue.JobStatus{queue.JobRunning, queue.JobQueued, queue.JobFailed, queue.JobPartial, queue.JobInterrupted, queue.JobCanceled}

func rowStatusRank(s queue.JobStatus) int {
	if i := slices.Index(rowStatusOrder, s); i >= 0 {
		return i
	}
	return len(rowStatusOrder)
}

func continuable(j *queue.Job) bool {
	return j.Capped && j.Status != queue.JobCanceled && j.Status != queue.JobFailed
}

var (
	gdlLogPrefix = regexp.MustCompile(`^\[[^\]]+\]\[error\] `)
	gdlHTTPError = regexp.MustCompile(`^HttpError '([^']+)' for `)
)

func (g jobGroup) Live() bool {
	return g.Worst.Status == queue.JobQueued || g.Worst.Status == queue.JobRunning
}

// A coded error is stored as "<code>: <message>", and the code is
// printed beside the reason already.
func (g jobGroup) Reason() string {
	reason := gdlLogPrefix.ReplaceAllString(strings.TrimPrefix(g.Lead.Error, g.Lead.ErrorCode+": "), "")
	if m := gdlHTTPError.FindStringSubmatch(reason); m != nil {
		return m[1]
	}
	if strings.HasPrefix(reason, "Unsupported URL ") {
		return ""
	}
	return reason
}

func (g jobGroup) ShowTotal() bool {
	s := g.Summary
	return !slices.Contains([]int{s.Created, s.Duplicate, s.Enriched, s.Matched, s.Replaced, s.Skipped - s.Blacklisted, s.Blacklisted, s.Failed, s.Canceled}, s.Total)
}

func (g jobGroup) Phase() string {
	if !g.Live() {
		return "finished"
	}
	pending, pushing := g.pending, g.pushing
	for _, it := range g.Items {
		pending = pending || it.Status == queue.ItemPending
		pushing = pushing || it.Status == queue.ItemDownloaded || it.Status == queue.ItemUploaded
	}
	if pushing && !pending {
		return "pushing"
	}
	return "downloading"
}

// jobs must be newest first and snapshots the caller owns: a
// single-window group keeps its item slice.
func groupJobs(jobs []*queue.Job) []jobGroup {
	groups := make([]jobGroup, 0, len(jobs))
	at := map[int64]int{}
	for _, j := range jobs {
		root := j.Root
		if root == 0 {
			root = j.ID
		}
		if i, ok := at[root]; ok {
			g := &groups[i]
			g.IDs = append([]int64{j.ID}, g.IDs...)
			g.Items = slices.Concat(j.Items, g.Items)
			g.Summary = g.Summary.Add(j.Summary)
			g.Count += j.ItemCount()
			if rowStatusRank(j.Status) < rowStatusRank(g.Worst.Status) {
				g.Worst = j
			}
			g.widest = max(g.widest, j.Cap)
			if continuable(j) && (g.Continue == nil || j.ID > g.Continue.ID) {
				g.Continue = j
			}
			continue
		}
		at[root] = len(groups)
		g := jobGroup{
			Root:    root,
			Lead:    j,
			Worst:   j,
			Summary: j.Summary,
			IDs:     []int64{j.ID},
			Items:   j.Items,
			Count:   j.ItemCount(),
			widest:  j.Cap,
		}
		if continuable(j) {
			g.Continue = j
		}
		groups = append(groups, g)
	}
	return groups
}

func (s *Server) fillItems(g *jobGroup) {
	for _, id := range g.IDs {
		if j, err := s.queue.Get(id); err == nil {
			g.Items = append(g.Items, j.Items...)
		}
	}
}

func (s *Server) fillHead(g *jobGroup) {
	for _, id := range g.IDs {
		head, pending, pushing, err := s.queue.LiveHead(id, maxQueueItems-len(g.Items))
		if err != nil {
			continue
		}
		g.Items = append(g.Items, head...)
		g.pending = g.pending || pending
		g.pushing = g.pushing || pushing
	}
	g.More = max(g.Count-len(g.Items), 0)
}

func (s *Server) fillQueue(r *http.Request, data map[string]any) {
	expanded := strings.Split(r.FormValue("more"), ",")
	data["Mascot"] = themeMascots(s.activeTheme())
	jobs, _ := s.queue.List(queue.ListOptions{OmitItems: true})
	groups := groupJobs(jobs)
	page, totalPages := pageWindow(r, len(groups))
	lo := (page - 1) * pageSize
	shown := groups[lo:min(lo+pageSize, len(groups))]
	configured := s.cfg.Current().Downloader.MaxItemsPerJob
	watched := s.watchedURLs()
	for i := range shown {
		shown[i].Watched = watched[shown[i].Lead.URL]
		shown[i].Watchable = !shown[i].Live() && s.watchableJob(shown[i].Lead, shown[i].Summary.Total)
		if shown[i].Continue != nil {
			shown[i].NextWindow = nextWindow(shown[i].widest, configured)
		}
		// A finished row loads its items on expand, and a live one sends
		// the ones past the cap only once its "+N more" is open, so the
		// poll does not grow with the job.
		if shown[i].Live() {
			if slices.Contains(expanded, strconv.FormatInt(shown[i].Root, 10)) {
				s.fillItems(&shown[i])
			} else {
				s.fillHead(&shown[i])
			}
		}
	}
	data["Groups"] = shown
	data["Page"] = page
	data["TotalPages"] = totalPages
	data["MonbooruURL"] = s.monbooruWebBase()
}

func nextWindow(seriesCap, configured int) int {
	if configured > 0 && configured < seriesCap {
		return configured
	}
	return seriesCap
}

// FormValue: htmx sends the inherited hx-vals in the body on a POST and
// in the query on the poll.
func pageWindow(r *http.Request, n int) (page, totalPages int) {
	totalPages = max((n+pageSize-1)/pageSize, 1)
	page, _ = strconv.Atoi(r.FormValue("page"))
	return min(max(page, 1), totalPages), totalPages
}

func (s *Server) jobAction(action func(id int64) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil {
			switch err := action(id); {
			case errors.Is(err, queue.ErrNotFound):
				addBarFlash(w, "That job is no longer in the queue.")
				return
			case errors.Is(err, queue.ErrNoneRetried):
				addBarFlash(w, "This download is still running - wait for it to finish.")
				return
			case errors.Is(err, queue.ErrNotCapped):
				addBarFlash(w, "This search has no more posts to fetch.")
				return
			}
		}
		s.queueRows(w, r)
	}
}

// Retargeted so a refusal does not swap the unchanged rows back in as if
// it worked.
func addBarFlash(w http.ResponseWriter, msg string) {
	retargetFlash(w, "#add-flash", msg)
}

// force re-runs the whole series: the row's archive skips are summed
// across its windows.
func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	if msg := s.monbooruBlocked(); msg != "" {
		addBarFlash(w, msg)
		return
	}
	s.jobAction(func(id int64) error {
		if r.URL.Query().Get("force") == "1" {
			return s.queue.RetrySeries(id, true)
		}
		return s.queue.Retry(id, false)
	})(w, r)
}

func (s *Server) continueAction(run func(id int64) (int64, error)) http.HandlerFunc {
	action := s.jobAction(func(id int64) error {
		_, err := run(id)
		return err
	})
	return func(w http.ResponseWriter, r *http.Request) {
		if msg := s.monbooruBlocked(); msg != "" {
			addBarFlash(w, msg)
			return
		}
		action(w, r)
	}
}

func (s *Server) then(op func(), render http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		op()
		render(w, r)
	}
}

func (s *Server) renderPauseToggle(w http.ResponseWriter, r *http.Request) {
	s.render(w, "pause_toggle", map[string]any{
		"Paused": s.queue.Paused(),
	})
}

type queueStats struct {
	Workers  int
	Queued   int
	Running  int
	Finished int
}
