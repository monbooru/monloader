package web

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/session"
)

type watchInterval struct {
	Hours int
	Label string
}

var watchIntervals = []watchInterval{
	{0, "manual"}, {6, "6h"}, {12, "12h"}, {24, "daily"}, {72, "3 days"}, {168, "weekly"},
}

const defaultWatchHours = 24

type watchRow struct {
	queue.Watch
	Items       []itemsChoice
	Result      string
	ResultClass string
	ResultTitle string
	Next        string
}

func (s *Server) watchesScreen(w http.ResponseWriter, r *http.Request) {
	data := s.base(r, "watches", "Watched - "+s.titleName())
	s.fillWatches(data)
	data["DefaultWatchHours"] = defaultWatchHours
	s.render(w, "watches", data)
}

func (s *Server) watchRows(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"CSRFToken": s.sessions.CSRFToken(session.FromContext(r.Context()))}
	s.fillWatches(data)
	s.render(w, "watch_rows", data)
}

func (s *Server) fillWatches(data map[string]any) {
	held := s.monbooruBlocked() != ""
	watches := s.queue.Watches()
	rows := make([]watchRow, len(watches))
	for i, wt := range watches {
		rows[i] = newWatchRow(wt, held)
		rows[i].Items = s.itemsChoices(wt.MaxItems)
	}
	data["Watches"] = rows
	data["ItemChoices"] = s.itemsChoices(0)
	data["Intervals"] = watchIntervals
	data["WatchStore"] = s.queue.WatchesAvailable()
}

func newWatchRow(wt queue.Watch, held bool) watchRow {
	row := watchRow{Watch: wt, ResultClass: "flash-warn"}
	last := wt.Last
	switch {
	case wt.Live:
		row.Result, row.ResultClass = "checking...", "hint"
	case last.JobID == 0:
		row.Result, row.ResultClass = "-", "hint"
	case last.Status == queue.JobQueued || last.Status == queue.JobRunning:
		row.Result = string(queue.JobInterrupted)
	case last.ErrorCode != "":
		row.Result, row.ResultTitle = last.ErrorCode, last.Error
	case last.Status != queue.JobSucceeded:
		row.Result = string(last.Status)
	case last.Total == 0:
		row.Result = "no posts matched"
	// gallery-dl's one-post extractors are all subcategory "post"; any
	// other, or none recorded, could be a search that matched one.
	case last.Subcategory == "post":
		row.Result = "a single post - nothing to watch"
		row.ResultTitle = "This URL resolves to one post, so a check never finds anything new."
	case last.Created > 0:
		row.Result, row.ResultClass = strconv.Itoa(last.Created)+" new", "o-created"
	default:
		row.Result, row.ResultClass = "nothing new", "hint"
		if last.Duplicate > 0 {
			row.ResultTitle = strconv.Itoa(last.Duplicate) + " already in monbooru"
		}
	}
	switch {
	case wt.Live:
		row.Next = "-"
	case wt.IntervalHours == 0:
		row.Next = "manual"
	case held:
		row.Next = "held"
	default:
		row.Next = humanDue(wt.Next().Unix())
		if wt.Last.CheckedAt.IsZero() {
			row.Next = "due now"
		}
	}
	return row
}

func validWatchHours(v string) (int, bool) {
	h, err := strconv.Atoi(v)
	return h, err == nil && slices.Contains(queue.WatchIntervals, h)
}

func watchFlash(w http.ResponseWriter, msg string) {
	w.Header().Set("HX-Retarget", "#watch-flash")
	w.Header().Set("HX-Reswap", "innerHTML")
	flashFragment(w, "err", msg)
}

func watchErrMsg(err error) string {
	switch {
	case errors.Is(err, queue.ErrNotFound):
		return "That watch is gone."
	case errors.Is(err, queue.ErrWatchExists), errors.Is(err, queue.ErrNoWatchStore), errors.Is(err, queue.ErrWatchLive):
		return err.Error()
	}
	return "Could not save the watch: " + err.Error()
}

