package pipeline

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
	"github.com/monbooru/monloader/internal/shared/logx"
)

type ContribSender interface {
	HasPersonalKey() bool
	CaughtUp() bool
	RefreshAccount(ctx context.Context) (*ptr.Account, error)
	RefreshTagFilter(ctx context.Context) (*ptr.TagFilter, error)
	PostUpdate(ctx context.Context, u *ptr.C2SUpdate) error
	Contrib() *ptr.ContribStore
	HashHasIdeal(hashHex, tag string) (bool, error)
	HashHasRaw(hashHex, tag string) (bool, error)
	SiblingCurrent(bad, good string) (bool, error)
	ParentCurrent(child, parent string) (bool, error)
}

// Rows per POST, matching the hydrus client's upload cadence; a pair
// counts as one row.
const contribChunkWeight = 100

const contribPostRetries = 3

var contribRetryBackoff = 2 * time.Second

type contribChunk struct {
	update *ptr.C2SUpdate
	items  []ptr.ContribItem
}

func (p *Processor) processContrib(ctx context.Context, job, snap *queue.Job) error {
	sender, ok := p.ptr.(ContribSender)
	if !ok || sender.Contrib() == nil {
		job.Fail(outcome.ErrCodePTRUnavailable, "the ptr backend is not available", time.Now())
		return nil
	}
	store := sender.Contrib()

	var items []ptr.ContribItem
	var err error
	if snap.ContribBacklog {
		items, err = store.ClaimBacklog()
	} else {
		items, err = store.ClaimIDs(snap.ContribIDs)
	}
	if err != nil {
		job.Fail(outcome.ErrCodePTRUnavailable, "claiming staged items: "+err.Error(), time.Now())
		return nil
	}
	if len(items) == 0 {
		job.SetNote("nothing to send")
		return nil
	}
	failStore := func(msg string) {
		for _, it := range items {
			if err := store.FailItem(it.ID, msg); err != nil {
				logx.Warnf("contrib: marking item %d failed: %v", it.ID, err)
			}
		}
	}
	failAll := func(code, msg string) {
		failStore(msg)
		job.Fail(code, msg, time.Now())
	}

	if !sender.HasPersonalKey() {
		failAll(outcome.ErrCodePTRAccountRequired, "no personal account key is set")
		return nil
	}
	if !sender.CaughtUp() {
		failAll(outcome.ErrCodePTRSyncing, "the ptr index is not fully synced yet")
		return nil
	}
	acc, err := sender.RefreshAccount(ctx)
	if err != nil {
		// A cancel reads as canceled, but the claimed rows are still
		// released so none strand in 'sending'.
		if ctx.Err() != nil {
			failStore("job canceled")
			return ctx.Err()
		}
		failAll(outcome.ErrCodePTRAccountRequired, "reading the account: "+err.Error())
		return nil
	}
	if acc.Banned {
		failAll(outcome.ErrCodePTRBanned, "the account is banned: "+acc.BanReason)
		return nil
	}
	if !acc.Type.CanPetition() {
		failAll(outcome.ErrCodePTRAccountRequired, "the account lacks contribution permissions")
		return nil
	}
	filter, err := sender.RefreshTagFilter(ctx)
	if err != nil {
		if ctx.Err() != nil {
			failStore("job canceled")
			return ctx.Err()
		}
		failAll(outcome.ErrCodePTRUnavailable, "fetching the tag filter: "+err.Error())
		return nil
	}

	survivors, droppedKnown, droppedFiltered, derr := p.reDiffContrib(sender, store, items, filter)
	if derr != nil {
		failAll(outcome.ErrCodePTRUnavailable, "re-checking against the index: "+derr.Error())
		return nil
	}

	up := p.newContribUpload(job, sender, store, packContribChunks(survivors))
	sent, failedCount, uerr := up.run(ctx)
	if perr := store.PruneLog(); perr != nil {
		logx.Warnf("contrib: pruning the ledger: %v", perr)
	}
	if uerr != nil {
		return uerr
	}
	job.SetNote(contribSummary(sent, droppedKnown, droppedFiltered, failedCount))
	return nil
}

// The server's tag filter drops silently, so it is applied here where the
// drop is counted.
func (p *Processor) reDiffContrib(sender ContribSender, store *ptr.ContribStore, items []ptr.ContribItem, filter *ptr.TagFilter) (survivors []ptr.ContribItem, droppedKnown, droppedFiltered int, err error) {
	for _, it := range items {
		resolved, derr := p.contribResolved(sender, it)
		if derr != nil {
			return nil, 0, 0, derr
		}
		if resolved {
			droppedKnown++
			_ = store.Drop(it.ID)
			continue
		}
		if it.Kind == ptr.ContribMappingAdd {
			if blocked, rule := filter.Blocks(it.Tag); blocked {
				droppedFiltered++
				logx.Infof("contrib: %q dropped by the server tag filter (%s)", it.Tag, rule)
				_ = store.Drop(it.ID)
				continue
			}
		}
		survivors = append(survivors, it)
	}
	return survivors, droppedKnown, droppedFiltered, nil
}

