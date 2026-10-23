package web

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"

	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/queue"
)

type itemDisplay struct {
	Subject      string
	SubjectURL   string
	DebugURL     string
	Outcome      string
	OutcomeCls   string
	OutcomeTitle string
	Note         string
	ViewURL      string
	Warnings     []string
}

func itemView(it queue.Item, ctx map[string]any) itemDisplay {
	kind, _ := ctx["Kind"].(queue.JobKind)
	base, _ := ctx["MonbooruURL"].(string)
	imageID, _ := ctx["ImageID"].(int64)
	site, _ := ctx["Site"].(string)

	d := itemDisplay{Warnings: it.TagWarnings}
	enrich := kind == queue.KindLookup || kind == queue.KindMetadata || kind == queue.KindPTRLookup

	switch {
	case base != "" && it.SHA256 != "":
		d.ViewURL = base + "/i/" + it.SHA256
	case base != "" && it.MonbooruID != 0:
		d.ViewURL = fmt.Sprintf("%s/images/%d", base, it.MonbooruID)
	case kind == queue.KindContrib && it.URL != "":
		d.ViewURL = it.URL
	case base != "" && enrich && imageID != 0:
		d.ViewURL = fmt.Sprintf("%s/images/%d", base, imageID)
	}

	switch {
	case kind == queue.KindContrib:
		// Pre-phrased by the pipeline: "tag solo", "alias a -> b".
		d.Subject = it.PostID
	case enrich:
		if id := cmp.Or(it.MonbooruID, imageID); id != 0 {
			d.Subject = "image #" + strconv.FormatInt(id, 10)
		} else {
			d.Subject = "image"
		}
	case it.PostID != "":
		d.Subject, d.SubjectURL = "post "+it.PostID, it.URL
	case it.Num != 0:
		d.Subject, d.SubjectURL = "#"+strconv.Itoa(it.Num), it.URL
	case site != "":
		d.Subject, d.SubjectURL = site, it.URL
	default:
		d.Subject, d.SubjectURL = "source", it.URL
	}

	// A direct-file send's item links its page, which nothing fetched.
	d.DebugURL = d.SubjectURL
	if pageURL, _ := ctx["PageURL"].(string); pageURL != "" && pageURL == it.URL {
		if jobURL, _ := ctx["JobURL"].(string); jobURL != "" {
			d.DebugURL = jobURL
		}
	}

	switch {
	case it.ErrorCode == outcome.ErrCodeCanceled:
		d.Outcome, d.OutcomeCls = "canceled", "o-canceled"
	case it.ErrorCode != "":
		d.Outcome, d.OutcomeCls, d.OutcomeTitle = it.ErrorCode, "o-failed", it.Error
	case it.Outcome != "":
		d.Outcome, d.OutcomeCls = string(it.Outcome), "o-"+string(it.Outcome)
	default:
		d.Outcome, d.OutcomeCls = string(it.Status), "o-"+string(it.Status)
	}

	var note []string
	if enrich && it.PostID != "" {
		note = append(note, it.PostID) // the matched source, e.g. "danbooru 95%"
	}
	if it.MergeNote != "" {
		note = append(note, it.MergeNote)
	}
	d.Note = strings.Join(note, ", ")

	return d
}

func clientMeta(j *queue.Job) string {
	var parts []string
	if len(j.Tags) > 0 {
		parts = append(parts, strings.Join(j.Tags, " "))
	}
	if j.Via != "" {
		parts = append(parts, "via "+j.Via)
	}
	if j.Source != "" {
		parts = append(parts, "source "+j.Source)
	}
	if j.Commentary != "" {
		parts = append(parts, "commentary")
	}
	return strings.Join(parts, ", ")
}