func (s *Server) addWatch(w http.ResponseWriter, r *http.Request) {
	if msg := s.monbooruBlocked(); msg != "" {
		flashFragment(w, "err", msg)
		return
	}
	target := strings.TrimSpace(r.FormValue("url"))
	if !config.IsHTTPURL(target) {
		flashFragment(w, "err", "Enter a valid http(s) URL.")
		return
	}
	hours, ok := validWatchHours(r.FormValue("interval_hours"))
	if !ok {
		flashFragment(w, "err", "Pick how often to check.")
		return
	}
	items, ok := parseItems(r.FormValue("items"))
	if !ok {
		flashFragment(w, "err", "Pick how many items to fetch.")
		return
	}
	id, err := s.queue.AddWatch(target, hours, items, nil)
	if err != nil {
		flashFragment(w, "err", watchErrMsg(err))
		return
	}
	_, _ = s.queue.CheckWatch(id, false)
	w.Header().Set("HX-Retarget", "#watch-rows")
	w.Header().Set("HX-Reswap", "innerHTML")
	s.watchRows(w, r)
}

func (s *Server) watchJob(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	job, err := s.queue.Get(id)
	if err != nil {
		addBarFlash(w, "That job is no longer in the queue.")
		return
	}
	if !s.watchableJob(job, job.Summary.Total) {
		addBarFlash(w, "Only a finished search can be watched.")
		return
	}
	if _, err := s.queue.AddWatch(job.URL, defaultWatchHours, s.rowItems(job), job); err != nil {
		addBarFlash(w, watchErrMsg(err))
		return
	}
	s.queueRows(w, r)
}

func (s *Server) setWatchInterval(w http.ResponseWriter, r *http.Request) {
	hours, ok := validWatchHours(r.FormValue("interval_hours"))
	if !ok {
		watchFlash(w, "Pick how often to check.")
		return
	}
	s.watchAction(w, r, func(id int64) error { return s.queue.SetWatchInterval(id, hours) })
}

func (s *Server) setWatchItems(w http.ResponseWriter, r *http.Request) {
	items, ok := parseItems(r.FormValue("items"))
	if !ok {
		watchFlash(w, "Pick how many items to fetch.")
		return
	}
	s.watchAction(w, r, func(id int64) error { return s.queue.SetWatchItems(id, items) })
}

func (s *Server) checkWatch(w http.ResponseWriter, r *http.Request) {
	if msg := s.monbooruBlocked(); msg != "" {
		watchFlash(w, msg)
		return
	}
	s.watchAction(w, r, func(id int64) error {
		_, err := s.queue.CheckWatch(id, false)
		return err
	})
}

func (s *Server) removeWatch(w http.ResponseWriter, r *http.Request) {
	s.watchAction(w, r, s.queue.RemoveWatch)
}

func (s *Server) watchAction(w http.ResponseWriter, r *http.Request, act func(id int64) error) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := act(id); err != nil {
		watchFlash(w, watchErrMsg(err))
		return
	}
	s.watchRows(w, r)
}

// A single post has nothing new to find, and a manga title or gallery
// would be fetched whole on every check. A window of a series or a capped
// one is a search whatever it held; a row that never resolved could be
// either, and a search that matched nothing resolved with a success.
func (s *Server) watchableJob(j *queue.Job, total int) bool {
	if j.Status == queue.JobQueued || j.Status == queue.JobRunning {
		return false
	}
	search := (j.Root != 0 && j.Root != j.ID) || j.Capped || total > 1 ||
		(total == 0 && j.Status == queue.JobSucceeded)
	return j.Kind == "" && config.IsHTTPURL(j.URL) && search &&
		s.mapper.KindOf(j.Site) != mapping.KindManga
}

// A watch made from a row keeps the count the row was fetched with.
func (s *Server) rowItems(j *queue.Job) int {
	limit := s.cfg.Current().Downloader.MaxItemsPerJob
	switch {
	case j.Target > 0:
		return j.Target
	case j.Auto:
		return queue.ItemsAll
	case j.MaxItems > 0 && j.MaxItems < limit:
		return j.MaxItems
	}
	return 0
}

func (s *Server) watchedURLs() map[string]bool {
	urls := map[string]bool{}
	for _, wt := range s.queue.Watches() {
		urls[wt.URL] = true
	}
	return urls
}