type contribUpload struct {
	p      *Processor
	job    *queue.Job
	sender ContribSender
	store  *ptr.ContribStore
	chunks []contribChunk
	// spans[ci] is the [start, end) range of queue items chunk ci owns.
	spans [][2]int
	total int
}

func (p *Processor) newContribUpload(job *queue.Job, sender ContribSender, store *ptr.ContribStore, chunks []contribChunk) *contribUpload {
	up := &contribUpload{p: p, job: job, sender: sender, store: store, chunks: chunks, spans: make([][2]int, len(chunks))}
	base := p.cfg.Current().Monbooru.WebBase()
	var jobItems []queue.Item
	for ci, c := range chunks {
		up.spans[ci][0] = len(jobItems)
		for _, cit := range c.items {
			jobItems = append(jobItems, queue.Item{
				PostID: ptr.ContribQueueLabel(cit.Kind, cit.Tag, cit.Tag2),
				URL:    ptr.ContribItemLink(base, cit.Kind, cit.Tag, cit.Hash),
				Status: queue.ItemPending,
			})
		}
		up.spans[ci][1] = len(jobItems)
	}
	up.total = len(jobItems)
	job.SetItems(jobItems)
	return up
}

// An accepted chunk stays committed whatever happens later: the ledger
// must match what the server took.
func (up *contribUpload) run(ctx context.Context) (sent map[string]int, failed int, err error) {
	sleep := time.Duration(up.p.cfg.Current().PTR.CommitSleep * float64(time.Second))
	sent = map[string]int{}
	for i, chunk := range up.chunks {
		if i > 0 && sleep > 0 {
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				failed += up.failRemainder(i, outcome.ErrCodeCanceled, "job canceled before sending")
				return sent, failed, ctx.Err()
			}
		}
		if perr := up.p.postContribChunk(ctx, up.sender, chunk); perr != nil {
			if ctx.Err() != nil {
				failed += up.failRemainder(i, outcome.ErrCodeCanceled, "job canceled before sending")
				return sent, failed, ctx.Err()
			}
			failed += up.failRemainder(i, outcome.ErrCodePTRUnavailable, perr.Error())
			break
		}
		up.commit(i, chunk, sent)
	}
	return sent, failed, nil
}

