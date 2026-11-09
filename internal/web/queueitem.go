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
	Sent         string
	SentTitle    string
}

func itemView(it queue.Item, ctx map[string]any) itemDisplay {
	kind, _ := ctx["Kind"].(queue.JobKind)
	base, _ := ctx["MonbooruURL"].(string)
	imageID, _ := ctx["ImageID"].(int64)
	site, _ := ctx["Site"].(string)

	d := itemDisplay{Warnings: it.TagWarnings}
	if it.TagsSent > 0 || len(it.Held) > 0 {
		d.Sent = fmt.Sprintf("%d tags sent", it.TagsSent)
		if it.TagsSent == 1 {
			d.Sent = "1 tag sent"
		}
		if len(it.Held) > 0 {
			d.Sent += fmt.Sprintf(", %d held", len(it.Held))
			d.SentTitle = strings.Join(it.Held, ", ")
		}
	}
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
		d.Outcome, d.OutcomeCls, d.OutcomeTitle = errorLabel(it.ErrorCode), "o-failed", it.Error
	case it.Outcome != "":
		d.Outcome, d.OutcomeCls = cmp.Or(outcomeLabels[it.Outcome], string(it.Outcome)), "o-"+string(it.Outcome)
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

var outcomeLabels = map[outcome.Outcome]string{
	outcome.SkippedBlacklist:   "blacklisted",
	outcome.SkippedArchive:     "already fetched",
	outcome.SkippedUnsupported: "file type not supported",
}

var errorLabels = map[string]string{
	outcome.ErrCodeDownloadFailed:      "download failed",
	outcome.ErrCodeUnsupportedURL:      "URL not supported",
	outcome.ErrCodeAuthRequired:        "login required",
	outcome.ErrCodeBlocked:             "blocked by the site",
	outcome.ErrCodeRateLimited:         "rate limited",
	outcome.ErrCodeNetworkUnreachable:  "site unreachable",
	outcome.ErrCodeFileTooLarge:        "file too large",
	outcome.ErrCodeMappingFailed:       "no usable file",
	outcome.ErrCodeMonbooruUnreachable: "monbooru unreachable",
	outcome.ErrCodeMonbooruRejected:    "refused by monbooru",
	outcome.ErrCodeHashMismatch:        "source file changed",
	outcome.ErrCodeAlreadyExists:       "already in monbooru",
	outcome.ErrCodeWrongType:           "not an image",
	outcome.ErrCodeHashNotFound:        "no match found",
	outcome.ErrCodePTRUnavailable:      "PTR is off",
	outcome.ErrCodePTRSyncing:          "PTR still syncing",
	outcome.ErrCodePTRAccountRequired:  "PTR account needed",
	outcome.ErrCodePTRBanned:           "PTR account banned",
}

func errorLabel(code string) string {
	return cmp.Or(errorLabels[code], code)
}

// A metadata job is monbooru's refresh and a replace its upgrade, so the
// badge uses monbooru's words.
func jobKindLabel(j *queue.Job) string {
	switch j.Kind {
	case "":
		return "download"
	case queue.KindLookup:
		if j.Backend == queue.BackendAll {
			return "lookup"
		}
		return "lookup:" + j.Backend
	case queue.KindHashImport:
		return "import"
	case queue.KindContrib:
		return "contribution"
	case queue.KindPTRLookup:
		return "lookup (PTR)"
	case queue.KindReplace:
		return "upgrade"
	case queue.KindMetadata:
		return "refresh"
	}
	return string(j.Kind)
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

// Every word moreBucket can return must be listed here, or its count is
// dropped.
var moreOrder = []string{"downloading", "created", "duplicate", "enriched", "matched",
	"replaced", "skipped", "blacklisted", "failed", "canceled"}

var moreWords = map[outcome.Outcome]string{
	outcome.Created: "created", outcome.Duplicate: "duplicate", outcome.Enriched: "enriched", outcome.Matched: "matched",
	outcome.Replaced: "replaced", outcome.SkippedArchive: "skipped", outcome.SkippedUnsupported: "skipped",
	outcome.SkippedBlacklist: "blacklisted", outcome.Failed: "failed",
}

func moreBucket(it queue.Item) string {
	if it.ErrorCode == outcome.ErrCodeCanceled {
		return "canceled"
	}
	return cmp.Or(moreWords[it.Outcome], "downloading")
}

func moreSummary(items []queue.Item) string {
	n := map[string]int{}
	for _, it := range items {
		n[moreBucket(it)]++
	}
	parts := make([]string, 0, len(moreOrder))
	for _, label := range moreOrder {
		if n[label] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[label], label))
		}
	}
	return strings.Join(parts, ", ")
}

const maxQueueItems = 20
