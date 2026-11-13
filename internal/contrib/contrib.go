// Package contrib holds what may be contributed to the PTR: which spelling goes
// out, how a pair reads, what a removal may name, who may send. The HTTP surfaces
// decode the request and word the answer.
package contrib

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/ptr"
	"github.com/monbooru/monloader/internal/shared/contract"
)

type Service interface {
	CurrentReader
	Status() ptr.Status
	Enabled() bool
	HasPersonalKey() bool
	Provisional() bool
	CaughtUp() bool
	Contrib() *ptr.ContribStore
	CreateContribAccount(ctx context.Context) (string, error)
	TagFilterCached() *ptr.TagFilter
	HashHasIdeals(hashHex string, tags []string) (map[string]bool, error)
	RawTagsForHash(hashHex string) ([]string, error)
	IdealTag(tag string) (string, bool, error)
	ParentEdgeCovered(child, parent string) (bool, error)
	RefreshAccount(ctx context.Context) (*ptr.Account, error)
}

var _ Service = (*ptr.Engine)(nil)

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func ValidHash(s string) bool { return sha256HexRe.MatchString(s) }

func Preview(svc Service, hashHex string, tags, implied []string) (contract.ContribPreviewResponse, error) {
	p, err := newPreview(svc, hashHex, tags)
	if err != nil {
		return contract.ContribPreviewResponse{}, err
	}
	resp := contract.ContribPreviewResponse{
		Provisional: svc.Provisional(),
		ToAdd:       p.adds(tags),
		PTROnly:     []contract.ContribPTROnly{},
	}
	if err := p.resolveSubmitted(implied); err != nil {
		return resp, err
	}
	if resp.PTROnly, err = p.removals(); err != nil {
		return resp, err
	}
	return resp, nil
}

// StageAll stages a batch and names the rows a send should claim. A row left
// from an earlier request is a genuine resend; the same item twice in one batch
// is a duplicate.
func StageAll(svc Service, store *ptr.ContribStore, items []contract.ContribItem, origin string) (results []contract.ContribResult, accepted []int64) {
	results = make([]contract.ContribResult, 0, len(items))
	seen := map[int64]bool{}
	// Read once: it stats the index files behind the engine lock.
	covered := svc.Status().CoveredThrough
	for _, item := range items {
		result, id := stageOne(svc, store, item, origin, covered)
		if result.Result == contract.ContribStaged && id != 0 {
			if seen[id] {
				result.Result, result.Note = contract.ContribDuplicate, "an identical item is already in this batch"
			} else {
				seen[id] = true
				accepted = append(accepted, id)
			}
		}
		results = append(results, result)
	}
	return results, accepted
}

var ErrUnknownPairKind = errors.New("kind must be sibling or parent")

