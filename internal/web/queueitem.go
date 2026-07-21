package web

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/monbooru/monloader/internal/queue"
)

// itemDisplay is one queue item shaped for its row: a plain-language subject
// that names what it is, the outcome (or the error code on failure) with its
// detail note, and a single monbooru link. Building it in Go keeps the three
// job kinds - download, enrich, contribution - reading identically in the row.
type itemDisplay struct {
	Subject      string
	SubjectURL   string // source link for a download; empty otherwise
	Outcome      string // outcome word, or the error code on failure
	OutcomeCls   string
	OutcomeTitle string // the full error on hover
	Note         string // parenthesised detail (matched source, merged tags)
	ViewURL      string // the monbooru link, or empty
	Warnings     []string
}

// itemView derives the row display for one item. ctx carries the job-level
// fields the template threads in (Kind, MonbooruURL, ImageID, Site); missing
// keys default, so a bare render still works.
func itemView(it queue.Item, ctx map[string]any) itemDisplay {
	kind, _ := ctx["Kind"].(queue.JobKind)
	base, _ := ctx["MonbooruURL"].(string)
	imageID, _ := ctx["ImageID"].(int64)
	site, _ := ctx["Site"].(string)

	d := itemDisplay{Warnings: it.TagWarnings}
	enrich := kind == queue.KindLookup || kind == queue.KindMetadata

	switch {
	case it.SHA256 != "":
		d.ViewURL = base + "/i/" + it.SHA256
	case it.MonbooruID != 0:
		d.ViewURL = fmt.Sprintf("%s/images/%d", base, it.MonbooruID)
	case kind == queue.KindContrib && it.URL != "":
		d.ViewURL = it.URL
	case enrich && imageID != 0:
		d.ViewURL = fmt.Sprintf("%s/images/%d", base, imageID)
	}

	switch {
	case kind == queue.KindContrib:
		// Pre-phrased by the pipeline: "tag solo", "alias a -> b".
		d.Subject = it.PostID
	case enrich:
		if id := enrichImageID(it, imageID); id != 0 {
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

	switch {
	case it.ErrorCode == queue.ErrCodeCanceled:
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

// enrichImageID is the monbooru image an enrich item touched: the one it landed
// on, or the job's target when it did not land (a miss still links home).
func enrichImageID(it queue.Item, jobImageID int64) int64 {
	if it.MonbooruID != 0 {
		return it.MonbooruID
	}
	return jobImageID
}
