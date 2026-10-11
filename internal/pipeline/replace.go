package pipeline

import (
	"cmp"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/kwdict"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/queue"
)

// The file is a working artifact, never a library entry, so the download
// bypasses the archive.
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
		p.failEnrichFetch(ctx, job, snap, &outcome.CodedError{
			Code: outcome.ErrCodeDownloadFailed, Msg: "the post resolved to no downloadable file"})
		return nil
	case len(res.Items) > 1:
		p.failEnrichFetch(ctx, job, snap, &outcome.CodedError{
			Code: outcome.ErrCodeUnsupportedURL, Msg: "the url names more than one post"})
		return nil
	}
	job.SetSite(res.Items[0].Category)

	workDir := filepath.Join(p.workRoot, fmt.Sprintf("job-%d", snap.ID))
	if mkErr := os.MkdirAll(workDir, 0o755); mkErr != nil {
		job.Fail(outcome.ErrCodeDownloadFailed, mkErr.Error(), time.Now())
		return mkErr
	}
	defer os.RemoveAll(workDir)

	downloaded, dlErr := p.runner.Download(ctx, snap.URL, "", workDir, gdl.ArchiveBypass, nil, false)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	w := firstWritten(downloaded)
	if w == nil {
		dlErr = cmp.Or(dlErr, entryErr(downloaded))
		if dlErr == nil {
			dlErr = &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: "the post's file did not download"}
		}
		p.failEnrichFetch(ctx, job, snap, dlErr)
		return nil
	}
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemDownloaded })

	// The site's md5 claim guards against a corrupted or swapped download.
	if claim := kwdict.String(w.Meta, "md5"); claim != "" {
		got, hashErr := md5OfFile(w.Path)
		if hashErr != nil {
			p.failEnrichFetch(ctx, job, snap, &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: hashErr.Error()})
			return nil
		}
		if !strings.EqualFold(got, claim) {
			p.failEnrichFetch(ctx, job, snap, &outcome.CodedError{
				Code: outcome.ErrCodeHashMismatch,
				Msg:  "the downloaded file does not match the md5 the source claims; nothing was pushed"})
			return nil
		}
	}

	pf := p.mapper.Map(w.Meta)
	meta := pushMetaFrom(pf, w.Path, p.itemURL(w.Meta, snap.URL, snap.PageURL), kwdict.ID(w.Meta))
	pushRes, err := p.client.ReplaceImageFile(ctx, snap.ImageID, w.Path, meta, snap.Gallery)
	if err != nil {
		// monbooru already recorded these refusals on its fetch status
		// with a richer message; a re-report would overwrite it.
		switch errorCode(err) {
		case outcome.ErrCodeAlreadyExists, outcome.ErrCodeWrongType, outcome.ErrCodeMonbooruRejected:
			failItem(job, 0, errorCode(err), err.Error())
		default:
			p.failEnrichFetch(ctx, job, snap, err)
		}
		return nil
	}
	job.UpdateItem(0, func(it *queue.Item) { it.Status = queue.ItemUploaded })
	job.UpdateItem(0, func(it *queue.Item) {
		it.Status = queue.ItemDone
		it.Outcome = outcome.Replaced
		it.MonbooruID = snap.ImageID
		// monbooru re-keys the image to the pushed file, so the link
		// needs its digest.
		it.SHA256 = pushRes.SHA256
		it.MergeNote = pushRes.MergeNote
	})
	return nil
}

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