// PairPreview reads how a pair stands: a parent pair arrives as a=child,
// b=parent, since monbooru owns the hydrus flip.
func PairPreview(svc Service, body contract.PairPreviewRequest) (contract.PairPreviewResponse, error) {
	aMapped, bMapped := mapping.ContribTagFor(body.A), mapping.ContribTagFor(body.B)
	resp := contract.PairPreviewResponse{Provisional: svc.Provisional()}
	for _, m := range []mapping.ContribTag{aMapped, bMapped} {
		if !m.Eligible() {
			resp.Direction, resp.Note = contract.PairIneligible, m.Note
			return resp, nil
		}
	}
	if aMapped.PTR == bMapped.PTR {
		resp.Direction, resp.Note = contract.PairIneligible, selfPairNote
		return resp, nil
	}
	resp.APtr, resp.BPtr = aMapped.PTR, bMapped.PTR

	var currentFn func(a, b string) (bool, error)
	var suggestKind, petitionKind string
	switch body.Kind {
	case ptr.ContribSibling:
		suggestKind, petitionKind = ptr.ContribSibling, ptr.ContribSiblingPetition
		currentFn = svc.SiblingCurrent
	case ptr.ContribParent:
		suggestKind, petitionKind = ptr.ContribParent, ptr.ContribParentPetition
		currentFn = svc.ParentCurrent
	default:
		return resp, ErrUnknownPairKind
	}
	// Raw-first like the petition stage: a stored spelling the mapper
	// would rewrite reads as petition, not suggest, and the pending
	// lookup keys on the raws the stage uses.
	ra, rb, current, err := currentPetitionPair(currentFn, body.A, body.B)
	if err != nil {
		return resp, err
	}

	if store := svc.Contrib(); store != nil {
		pending, _ := store.PendingLog(suggestKind, aMapped.PTR, bMapped.PTR)
		if !pending && current {
			pending, _ = store.PendingLog(petitionKind, ra, rb)
		}
		if pending {
			resp.Direction = contract.PairPending
			return resp, nil
		}
	}
	if current {
		resp.Direction = contract.PairPetition
	} else if body.Kind == ptr.ContribSibling {
		ia, err := endpointIdeals(svc, body.A)
		if err != nil {
			return resp, err
		}
		ib, err := endpointIdeals(svc, body.B)
		if err != nil {
			return resp, err
		}
		_, _, reverse, err := currentPetitionPair(currentFn, body.B, body.A)
		if err != nil {
			return resp, err
		}
		switch {
		case reverse:
			resp.Direction = contract.PairConflict
		case sharesIdeal(ia, ib):
			resp.Direction = contract.PairCovered
		default:
			resp.Direction = contract.PairSuggest
		}
	} else {
		_, _, reverse, err := currentPetitionPair(currentFn, body.B, body.A)
		if err != nil {
			return resp, err
		}
		if reverse {
			resp.Direction = contract.PairConflict
		} else {
			covered, err := parentEdgeCovered(svc, body.A, body.B)
			if err != nil {
				return resp, err
			}
			if covered {
				resp.Direction = contract.PairCovered
			} else {
				resp.Direction = contract.PairSuggest
			}
		}
	}
	return resp, nil
}

func parentEdgeCovered(svc Service, a, b string) (bool, error) {
	ia, err := endpointIdeals(svc, a)
	if err != nil {
		return false, err
	}
	ib, err := endpointIdeals(svc, b)
	if err != nil {
		return false, err
	}
	for ca := range ia {
		for cb := range ib {
			covered, err := svc.ParentEdgeCovered(ca, cb)
			if err != nil {
				return false, err
			}
			if covered {
				return true, nil
			}
		}
	}
	return false, nil
}

type preview struct {
	svc       Service
	hashHex   string
	hashRaw   []byte
	filter    *ptr.TagFilter
	store     *ptr.ContribStore
	covered   int64
	rawTags   []string
	displayed map[string]bool
	// carried also holds each projection's pre-widening fold: monbooru
	// rows stored before the charset widening keep it (girls' -> girls_),
	// and the known check would offer them as new forever.
	carried         map[string]bool
	submitted       map[string]bool
	submittedPTR    []string
	submittedIdeals map[string]bool
}

func newPreview(svc Service, hashHex string, tags []string) (*preview, error) {
	p := &preview{
		svc:       svc,
		hashHex:   hashHex,
		hashRaw:   mustHex(hashHex),
		filter:    svc.TagFilterCached(),
		store:     svc.Contrib(),
		covered:   svc.Status().CoveredThrough,
		carried:   map[string]bool{},
		submitted: map[string]bool{},
	}
	var err error
	if p.rawTags, err = svc.RawTagsForHash(hashHex); err != nil {
		return nil, err
	}
	if p.displayed, err = svc.HashHasIdeals(hashHex, ptrForms(tags)); err != nil {
		return nil, err
	}
	for _, raw := range p.rawTags {
		if mb := mapping.MapPTRTag(raw); mb != "" {
			p.carried[mb] = true
			p.carried[mapping.LegacyFoldTag(mb)] = true
		}
	}
	return p, nil
}

