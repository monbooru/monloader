package pipeline

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/kwdict"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/queue"
)

// processReplace downloads the post the job's URL names and pushes its file
// into the existing monbooru image the job targets - the byte-replacing
// sibling of a metadata refetch. The downloaded file is a working artifact
// of the job, never a library entry: the download bypasses the archive and
// the scratch dir is removed whatever the outcome.
func (p *Processor) processReplace(ctx context.Context, job, snap *queue.Job) error {
	job.SetItems([]queue.Item{{URL: snap.URL, Status: queue.ItemPending}})
	res, err := p.runner.Resolve(ctx, snap.URL, "", false)
	if err != nil {
		p.failEnrichFetch(ctx, job, snap, err)
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	switch {
	case len(res.Items) == 0:
		p.failEnrichFetch(ctx, job, snap, &queue.CodedError{
			Code: queue.ErrCodeDownloadFailed, Msg: "the post resolved to no downloadable file"})
		return nil
	case len(res.Items) > 1:
		// A replace targets one image, so a URL that fans out (a pool, a
		// search) cannot mean anything safe here.
		p.failEnrichFetch(ctx, job, snap, &queue.CodedError{
			Code: queue.ErrCodeUnsupportedURL, Msg: "the url names more than one post"})
		return nil
	}
	job.SetSite(res.Items[0].Category)

	workDir := filepath.Join(p.workRoot, fmt.Sprintf("job-%d", snap.ID))
	if mkErr := os.MkdirAll(workDir, 0o755); mkErr != nil {
		job.Fail(queue.ErrCodeDownloadFailed, mkErr.Error(), time.Now())
		return mkErr
	}
	defer os.RemoveAll(workDir)

	downloaded, dlErr := p.runner.Download(ctx, snap.URL, "", workDir, true, nil, false)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	w := firstWritten(downloaded)
	if w == nil {
		if dlErr == nil {
			dlErr = &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: "the post's file did not download"}
		}
		p.failEnrichFetch(ctx, job, snap, dlErr)
		return nil
	}
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemDownloaded })

	// The site's claimed md5 guards against a corrupted or swapped download:
	// bytes that don't match the claim push nothing.
	if claim := kwdict.String(w.Meta, "md5"); claim != "" {
		got, hashErr := md5OfFile(w.Path)
		if hashErr != nil {
			p.failEnrichFetch(ctx, job, snap, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: hashErr.Error()})
			return nil
		}
		if !strings.EqualFold(got, claim) {
			p.failEnrichFetch(ctx, job, snap, &queue.CodedError{
				Code: queue.ErrCodeHashMismatch,
				Msg:  "the downloaded file does not match the md5 the source claims; nothing was pushed"})
			return nil
		}
	}

	pf := p.mapper.Map(w.Meta)
	meta := monbooru.PushMeta{
		Filename:   filepath.Base(w.Path),
		Tags:       pf.Tags,
		Source:     pf.Source,
		PostID:     kwdict.ID(w.Meta),
		URL:        p.itemURL(w.Meta, snap.URL, snap.PageURL),
		Commentary: pf.Commentary,
		Original:   pf.Original,
		ParentURL:  pf.ParentURL,
		Notes:      toNoteBoxes(pf.Notes),
	}
	pushRes, err := p.client.ReplaceImageFile(ctx, snap.ImageID, w.Path, meta, snap.Gallery)
	if err != nil {
		// monbooru's own refusals (already_exists, wrong_type, a rejected
		// push) already landed on its fetch status with a richer message
		// than the code; re-reporting would overwrite it.
		switch errorCode(err) {
		case queue.ErrCodeAlreadyExists, queue.ErrCodeWrongType, queue.ErrCodeMonbooruRejected:
			failItem(job, 0, errorCode(err), err.Error())
		default:
			p.failEnrichFetch(ctx, job, snap, err)
		}
		return nil
	}
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	job.UpdateItem(0, func(it *queue.Item) {
		it.Status = queue.ItemDone
		it.Outcome = queue.OutcomeReplaced
		it.MonbooruID = snap.ImageID
		// monbooru re-keys the row to the pushed file, so the digest makes the
		// item's link a /i/<sha256> permalink; a bare /images/<id> would open
		// whichever image holds that id in the active gallery.
		it.SHA256 = pushRes.SHA256
		it.MergeNote = pushRes.MergeNote
	})
	return nil
}

// md5OfFile hashes one downloaded artifact for the pre-push claim check.
func md5OfFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