func (up *contribUpload) commit(i int, chunk contribChunk, sent map[string]int) {
	for _, it := range chunk.items {
		if _, cerr := up.store.Complete(it); cerr != nil {
			logx.Warnf("contrib: recording item %d in the ledger: %v", it.ID, cerr)
		}
		sent[it.Kind]++
	}
	for k := up.spans[i][0]; k < up.spans[i][1]; k++ {
		up.job.UpdateItem(k, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
		up.job.UpdateItem(k, func(it *queue.Item) { it.Status = queue.ItemUploaded })
		up.job.UpdateItem(k, func(it *queue.Item) {
			it.Status = queue.ItemDone
			it.Outcome = outcome.Created
		})
	}
}

// Both a failed post and a cancel end here, so no claimed row strands in
// 'sending', where retry and rescind cannot reach it.
func (up *contribUpload) failRemainder(i int, code, reason string) int {
	failed := 0
	for _, c := range up.chunks[i:] {
		for _, it := range c.items {
			if ferr := up.store.FailItem(it.ID, reason); ferr != nil {
				logx.Warnf("contrib: marking item %d failed: %v", it.ID, ferr)
			}
			failed++
		}
	}
	for k := up.spans[i][0]; k < up.total; k++ {
		up.job.UpdateItem(k, func(it *queue.Item) {
			it.Status = queue.ItemFailed
			it.Outcome = outcome.Failed
			it.ErrorCode = code
			it.Error = reason
		})
	}
	return failed
}

func (p *Processor) contribResolved(sender ContribSender, it ptr.ContribItem) (bool, error) {
	hashHex := hex.EncodeToString(it.Hash)
	switch it.Kind {
	case ptr.ContribMappingAdd:
		return sender.HashHasIdeal(hashHex, it.Tag)
	case ptr.ContribMappingPetition:
		has, err := sender.HashHasRaw(hashHex, it.Tag)
		return !has, err
	case ptr.ContribSibling:
		if cur, err := sender.SiblingCurrent(it.Tag, it.Tag2); err != nil || cur {
			return cur, err
		}
		return sender.SiblingCurrent(it.Tag2, it.Tag)
	case ptr.ContribParent:
		if cur, err := sender.ParentCurrent(it.Tag, it.Tag2); err != nil || cur {
			return cur, err
		}
		return sender.ParentCurrent(it.Tag2, it.Tag)
	case ptr.ContribSiblingPetition:
		cur, err := sender.SiblingCurrent(it.Tag, it.Tag2)
		return !cur, err
	case ptr.ContribParentPetition:
		cur, err := sender.ParentCurrent(it.Tag, it.Tag2)
		return !cur, err
	}
	return false, fmt.Errorf("unknown contribution kind %q", it.Kind)
}

type contribUnit struct {
	kind   string
	tag    string
	tag2   string
	reason string
	hashes [][]byte
	items  []ptr.ContribItem
}

// Petitions go before pends and mapping rows group per tag, as the hydrus
// client uploads them.
func packContribChunks(items []ptr.ContribItem) []contribChunk {
	var petitions, pends []ptr.ContribItem
	for _, it := range items {
		switch it.Kind {
		case ptr.ContribMappingPetition, ptr.ContribSiblingPetition, ptr.ContribParentPetition:
			petitions = append(petitions, it)
		default:
			pends = append(pends, it)
		}
	}

	var units []*contribUnit
	mappingUnits := map[string]*contribUnit{}
	addItem := func(it ptr.ContribItem) {
		switch it.Kind {
		case ptr.ContribMappingAdd, ptr.ContribMappingPetition:
			key := it.Kind + "\x00" + it.Tag + "\x00" + it.Reason
			u, ok := mappingUnits[key]
			if !ok {
				u = &contribUnit{kind: it.Kind, tag: it.Tag, reason: it.Reason}
				mappingUnits[key] = u
				units = append(units, u)
			}
			u.hashes = append(u.hashes, it.Hash)
			u.items = append(u.items, it)
		default:
			units = append(units, &contribUnit{
				kind: it.Kind, tag: it.Tag, tag2: it.Tag2, reason: it.Reason,
				items: []ptr.ContribItem{it},
			})
		}
	}
	for _, it := range petitions {
		addItem(it)
	}
	for _, it := range pends {
		addItem(it)
	}

	var chunks []contribChunk
	cur := contribChunk{update: &ptr.C2SUpdate{}}
	weight := 0
	cut := func() {
		if !cur.update.Empty() {
			chunks = append(chunks, cur)
		}
		cur = contribChunk{update: &ptr.C2SUpdate{}}
		weight = 0
	}
	emitMapping := func(u *contribUnit, hashes [][]byte, its []ptr.ContribItem) {
		action := ptr.ActionPend
		if u.kind == ptr.ContribMappingPetition {
			action = ptr.ActionPetition
		}
		cur.update.AddMappings(action, u.tag, hashes, u.reason)
		cur.items = append(cur.items, its...)
		weight += len(hashes)
	}
	for _, u := range units {
		switch u.kind {
		case ptr.ContribMappingAdd, ptr.ContribMappingPetition:
			rest, restItems := u.hashes, u.items
			for len(rest) > 0 {
				if weight >= contribChunkWeight {
					cut()
				}
				room := contribChunkWeight - weight
				n := min(room, len(rest))
				emitMapping(u, rest[:n], restItems[:n])
				rest, restItems = rest[n:], restItems[n:]
			}
		default:
			if weight >= contribChunkWeight {
				cut()
			}
			action := ptr.ActionPend
			if u.kind == ptr.ContribSiblingPetition || u.kind == ptr.ContribParentPetition {
				action = ptr.ActionPetition
			}
			if u.kind == ptr.ContribSibling || u.kind == ptr.ContribSiblingPetition {
				cur.update.AddSibling(action, u.tag, u.tag2, u.reason)
			} else {
				cur.update.AddParent(action, u.tag, u.tag2, u.reason)
			}
			cur.items = append(cur.items, u.items...)
			weight++
		}
	}
	cut()
	return chunks
}

func (p *Processor) postContribChunk(ctx context.Context, sender ContribSender, chunk contribChunk) error {
	var err error
	for attempt := 0; attempt < contribPostRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * contribRetryBackoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err = sender.PostUpdate(ctx, chunk.update)
		if err == nil {
			return nil
		}
		if !ptr.IsRetryable(err) {
			return err
		}
	}
	return err
}

func contribSummary(sent map[string]int, droppedKnown, droppedFiltered, failed int) string {
	line := fmt.Sprintf("committed %d tag adds, %d removal petitions, %d alias and %d implication suggestions",
		sent[ptr.ContribMappingAdd], sent[ptr.ContribMappingPetition],
		sent[ptr.ContribSibling]+sent[ptr.ContribSiblingPetition],
		sent[ptr.ContribParent]+sent[ptr.ContribParentPetition])
	if droppedKnown > 0 || droppedFiltered > 0 {
		line += fmt.Sprintf("; dropped %d (already on the PTR), %d (server tag filter)", droppedKnown, droppedFiltered)
	}
	if failed > 0 {
		line += fmt.Sprintf("; %d failed, kept for retry", failed)
	}
	return line
}