// adds records each tag's PTR spelling, so it runs before the removal pass.
func (p *preview) adds(tags []string) []contract.ContribToAdd {
	out := make([]contract.ContribToAdd, 0, len(tags))
	for _, tag := range tags {
		p.submitted[tag] = true
		entry := contract.ContribToAdd{Tag: tag}
		mapped := mapping.ContribTagFor(tag)
		if !mapped.Eligible() {
			entry.Status, entry.Note = contract.PreviewIneligible, mapped.Note
			out = append(out, entry)
			continue
		}
		entry.PTR = mapped.PTR
		p.submittedPTR = append(p.submittedPTR, mapped.PTR)
		if blocked, rule := p.filter.Blocks(mapped.PTR); blocked {
			entry.Status, entry.Note = contract.PreviewFiltered, "blocked by the PTR tag filter ("+rule+")"
			out = append(out, entry)
			continue
		}
		if p.store != nil {
			if waiting, err := p.store.UnsentByKindTag(ptr.ContribMappingAdd, mapped.PTR, p.hashRaw); err == nil && waiting {
				entry.Status = contract.PreviewUnsent
				out = append(out, entry)
				continue
			}
		}
		known, _ := mappingAddKnown(p.svc, p.displayed[mapped.PTR], mapped.PTR, p.hashRaw, p.covered)
		if known || p.carried[tag] {
			entry.Status = contract.PreviewKnown
		} else if p.store != nil && notTaken(p.store, mapped.PTR, p.hashRaw) {
			entry.Status, entry.Note = contract.PreviewIneligible, notTakenNote
		} else {
			entry.Status = contract.PreviewNew
			_, exists, err := p.svc.IdealTag(mapped.PTR)
			entry.UnknownTag = err == nil && !exists
		}
		out = append(out, entry)
	}
	return out
}

// The removal diff compares on ideals: a raw spelling of a tag the caller
// shows must not read as extra, or its petition would strip a right mapping.
func (p *preview) resolveSubmitted(implied []string) error {
	for _, tag := range implied {
		p.submitted[tag] = true
		if mapped := mapping.ContribTagFor(tag); mapped.Eligible() {
			p.submittedPTR = append(p.submittedPTR, mapped.PTR)
		}
	}
	p.submittedIdeals = map[string]bool{}
	for _, ptrTag := range p.submittedPTR {
		ideal, ok, err := p.svc.IdealTag(ptrTag)
		if err != nil {
			return err
		}
		if ok {
			p.submittedIdeals[ideal] = true
		}
	}
	return nil
}

func (p *preview) removals() ([]contract.ContribPTROnly, error) {
	idealOfRaw := make(map[string]string, len(p.rawTags))
	idealCarriers := map[string]int{}
	for _, raw := range p.rawTags {
		ideal, ok, err := p.svc.IdealTag(raw)
		if err != nil {
			return nil, err
		}
		if ok {
			idealOfRaw[raw] = ideal
			// Only the canonical raw counts: two bad spellings of an
			// absent canonical must not suppress each other, since
			// removing both is the fix.
			if raw == ideal {
				idealCarriers[ideal]++
			}
		}
	}

	out := []contract.ContribPTROnly{}
	for _, raw := range p.rawTags {
		mb := mapping.MapPTRTag(raw)
		if mb == "" || p.submitted[mb] || p.submitted[mapping.LegacyFoldTag(mb)] {
			continue
		}
		// The add side refuses these categories, so the petition side
		// must not offer them either.
		if mapping.ContribDenied(mb) {
			continue
		}
		if ideal, ok := idealOfRaw[raw]; ok && p.suppressedRemoval(ideal, raw, idealCarriers) {
			continue
		}
		petitionable := true
		if p.store != nil {
			if pending, err := p.store.PendingMappingPetition(raw, p.hashRaw); err == nil && pending {
				petitionable = false
			}
		}
		out = append(out, contract.ContribPTROnly{Tag: mb, PTR: raw, Petitionable: petitionable})
	}
	return out, nil
}

