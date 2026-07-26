package pipeline

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/monbooru/monloader/internal/logx"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/queue"
)

// ContribSender is the engine surface a contribution send drives. The
// live *ptr.Engine satisfies it; the plain PTRService stays narrow for
// the runs without contributions.
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

// contribChunkWeight caps mapping-row-equivalents per POST, matching
// the hydrus client's upload cadence; a pair counts as one row.
const contribChunkWeight = 100

// contribPostRetries bounds per-chunk retries on a transient refusal
// before the remainder is kept for a manual retry; the backoff is a var
// so tests can shorten it.
const contribPostRetries = 3

var contribRetryBackoff = 2 * time.Second

// contribChunk is one POST: the update and the store rows it commits.
type contribChunk struct {
	update *ptr.C2SUpdate
	items  []ptr.ContribItem
}

// processContrib runs one contribution send: claim, account and filter
// checks, re-diff, package, and the chunked upload. Chunks the server
// accepted stay committed whatever happens later, so the ledger
// reflects exactly what the server took.
func (p *Processor) processContrib(ctx context.Context, job, snap *queue.Job) error {
	sender, ok := p.ptr.(ContribSender)
	if !ok || sender.Contrib() == nil {
		job.Fail(queue.ErrCodePTRUnavailable, "the ptr backend is not available", time.Now())
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
		job.Fail(queue.ErrCodePTRUnavailable, "claiming staged items: "+err.Error(), time.Now())
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
		failAll(queue.ErrCodePTRAccountRequired, "no personal account key is set")
		return nil
	}
	if !sender.CaughtUp() {
		failAll(queue.ErrCodePTRSyncing, "the ptr index is not fully synced yet")
		return nil
	}
	acc, err := sender.RefreshAccount(ctx)
	if err != nil {
		// A cancel mid-call must read as canceled, not as the call's error;
		// the claimed rows are still released so none strand in 'sending'.
		if ctx.Err() != nil {
			failStore("job canceled")
			return ctx.Err()
		}
		failAll(queue.ErrCodePTRAccountRequired, "reading the account: "+err.Error())
		return nil
	}
	if acc.Banned {
		failAll(queue.ErrCodePTRBanned, "the account is banned: "+acc.BanReason)
		return nil
	}
	if !acc.Type.CanPetition() {
		failAll(queue.ErrCodePTRAccountRequired, "the account lacks contribution permissions")
		return nil
	}
	filter, err := sender.RefreshTagFilter(ctx)
	if err != nil {
		if ctx.Err() != nil {
			failStore("job canceled")
			return ctx.Err()
		}
		failAll(queue.ErrCodePTRUnavailable, "fetching the tag filter: "+err.Error())
		return nil
	}

	// Re-run the staging checks against the now-current index; items the
	// world already resolved drop as local no-ops, and the server's
	// silent tag filter is applied loudly here instead.
	var survivors []ptr.ContribItem
	droppedKnown, droppedFiltered := 0, 0
	for _, it := range items {
		resolved, derr := p.contribResolved(sender, it)
		if derr != nil {
			failAll(queue.ErrCodePTRUnavailable, "re-checking against the index: "+derr.Error())
			return nil
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

	chunks := packContribChunks(survivors)
	// One queue item per contribution, linked to the monbooru image (mapping)
	// or tag (pair), with the chunk each item belongs to remembered so a chunk
	// outcome marks exactly its items. spans[ci] is the [start, end) range.
	base := p.cfg.Current().Monbooru.WebBase()
	var jobItems []queue.Item
	spans := make([][2]int, len(chunks))
	for ci, c := range chunks {
		spans[ci][0] = len(jobItems)
		for _, cit := range c.items {
			jobItems = append(jobItems, queue.Item{
				PostID: ptr.ContribQueueLabel(cit.Kind, cit.Tag, cit.Tag2),
				URL:    ptr.ContribItemLink(base, cit.Kind, cit.Tag, cit.Hash),
				Status: queue.ItemPending,
			})
		}
		spans[ci][1] = len(jobItems)
	}
	job.SetItems(jobItems)

	sleep := time.Duration(p.cfg.Current().PTR.CommitSleep * float64(time.Second))
	sent := map[string]int{}
	failedCount := 0
	// Everything from chunk i onward stays for the manual retry; the accepted
	// chunks before it are already in the ledger. Used on a post failure and
	// on cancellation, so claimed rows never strand in 'sending'.
	failRemainder := func(i int, code, reason string) {
		for _, c := range chunks[i:] {
			for _, it := range c.items {
				if ferr := store.FailItem(it.ID, reason); ferr != nil {
					logx.Warnf("contrib: marking item %d failed: %v", it.ID, ferr)
				}
				failedCount++
			}
		}
		for k := spans[i][0]; k < len(jobItems); k++ {
			job.UpdateItem(k, func(it *queue.Item) {
				it.Status = queue.ItemFailed
				it.Outcome = queue.OutcomeFailed
				it.ErrorCode = code
				it.Error = reason
			})
		}
	}
	for i, chunk := range chunks {
		if i > 0 && sleep > 0 {
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				failRemainder(i, queue.ErrCodeCanceled, "job canceled before sending")
				return ctx.Err()
			}
		}
		err := p.postContribChunk(ctx, sender, chunk)
		if err != nil {
			if ctx.Err() != nil {
				failRemainder(i, queue.ErrCodeCanceled, "job canceled before sending")
				return ctx.Err()
			}
			failRemainder(i, queue.ErrCodePTRUnavailable, err.Error())
			break
		}
		for _, it := range chunk.items {
			if _, cerr := store.Complete(it); cerr != nil {
				logx.Warnf("contrib: recording item %d in the ledger: %v", it.ID, cerr)
			}
			sent[it.Kind]++
		}
		for k := spans[i][0]; k < spans[i][1]; k++ {
			job.UpdateItem(k, func(it *queue.Item) { it.Status = queue.ItemDownloaded })
			job.UpdateItem(k, func(it *queue.Item) { it.Status = queue.ItemUploaded })
			job.UpdateItem(k, func(it *queue.Item) {
				it.Status = queue.ItemDone
				it.Outcome = queue.OutcomeCreated
			})
		}
	}

	job.SetNote(contribSummary(sent, droppedKnown, droppedFiltered, failedCount))
	return nil
}

// contribResolved re-runs one item's staging predicate: true when the
// world already resolved it and the item commits as a local no-op.
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

// contribUnit is one packable piece: a pair, or one tag's mapping rows
// merged into a single content entry (grouped by kind, tag, and reason
// the way the hydrus client uploads them).
type contribUnit struct {
	kind   string
	tag    string
	tag2   string
	reason string
	hashes [][]byte
	items  []ptr.ContribItem
}

// packContribChunks orders petitions before pends (the hydrus
// ordering), groups mapping rows per tag, and cuts chunks at the
// per-POST weight; a mapping group larger than the remaining room
// splits across chunks.
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

// postContribChunk uploads one chunk with bounded retries on transient
// refusals (509, network, 5xx).
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

// contribSummary renders the job's one-line result.
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