func (p *preview) suppressedRemoval(ideal, raw string, idealCarriers map[string]int) bool {
	switch {
	// A pull never applies a mapping monbooru cannot represent, so there
	// is no local judgement to petition it against.
	case mapping.MapPTRTag(ideal) == "":
		return true
	// A bad sibling whose canonical the file also carries shows no tag of
	// its own: petitioning it is noise.
	case ideal != raw && idealCarriers[ideal] > 0:
		return true
	case p.submittedIdeals[ideal]:
		return true
	}
	// submittedIdeals reverses through ContribTagFor, which is lossy for
	// quoted or namespace-lifted spellings, so the forward mapping a pull
	// stores is checked too.
	mi := mapping.MapPTRTag(ideal)
	return mi != "" && (p.submitted[mi] || p.submitted[mapping.LegacyFoldTag(mi)])
}

func ptrForms(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if mapped := mapping.ContribTagFor(tag); mapped.Eligible() {
			out = append(out, mapped.PTR)
		}
	}
	return out
}

func mustHex(h string) []byte {
	raw, _ := hex.DecodeString(h)
	return raw
}

const fixedRescindReason = "rescinding my own earlier add"

const notTakenNote = "the repository did not take this add earlier"

const selfPairNote = "both sides are the same tag"

func SendRefusal(ctx context.Context, svc Service) *Refusal {
	if !svc.Enabled() {
		return &Refusal{Code: outcome.ErrCodePTRUnavailable, Msg: "the ptr index is not available"}
	}
	if !svc.HasPersonalKey() {
		return &Refusal{Code: outcome.ErrCodePTRAccountRequired, Msg: "no personal account key is set"}
	}
	// Fresh, so a ban the janitors just set refuses the send now; bounded
	// well inside contract.ContribSendBudget.
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	banned := false
	if acc, err := svc.RefreshAccount(rctx); err == nil {
		banned = acc.Banned
	} else if c := svc.Status().Contrib; c != nil {
		banned = c.Banned
	}
	if banned {
		return &Refusal{Code: outcome.ErrCodePTRBanned, Msg: "the account is banned"}
	}
	if !svc.CaughtUp() {
		return &Refusal{Code: outcome.ErrCodePTRSyncing, Msg: "the ptr index is not fully synced yet"}
	}
	return nil
}

func stageOne(svc Service, store *ptr.ContribStore, item contract.ContribItem, origin string, covered int64) (contract.ContribResult, int64) {
	s := &stager{
		svc: svc, store: store, item: item, origin: origin,
		reason: strings.TrimSpace(item.Reason),
		res:    contract.ContribResult{Kind: item.Kind},
	}
	if item.Kind != ptr.ContribMappingAdd && s.reason == "" {
		return s.fail(contract.ContribInvalidReason, "a reason is required")
	}
	switch item.Kind {
	case ptr.ContribMappingAdd, ptr.ContribMappingPetition:
		return s.stageMapping(covered)
	case ptr.ContribSibling:
		return s.stageRelation(svc.SiblingCurrent, "alias", "bad", "good", item.Bad, item.Good)
	case ptr.ContribSiblingPetition:
		return s.stagePetition(svc.SiblingCurrent, "alias", item.Bad, item.Good)
	case ptr.ContribParent:
		return s.stageRelation(svc.ParentCurrent, "implication", "child", "parent", item.Child, item.Parent)
	case ptr.ContribParentPetition:
		return s.stagePetition(svc.ParentCurrent, "implication", item.Child, item.Parent)
	}
	return s.fail(contract.ContribIneligible, "unknown kind "+item.Kind)
}

type stager struct {
	svc    Service
	store  *ptr.ContribStore
	item   contract.ContribItem
	origin string
	reason string
	res    contract.ContribResult
}

func (s *stager) fail(result, note string) (contract.ContribResult, int64) {
	s.res.Result, s.res.Note = result, note
	return s.res, 0
}

func (s *stager) stage(it ptr.ContribItem) (contract.ContribResult, int64) {
	it.Origin = s.origin
	it.Reason = s.reason
	id, dup, err := s.store.Stage(it)
	if err != nil {
		return s.fail(contract.ContribIneligible, "staging failed: "+err.Error())
	}
	if dup {
		// Re-confirming an unsent row resends it rather than reporting a no-op.
		if existing, ok, _ := s.store.StagedID(it.Kind, it.Tag, it.Tag2, it.Hash); ok {
			if err := s.store.SetReason(existing, s.reason); err != nil {
				return s.fail(contract.ContribIneligible, "staging failed: "+err.Error())
			}
			s.res.Result = contract.ContribStaged
			return s.res, existing
		}
		return s.fail(contract.ContribDuplicate, "an identical item already waits unsent")
	}
	s.res.Result = contract.ContribStaged
	return s.res, id
}

func (s *stager) stageMapping(covered int64) (contract.ContribResult, int64) {
	if !sha256HexRe.MatchString(s.item.SHA256) {
		return s.fail(contract.ContribIneligible, "sha256 must be 64 lowercase hex characters")
	}
	hash := mustHex(s.item.SHA256)
	if s.item.Kind == ptr.ContribMappingAdd {
		mapped := mapping.ContribTagFor(s.item.Tag)
		if !mapped.Eligible() {
			return s.fail(contract.ContribIneligible, mapped.Note)
		}
		displayed, err := s.svc.HashHasIdeal(s.item.SHA256, mapped.PTR)
		if err != nil {
			return s.fail(contract.ContribIneligible, err.Error())
		}
		if known, note := mappingAddKnown(s.svc, displayed, mapped.PTR, hash, covered); known {
			return s.fail(contract.ContribAlreadyKnown, note)
		}
		if notTaken(s.store, mapped.PTR, hash) {
			return s.fail(contract.ContribIneligible, notTakenNote)
		}
		return s.stage(ptr.ContribItem{Kind: s.item.Kind, Tag: mapped.PTR, Hash: hash})
	}
	// The verbatim spelling first: a stored raw whose namespace or
	// underscores the outbound mapper would rewrite stays petitionable.
	tag := s.item.Tag
	current, err := s.svc.HashHasRaw(s.item.SHA256, tag)
	if err != nil {
		return s.fail(contract.ContribIneligible, err.Error())
	}
	if !current {
		mapped := mapping.ContribTagFor(s.item.Tag)
		if !mapped.Eligible() {
			return s.fail(contract.ContribIneligible, mapped.Note)
		}
		tag = mapped.PTR
		if current, err = s.svc.HashHasRaw(s.item.SHA256, tag); err != nil {
			return s.fail(contract.ContribIneligible, err.Error())
		}
		if !current {
			return s.fail(contract.ContribNotOnPTR, "the PTR does not hold this exact mapping")
		}
	}
	if pending, err := s.store.PendingMappingPetition(tag, hash); err == nil && pending {
		return s.fail(contract.ContribAlreadySuggested, "a removal for this mapping is already staged or awaiting review")
	}
	return s.stage(ptr.ContribItem{Kind: s.item.Kind, Tag: tag, Hash: hash})
}

func (s *stager) stagePetition(current func(a, b string) (bool, error), noun, a, b string) (contract.ContribResult, int64) {
	ra, rb, ok, err := currentPetitionPair(current, a, b)
	if err != nil {
		return s.fail(contract.ContribIneligible, err.Error())
	}
	if !ok {
		return s.fail(contract.ContribNotOnPTR, "the PTR does not hold this "+noun)
	}
	if pending, err := s.store.PendingLog(s.item.Kind, ra, rb); err == nil && pending {
		return s.fail(contract.ContribAlreadySuggested, "already sent and awaiting janitor review")
	}
	return s.stage(ptr.ContribItem{Kind: s.item.Kind, Tag: ra, Tag2: rb})
}

func (s *stager) stageRelation(current func(a, b string) (bool, error), noun, aLabel, bLabel, a, b string) (contract.ContribResult, int64) {
	aMapped, bMapped := mapping.ContribTagFor(a), mapping.ContribTagFor(b)
	if !aMapped.Eligible() {
		return s.fail(contract.ContribIneligible, aLabel+": "+aMapped.Note)
	}
	if !bMapped.Eligible() {
		return s.fail(contract.ContribIneligible, bLabel+": "+bMapped.Note)
	}
	if aMapped.PTR == bMapped.PTR {
		return s.fail(contract.ContribIneligible, selfPairNote)
	}
	_, _, cur, err := currentPetitionPair(current, a, b)
	if err != nil {
		return s.fail(contract.ContribIneligible, err.Error())
	}
	if cur {
		return s.fail(contract.ContribAlreadyKnown, "the PTR already has this "+noun)
	}
	_, _, reverse, err := currentPetitionPair(current, b, a)
	if err != nil {
		return s.fail(contract.ContribIneligible, err.Error())
	}
	if reverse {
		return s.fail(contract.ContribConflict, "the PTR holds this "+noun+" in the opposite direction")
	}
	if pending, err := s.store.PendingLog(s.item.Kind, aMapped.PTR, bMapped.PTR); err == nil && pending {
		return s.fail(contract.ContribAlreadySuggested, "already sent and awaiting janitor review")
	}
	return s.stage(ptr.ContribItem{Kind: s.item.Kind, Tag: aMapped.PTR, Tag2: bMapped.PTR})
}

func mappingAddKnown(svc Service, displayed bool, ptrTag string, hash []byte, covered int64) (bool, string) {
	if displayed {
		return true, "the PTR already has this tag for the file"
	}
	if store := svc.Contrib(); store != nil {
		if committed, err := store.MappingAddCommittedSince(ptrTag, hash, covered); err == nil && committed {
			return true, "sent earlier and not yet in the synced index"
		}
	}
	return false, ""
}

func notTaken(store *ptr.ContribStore, ptrTag string, hash []byte) bool {
	dropped, err := store.MappingAddNotTaken(ptrTag, hash)
	return err == nil && dropped
}

func currentPetitionPair(current func(a, b string) (bool, error), a, b string) (ra, rb string, ok bool, err error) {
	for _, x := range mapping.PTRNamespacesFor(a) {
		for _, y := range mapping.PTRNamespacesFor(b) {
			cur, err := current(x, y)
			if err != nil {
				return "", "", false, err
			}
			if cur {
				return x, y, true, nil
			}
		}
	}
	return "", "", false, nil
}

// Every spelling's ideal, not the first hit: a verbatim underscore form
// can be an orphan while the space-joined form carries the pair.
func endpointIdeals(svc Service, t string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, f := range mapping.PTRNamespacesFor(t) {
		ideal, ok, err := svc.IdealTag(f)
		if err != nil {
			return nil, err
		}
		if ok {
			out[ideal] = true
		}
	}
	return out, nil
}

func sharesIdeal(a, b map[string]bool) bool {
	for ideal := range a {
		if b[ideal] {
			return true
		}
	}
	return false
}

type RefusalKind int

// A Refused refusal carries its own Code; the others are the api's words.
const (
	Refused RefusalKind = iota
	NotFound
	NotRescindable
	Internal
)

type Refusal struct {
	Kind RefusalKind
	Code string
	Msg  string
}

// RescindCommittedAdd stages the removal petition; the caller must send the
// returned item. The ledger row reads rescinded once that send lands.
func RescindCommittedAdd(svc Service, store *ptr.ContribStore, logID int64) (int64, *Refusal) {
	row, err := store.LogRow(logID)
	if err != nil {
		return 0, &Refusal{Kind: Internal, Msg: err.Error()}
	}
	if row == nil {
		return 0, &Refusal{Kind: NotFound, Msg: "no ledger row with that id"}
	}
	if row.Kind != ptr.ContribMappingAdd {
		return 0, &Refusal{Kind: NotRescindable, Msg: "only a committed mapping add can be rescinded"}
	}
	// Not the stamped outcome: after an earlier rescind's petition failed
	// and was cleared, the add must stay rescindable.
	if pending, _ := store.PendingMappingPetition(row.Tag, row.Hash); pending {
		return 0, &Refusal{Kind: NotRescindable, Msg: "a removal for this add is already staged or awaiting review"}
	}
	current, err := svc.HashHasRaw(hex.EncodeToString(row.Hash), row.Tag)
	if err != nil {
		return 0, &Refusal{Code: outcome.ErrCodePTRUnavailable, Msg: err.Error()}
	}
	if !current {
		// An add committed after the coverage window is live server-side
		// but not yet in the index.
		committed, _ := store.MappingAddCommittedSince(row.Tag, row.Hash, svc.Status().CoveredThrough)
		if !committed {
			return 0, &Refusal{Kind: NotRescindable, Msg: "the mapping is already gone from the index"}
		}
	}
	itemID, dup, err := store.Stage(ptr.ContribItem{
		Kind: ptr.ContribMappingPetition, Tag: row.Tag, Hash: row.Hash, Reason: fixedRescindReason,
	})
	if err != nil {
		return 0, &Refusal{Kind: Internal, Msg: err.Error()}
	}
	if dup {
		if existing, ok, _ := store.StagedID(ptr.ContribMappingPetition, row.Tag, "", row.Hash); ok {
			itemID = existing
		}
	}
	return itemID, nil
}

type CurrentReader interface {
	HashHasIdeal(hashHex, tag string) (bool, error)
	HashHasRaw(hashHex, tag string) (bool, error)
	SiblingCurrent(bad, good string) (bool, error)
	ParentCurrent(child, parent string) (bool, error)
}

// Resolved differs from staging on purpose: a logged add past the coverage window
// is not known here, and a pair held in reverse is a drop rather than a conflict,
// since the row is already the operator's and the index may have moved under it.
func Resolved(sender CurrentReader, store *ptr.ContribStore, it ptr.ContribItem, covered int64) (bool, error) {
	hashHex := hex.EncodeToString(it.Hash)
	switch it.Kind {
	case ptr.ContribMappingAdd:
		return sender.HashHasIdeal(hashHex, it.Tag)
	case ptr.ContribMappingPetition:
		has, err := sender.HashHasRaw(hashHex, it.Tag)
		if err != nil || has {
			return false, err
		}
		// An add committed since the index's coverage is live on the
		// server already, so its rescind still has work to do.
		live, err := store.MappingAddCommittedSince(it.Tag, it.Hash, covered)
		return !live, err
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

type AccountFailure struct {
	Kind AccountFailureKind
	Msg  string
}

type AccountFailureKind int

const (
	AccountVelocity AccountFailureKind = iota
	AccountExists
	AccountRepository
	AccountNotSaved
)

// CreateAccount redeems a new account and saves its key. The redemption is
// single-use, so a failed save leaves the key only in memory, which the message
// tells the operator to back up.
func CreateAccount(ctx context.Context, svc Service, save func(key string) error) *AccountFailure {
	key, err := svc.CreateContribAccount(ctx)
	if err != nil {
		var serr *ptr.ServerError
		switch {
		// The repository's velocity refusal.
		case errors.As(err, &serr) && serr.Status == 400:
			return &AccountFailure{AccountVelocity, "The repository is not taking new accounts right now, try again later."}
		case errors.Is(err, ptr.ErrAccountExists):
			return &AccountFailure{AccountExists, err.Error()}
		}
		return &AccountFailure{AccountRepository, err.Error()}
	}
	if err := save(key); err != nil {
		return &AccountFailure{AccountNotSaved, "account created but saving the key failed: " + err.Error() + " - reveal and back it up from the ptr page before restarting"}
	}
	return nil
}
