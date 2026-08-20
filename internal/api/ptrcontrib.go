package api

import (
	"context"
	"encoding/hex"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/ptr"
)

// sha256HexRe validates the hash the contribution endpoints key on.
var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// contribToAdd is one submitted tag's verdict in the preview.
type contribToAdd struct {
	Tag string `json:"tag"`
	PTR string `json:"ptr"`
	// Status: new (would be sent), known (the index has it,
	// ideal-compared), ineligible (unmappable or invalid, note says
	// why), filtered (the cached server tag filter blocks it), unsent
	// (already staged in monloader awaiting a send).
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
	// UnknownTag marks a new row whose PTR spelling is not a tag the index
	// holds at all: sending it creates the tag, not just the mapping.
	UnknownTag bool `json:"unknown_tag,omitempty"`
}

// contribPTROnly is one PTR-current tag the submitted list lacks - a
// removal-petition candidate.
type contribPTROnly struct {
	Tag          string `json:"tag"`
	PTR          string `json:"ptr"`
	Petitionable bool   `json:"petitionable"`
}

type contribPreviewResponse struct {
	Provisional bool             `json:"provisional"`
	ToAdd       []contribToAdd   `json:"to_add"`
	PTROnly     []contribPTROnly `json:"ptr_only"`
}

// contribGate answers the common refusals; ok=false means a response
// was already written.
func (h *Handler) contribGate(w http.ResponseWriter) bool {
	if h.ptr == nil || !h.ptr.Enabled() {
		apiError(w, http.StatusConflict, "ptr_unavailable", "the ptr index is not available")
		return false
	}
	return true
}

// ptrContribPreview handles POST /api/v1/ptr/contrib/preview: the
// two-way diff for one file. Tags arrive in monbooru form; the answer
// carries the exact PTR spelling a send would use so the operator sees
// it before deciding.
func (h *Handler) ptrContribPreview(w http.ResponseWriter, r *http.Request) {
	if !h.contribGate(w) {
		return
	}
	var body struct {
		SHA256 string   `json:"sha256"`
		Tags   []string `json:"tags"`
		// Implied is context only: tags the caller shows through its own
		// implications. They are never offered as adds, but they suppress
		// removal petitions the same way submitted tags do.
		Implied []string `json:"implied"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if !sha256HexRe.MatchString(body.SHA256) {
		apiError(w, http.StatusBadRequest, "invalid_request", "sha256 must be 64 lowercase hex characters")
		return
	}

	p, err := h.newContribPreview(body.SHA256, body.Tags)
	if err != nil {
		apiError(w, http.StatusConflict, "ptr_unavailable", err.Error())
		return
	}
	resp := contribPreviewResponse{
		Provisional: h.ptr.Provisional(),
		ToAdd:       p.adds(body.Tags),
		PTROnly:     []contribPTROnly{},
	}
	if err := p.resolveSubmitted(body.Implied); err != nil {
		apiError(w, http.StatusConflict, "ptr_unavailable", err.Error())
		return
	}
	if resp.PTROnly, err = p.removals(); err != nil {
		apiError(w, http.StatusConflict, "ptr_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// contribPreview is one preview request's working set: the file's hash, the
// caches both passes read, and the views of the submitted list that decide
// which of the file's PTR tags are removal candidates.
type contribPreview struct {
	h       *Handler
	hashHex string
	hashRaw []byte
	filter  *ptr.TagFilter
	store   *ptr.ContribStore
	covered int64
	// rawTags are the file's current raw PTR mappings; displayed is the
	// display-view answer for the submitted list, derived once from the hash.
	rawTags   []string
	displayed map[string]bool
	// carried holds the monbooru projection of every raw the file already has,
	// plus its pre-widening fold: rows monbooru stored before the charset
	// widening keep the folded spelling (girls' -> girls_), and neither shape's
	// reverse mapping reproduces the original, so the exact known-check would
	// keep offering them as new forever.
	carried map[string]bool
	// submitted holds the monbooru forms the caller sent (tags plus implied)
	// and submittedIdeals their PTR ideals; both suppress a removal petition.
	submitted       map[string]bool
	submittedPTR    []string
	submittedIdeals map[string]bool
}

// newContribPreview derives the per-hash side of the diff, which both passes
// share: the file's raw mappings, their monbooru projection, and the display
// view of the submitted list.
func (h *Handler) newContribPreview(hashHex string, tags []string) (*contribPreview, error) {
	p := &contribPreview{
		h:         h,
		hashHex:   hashHex,
		hashRaw:   mustHex(hashHex),
		filter:    h.ptr.TagFilterCached(),
		store:     h.ptr.Contrib(),
		covered:   h.ptr.Status().CoveredThrough,
		carried:   map[string]bool{},
		submitted: map[string]bool{},
	}
	var err error
	if p.rawTags, err = h.ptr.RawTagsForHash(hashHex); err != nil {
		return nil, err
	}
	// The display-view side of the known check depends on the hash alone, so
	// the whole submitted list is compared against one derivation of it.
	if p.displayed, err = h.ptr.HashHasIdeals(hashHex, contribPTRForms(tags)); err != nil {
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

// adds is the submitted side of the diff: one verdict per tag the caller sent,
// in submission order. It records each tag's PTR spelling for the removal pass.
func (p *contribPreview) adds(tags []string) []contribToAdd {
	out := make([]contribToAdd, 0, len(tags))
	for _, tag := range tags {
		p.submitted[tag] = true
		entry := contribToAdd{Tag: tag}
		mapped := mapping.ContribTagFor(tag)
		if !mapped.Eligible() {
			entry.Status, entry.Note = "ineligible", mapped.Note
			out = append(out, entry)
			continue
		}
		entry.PTR = mapped.PTR
		p.submittedPTR = append(p.submittedPTR, mapped.PTR)
		if blocked, rule := p.filter.Blocks(mapped.PTR); blocked {
			entry.Status, entry.Note = "filtered", "blocked by the PTR tag filter ("+rule+")"
			out = append(out, entry)
			continue
		}
		if p.store != nil {
			if waiting, err := p.store.UnsentByKindTag(ptr.ContribMappingAdd, mapped.PTR, p.hashRaw); err == nil && waiting {
				entry.Status = "unsent"
				out = append(out, entry)
				continue
			}
		}
		known, _ := p.h.mappingAddKnown(p.displayed[mapped.PTR], mapped.PTR, p.hashRaw, p.covered)
		if known || p.carried[tag] {
			entry.Status = "known"
		} else {
			entry.Status = "new"
			_, exists, err := p.h.ptr.IdealTag(mapped.PTR)
			entry.UnknownTag = err == nil && !exists
		}
		out = append(out, entry)
	}
	return out
}

// resolveSubmitted folds the implied tags into the submitted set and resolves
// every submitted PTR spelling to its ideal. The removal diff compares on
// ideals, so a raw spelling of a tag the caller already shows never reads as
// "extra" - petitioning it would strip a mapping that is right.
func (p *contribPreview) resolveSubmitted(implied []string) error {
	for _, tag := range implied {
		p.submitted[tag] = true
		if mapped := mapping.ContribTagFor(tag); mapped.Eligible() {
			p.submittedPTR = append(p.submittedPTR, mapped.PTR)
		}
	}
	p.submittedIdeals = map[string]bool{}
	for _, ptrTag := range p.submittedPTR {
		ideal, ok, err := p.h.ptr.IdealTag(ptrTag)
		if err != nil {
			return err
		}
		if ok {
			p.submittedIdeals[ideal] = true
		}
	}
	return nil
}

// removals is the PTR side of the diff: the file's current tags the submitted
// list lacks, each a removal-petition candidate.
func (p *contribPreview) removals() ([]contribPTROnly, error) {
	// A raw whose ideal a sibling of a different raw already carries on the
	// file displays nothing of its own, so removing it changes nothing the
	// file shows. Count how many of the hash's raws resolve to each ideal so
	// those redundant bad-sibling mappings can be left out below.
	idealOfRaw := make(map[string]string, len(p.rawTags))
	idealCarriers := map[string]int{}
	for _, raw := range p.rawTags {
		ideal, ok, err := p.h.ptr.IdealTag(raw)
		if err != nil {
			return nil, err
		}
		if ok {
			idealOfRaw[raw] = ideal
			// Only the canonical raw itself counts as "the good form shows":
			// two bad spellings of one ideal must not suppress each other when
			// the canonical is absent, since removing both is the real fix.
			if raw == ideal {
				idealCarriers[ideal]++
			}
		}
	}

	out := []contribPTROnly{}
	for _, raw := range p.rawTags {
		mb := mapping.MapPTRTag(raw)
		if mb == "" || p.submitted[mb] || p.submitted[mapping.LegacyFoldTag(mb)] {
			continue
		}
		// A rating mapping is local opinion the add side refuses, so the
		// petition side must not offer it either.
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
		out = append(out, contribPTROnly{Tag: mb, PTR: raw, Petitionable: petitionable})
	}
	return out, nil
}

// suppressedRemoval reports whether a raw mapping whose ideal is known is not
// worth petitioning.
func (p *contribPreview) suppressedRemoval(ideal, raw string, idealCarriers map[string]int) bool {
	switch {
	// The ideal lives in a namespace monbooru cannot represent (source:,
	// title:, ...), so a pull never applies this mapping and the operator has
	// no local judgement to petition it against.
	case mapping.MapPTRTag(ideal) == "":
		return true
	// Petitioning a bad-sibling mapping whose canonical form the file also
	// carries removes no visible tag - it is noise, not a fix.
	case ideal != raw && idealCarriers[ideal] > 0:
		return true
	case p.submittedIdeals[ideal]:
		return true
	}
	// A pull applies MapPTRTag(ideal); once the file carries that forward
	// mapping the raw is not a removal candidate. submittedIdeals above
	// reverses through ContribTagFor, which is lossy for ideals whose monbooru
	// form does not round-trip (quoted or namespace-lifted spellings), so check
	// the forward mapping the pull actually stores.
	mi := mapping.MapPTRTag(ideal)
	return mi != "" && (p.submitted[mi] || p.submitted[mapping.LegacyFoldTag(mi)])
}

// contribPTRForms lists the PTR spellings a submitted monbooru-form list
// would send, skipping the ineligible ones.
func contribPTRForms(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if mapped := mapping.ContribTagFor(tag); mapped.Eligible() {
			out = append(out, mapped.PTR)
		}
	}
	return out
}

// mustHex decodes a hash the regexp already validated.
func mustHex(h string) []byte {
	raw, _ := hex.DecodeString(h)
	return raw
}

// contribStageItem is one submitted contribution in monbooru form.
type contribStageItem struct {
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256"`
	Tag    string `json:"tag"`
	Bad    string `json:"bad"`
	Good   string `json:"good"`
	Child  string `json:"child"`
	Parent string `json:"parent"`
	Reason string `json:"reason"`
}

// contribStageResult is the per-item verdict, in submission order.
type contribStageResult struct {
	Kind   string `json:"kind"`
	Result string `json:"result"`
	Note   string `json:"note,omitempty"`
}

// fixedRescindReason is the reason a ledger rescind sends with its
// petition; fixed so the janitors see a consistent, honest label.
const fixedRescindReason = "rescinding my own earlier add"

// ContribSendRefusal reports why a contribution may not go out right now -
// the PTR unavailable, no personal account, a ban, or an index that has not
// caught up - or nil when it may proceed. The API routes and the PTR page's
// buttons share it so the two surfaces refuse on the same conditions.
func ContribSendRefusal(ctx context.Context, svc PTRService) *ContribRefusal {
	if svc == nil || !svc.Enabled() {
		return &ContribRefusal{http.StatusConflict, "ptr_unavailable", "the ptr index is not available"}
	}
	if !svc.HasPersonalKey() {
		return &ContribRefusal{http.StatusConflict, "ptr_account_required", "no personal account key is set"}
	}
	// Read the account fresh so a ban the janitors just set refuses the send
	// synchronously, not after a receipted handoff; fall back to the cached
	// flag if the fetch fails.
	banned := false
	if acc, err := svc.RefreshAccount(ctx); err == nil {
		banned = acc.Banned
	} else if c := svc.Status().Contrib; c != nil {
		banned = c.Banned
	}
	if banned {
		return &ContribRefusal{http.StatusConflict, "ptr_banned", "the account is banned"}
	}
	if !svc.CaughtUp() {
		return &ContribRefusal{http.StatusConflict, "ptr_syncing", "the ptr index is not fully synced yet"}
	}
	return nil
}

// contribAccountGate answers the synchronous account refusals for the
// staging and commit paths; ok=false means a response was written.
func (h *Handler) contribAccountGate(ctx context.Context, w http.ResponseWriter) bool {
	if refusal := ContribSendRefusal(ctx, h.ptr); refusal != nil {
		apiError(w, refusal.Status, refusal.Code, refusal.Msg)
		return false
	}
	return true
}

// ptrContribStage handles POST /api/v1/ptr/contrib: stage items and, by
// default, commit them in the same call as one queue job. Nothing is
// all-or-nothing - one bad item does not sink a panelful; the per-item
// results say exactly what got sent.
func (h *Handler) ptrContribStage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Commit *bool              `json:"commit"`
		Origin string             `json:"origin"`
		Items  []contribStageItem `json:"items"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	commit := body.Commit == nil || *body.Commit
	if commit {
		if !h.contribAccountGate(r.Context(), w) {
			return
		}
	} else if !h.contribGate(w) {
		return
	}
	store := h.ptr.Contrib()
	if store == nil {
		apiError(w, http.StatusConflict, "ptr_unavailable", "the contribution store is not available")
		return
	}
	if len(body.Items) == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", "items must not be empty")
		return
	}

	results := make([]contribStageResult, 0, len(body.Items))
	var accepted []int64
	seen := map[int64]bool{}
	// Coverage is a property of the index, not of an item, and reading it
	// stats the index files behind the engine lock.
	covered := h.ptr.Status().CoveredThrough
	for _, item := range body.Items {
		result, id := h.stageOne(store, item, body.Origin, covered)
		if result.Result == "staged" && id != 0 {
			// A row resolved twice in one request (the same item listed
			// twice) is a duplicate; a row that already existed from an
			// earlier request is a genuine resend.
			if seen[id] {
				result.Result, result.Note = "duplicate", "an identical item is already in this batch"
			} else {
				seen[id] = true
				accepted = append(accepted, id)
			}
		}
		results = append(results, result)
	}
	resp := map[string]any{"results": results}
	status := http.StatusOK
	if commit && len(accepted) > 0 {
		resp["job_id"] = h.queue.EnqueueContrib(accepted, false)
		status = http.StatusAccepted
	}
	writeJSON(w, status, resp)
}

// stageOne validates and stages one item, returning its verdict and the
// staged row id.
func (h *Handler) stageOne(store *ptr.ContribStore, item contribStageItem, origin string, covered int64) (contribStageResult, int64) {
	s := &contribStager{
		h: h, store: store, item: item, origin: origin,
		reason: strings.TrimSpace(item.Reason),
		res:    contribStageResult{Kind: item.Kind},
	}
	if item.Kind != ptr.ContribMappingAdd && s.reason == "" {
		return s.fail("invalid_reason", "a reason is required")
	}
	switch item.Kind {
	case ptr.ContribMappingAdd, ptr.ContribMappingPetition:
		return s.stageMapping(covered)
	case ptr.ContribSibling:
		return s.stageRelation(h.ptr.SiblingCurrent, "alias", "bad", "good", item.Bad, item.Good)
	case ptr.ContribSiblingPetition:
		return s.stagePetition(h.ptr.SiblingCurrent, "alias", item.Bad, item.Good)
	case ptr.ContribParent:
		return s.stageRelation(h.ptr.ParentCurrent, "implication", "child", "parent", item.Child, item.Parent)
	case ptr.ContribParentPetition:
		return s.stagePetition(h.ptr.ParentCurrent, "implication", item.Child, item.Parent)
	}
	return s.fail("ineligible", "unknown kind "+item.Kind)
}

// contribStager stages one submitted item: the shared refusal and store
// plumbing, plus one method per kind the switch above dispatches into.
type contribStager struct {
	h      *Handler
	store  *ptr.ContribStore
	item   contribStageItem
	origin string
	reason string
	res    contribStageResult
}

// fail records a refusal verdict; nothing was staged, so the id is zero.
func (s *contribStager) fail(result, note string) (contribStageResult, int64) {
	s.res.Result, s.res.Note = result, note
	return s.res, 0
}

func (s *contribStager) stage(it ptr.ContribItem) (contribStageResult, int64) {
	it.Origin = s.origin
	it.Reason = s.reason
	id, dup, err := s.store.Stage(it)
	if err != nil {
		return s.fail("ineligible", "staging failed: "+err.Error())
	}
	if dup {
		// The row already exists unsent (staged or failed). Re-confirming
		// should resend it, not report a no-op, so resolve to its id and
		// let the commit reclaim it.
		if existing, ok, _ := s.store.StagedID(it.Kind, it.Tag, it.Tag2, it.Hash); ok {
			s.res.Result = "staged"
			return s.res, existing
		}
		return s.fail("duplicate", "an identical item already waits unsent")
	}
	s.res.Result = "staged"
	return s.res, id
}

// stageMapping handles the two hash-keyed kinds: adding a tag to a file, and
// petitioning one off it.
func (s *contribStager) stageMapping(covered int64) (contribStageResult, int64) {
	if !sha256HexRe.MatchString(s.item.SHA256) {
		return s.fail("ineligible", "sha256 must be 64 lowercase hex characters")
	}
	hash := mustHex(s.item.SHA256)
	if s.item.Kind == ptr.ContribMappingAdd {
		mapped := mapping.ContribTagFor(s.item.Tag)
		if !mapped.Eligible() {
			return s.fail("ineligible", mapped.Note)
		}
		displayed, err := s.h.ptr.HashHasIdeal(s.item.SHA256, mapped.PTR)
		if err != nil {
			return s.fail("ineligible", err.Error())
		}
		if known, note := s.h.mappingAddKnown(displayed, mapped.PTR, hash, covered); known {
			return s.fail("already_known", note)
		}
		return s.stage(ptr.ContribItem{Kind: s.item.Kind, Tag: mapped.PTR, Hash: hash})
	}
	// A petition names a mapping the server already stores, so the
	// submitted spelling is tried verbatim first; a raw whose
	// namespace or underscores the outbound mapper would rewrite
	// stays petitionable that way. The monbooru-form mapping is the
	// fallback.
	tag := s.item.Tag
	current, err := s.h.ptr.HashHasRaw(s.item.SHA256, tag)
	if err != nil {
		return s.fail("ineligible", err.Error())
	}
	if !current {
		mapped := mapping.ContribTagFor(s.item.Tag)
		if !mapped.Eligible() {
			return s.fail("ineligible", mapped.Note)
		}
		tag = mapped.PTR
		if current, err = s.h.ptr.HashHasRaw(s.item.SHA256, tag); err != nil {
			return s.fail("ineligible", err.Error())
		}
		if !current {
			return s.fail("not_on_ptr", "the PTR does not hold this exact mapping")
		}
	}
	if pending, err := s.store.PendingMappingPetition(tag, hash); err == nil && pending {
		return s.fail("already_suggested", "a removal for this mapping is already staged or awaiting review")
	}
	return s.stage(ptr.ContribItem{Kind: s.item.Kind, Tag: tag, Hash: hash})
}

// stagePetition handles a pair petition, which names a relation the server
// already stores; the two petition kinds differ only in which relation is
// checked and its noun.
func (s *contribStager) stagePetition(current func(a, b string) (bool, error), noun, a, b string) (contribStageResult, int64) {
	ra, rb, ok, err := s.h.currentPetitionPair(current, a, b)
	if err != nil {
		return s.fail("ineligible", err.Error())
	}
	if !ok {
		return s.fail("not_on_ptr", "the PTR does not hold this "+noun)
	}
	if pending, err := s.store.PendingLog(s.item.Kind, ra, rb); err == nil && pending {
		return s.fail("already_suggested", "already sent and awaiting janitor review")
	}
	return s.stage(ptr.ContribItem{Kind: s.item.Kind, Tag: ra, Tag2: rb})
}

// stageRelation handles a pair suggestion, which names a relation the server
// does not store yet; the two suggestion kinds differ only in which relation is
// checked, its noun, and how each end is labelled in a refusal.
func (s *contribStager) stageRelation(current func(a, b string) (bool, error), noun, aLabel, bLabel, a, b string) (contribStageResult, int64) {
	aMapped, bMapped := mapping.ContribTagFor(a), mapping.ContribTagFor(b)
	if !aMapped.Eligible() {
		return s.fail("ineligible", aLabel+": "+aMapped.Note)
	}
	if !bMapped.Eligible() {
		return s.fail("ineligible", bLabel+": "+bMapped.Note)
	}
	_, _, cur, err := s.h.currentPetitionPair(current, a, b)
	if err != nil {
		return s.fail("ineligible", err.Error())
	}
	if cur {
		return s.fail("already_known", "the PTR already has this "+noun)
	}
	reverse, err := current(bMapped.PTR, aMapped.PTR)
	if err != nil {
		return s.fail("ineligible", err.Error())
	}
	if reverse {
		return s.fail("conflict", "the PTR holds this "+noun+" in the opposite direction")
	}
	if pending, err := s.store.PendingLog(s.item.Kind, aMapped.PTR, bMapped.PTR); err == nil && pending {
		return s.fail("already_suggested", "already sent and awaiting janitor review")
	}
	return s.stage(ptr.ContribItem{Kind: s.item.Kind, Tag: aMapped.PTR, Tag2: bMapped.PTR})
}

// mappingAddKnown reports whether the PTR already effectively carries the
// tag for the hash: displayed says whether the synced display view has it,
// and this adds the sends committed too recently to have replayed into the
// index. The note names which, for the stage verdict; the preview ignores it.
func (h *Handler) mappingAddKnown(displayed bool, ptrTag string, hash []byte, covered int64) (bool, string) {
	if displayed {
		return true, "the PTR already has this tag for the file"
	}
	if store := h.ptr.Contrib(); store != nil {
		if committed, err := store.MappingAddCommittedSince(ptrTag, hash, covered); err == nil && committed {
			return true, "sent earlier and not yet in the synced index"
		}
	}
	return false, ""
}

// currentPetitionPair finds the raw spellings of a pair petition that the
// index actually holds. Like a mapping petition, each endpoint is tried
// verbatim before its monbooru-form mapping, so a raw the outbound mapper
// would rewrite - a literal underscore, a space, a shifted namespace -
// stays petitionable on whichever side carries it. ok=false when no
// combination is current.
func (h *Handler) currentPetitionPair(current func(a, b string) (bool, error), a, b string) (ra, rb string, ok bool, err error) {
	for _, x := range petitionForms(a) {
		for _, y := range petitionForms(b) {
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

// endpointIdeals resolves a monbooru-form pair endpoint to the ideals of
// every spelling the index knows. Spellings can sit in different clusters -
// a verbatim underscore form can be an orphan while the space-joined form
// carries the sibling pair - so the covered check must see them all, not
// the first hit.
func (h *Handler) endpointIdeals(t string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, f := range petitionForms(t) {
		ideal, ok, err := h.ptr.IdealTag(f)
		if err != nil {
			return nil, err
		}
		if ok {
			out[ideal] = true
		}
	}
	return out, nil
}

// sharesIdeal reports whether the two ideal sets intersect.
func sharesIdeal(a, b map[string]bool) bool {
	for ideal := range a {
		if b[ideal] {
			return true
		}
	}
	return false
}

// petitionForms lists the PTR spellings a monbooru-form endpoint might
// carry: every namespace the category could route through, each in its
// underscore and space forms, so a pair the index holds under a secondary
// namespace (artist: beside creator:, studio: beside copyright:) is still
// matched.
func petitionForms(t string) []string {
	return mapping.PTRNamespacesFor(t)
}

// contribUnsentJSON is one unsent row on the ledger endpoint.
type contribUnsentJSON struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Tag       string `json:"tag"`
	Tag2      string `json:"tag2,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Origin    string `json:"origin,omitempty"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// contribLogJSON is one committed-history row.
type contribLogJSON struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	Tag         string `json:"tag"`
	Tag2        string `json:"tag2,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Reason      string `json:"reason,omitempty"`
	CommittedAt int64  `json:"committed_at"`
	Outcome     string `json:"outcome,omitempty"`
	OutcomeAt   int64  `json:"outcome_at,omitempty"`
}

// ptrContribLedger handles GET /api/v1/ptr/contrib: the unsent backlog,
// counts, and the newest slice of the committed history.
func (h *Handler) ptrContribLedger(w http.ResponseWriter, r *http.Request) {
	if !h.contribGate(w) {
		return
	}
	store := h.ptr.Contrib()
	if store == nil {
		apiError(w, http.StatusConflict, "ptr_unavailable", "the contribution store is not available")
		return
	}
	unsent, err := store.Unsent()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	logRows, err := store.LogRows(100)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	unsentJSON := make([]contribUnsentJSON, 0, len(unsent))
	countsByKind := map[string]int{}
	countsByStatus := map[string]int{}
	for _, it := range unsent {
		countsByKind[it.Kind]++
		countsByStatus[it.Status]++
		unsentJSON = append(unsentJSON, contribUnsentJSON{
			ID: it.ID, Kind: it.Kind, Tag: it.Tag, Tag2: it.Tag2,
			SHA256: hexOrEmpty(it.Hash), Reason: it.Reason, Origin: it.Origin,
			Status: it.Status, Error: it.Error, CreatedAt: it.CreatedAt,
		})
	}
	logJSON := make([]contribLogJSON, 0, len(logRows))
	outcomes := map[string]int{}
	for _, row := range logRows {
		key := row.Outcome
		if key == "" {
			key = "pending"
		}
		outcomes[key]++
		logJSON = append(logJSON, contribLogJSON{
			ID: row.ID, Kind: row.Kind, Tag: row.Tag, Tag2: row.Tag2,
			SHA256: hexOrEmpty(row.Hash), Reason: row.Reason,
			CommittedAt: row.CommittedAt, Outcome: row.Outcome, OutcomeAt: row.OutcomeAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"unsent": unsentJSON,
		"counts": map[string]any{"by_kind": countsByKind, "by_status": countsByStatus, "outcomes": outcomes},
		"log":    logJSON,
	})
}

// ptrContribRescind handles DELETE /api/v1/ptr/contrib/{id}: rescind
// one unsent item exactly - nothing ever left the machine.
func (h *Handler) ptrContribRescind(w http.ResponseWriter, r *http.Request) {
	if !h.contribGate(w) {
		return
	}
	store := h.ptr.Contrib()
	if store == nil {
		apiError(w, http.StatusConflict, "ptr_unavailable", "the contribution store is not available")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "bad item id")
		return
	}
	ok, err := store.Rescind(id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if !ok {
		apiError(w, http.StatusNotFound, "not_found", "no unsent item with that id")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ptrContribRescindAll handles DELETE /api/v1/ptr/contrib.
func (h *Handler) ptrContribRescindAll(w http.ResponseWriter, r *http.Request) {
	if !h.contribGate(w) {
		return
	}
	store := h.ptr.Contrib()
	if store == nil {
		apiError(w, http.StatusConflict, "ptr_unavailable", "the contribution store is not available")
		return
	}
	n, err := store.RescindAll()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rescinded": n})
}

// ptrContribCommit handles POST /api/v1/ptr/contrib/commit: send the
// unsent backlog - the retry path for failed items. The only path that
// needs the already_running guard: per-confirm sends claim disjoint id
// sets and never contend.
func (h *Handler) ptrContribCommit(w http.ResponseWriter, r *http.Request) {
	if !h.contribAccountGate(r.Context(), w) {
		return
	}
	if h.queue.ContribSendLive() {
		apiError(w, http.StatusConflict, "already_running", "a contribution send is already running")
		return
	}
	jobID := h.queue.EnqueueContrib(nil, true)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": jobID})
}

// ptrContribLogRescind handles POST /api/v1/ptr/contrib/log/{id}/rescind:
// the only honest withdraw the protocol allows - a committed mapping add
// is live under this account's name, so a removal petition for the same
// (tag, hash) goes out with a fixed reason and the ledger row is marked.
// Suggestions and petitions cannot be withdrawn once uploaded.
func (h *Handler) ptrContribLogRescind(w http.ResponseWriter, r *http.Request) {
	if !h.contribAccountGate(r.Context(), w) {
		return
	}
	store := h.ptr.Contrib()
	if store == nil {
		apiError(w, http.StatusConflict, "ptr_unavailable", "the contribution store is not available")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "bad log id")
		return
	}
	itemID, rerr := RescindCommittedAdd(h.ptr, store, id)
	if rerr != nil {
		apiError(w, rerr.Status, rerr.Code, rerr.Msg)
		return
	}
	jobID := h.queue.EnqueueContrib([]int64{itemID}, false)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": jobID})
}

// ContribRefusal is a refused contribution write: the status and code the API
// answers with, and a message any surface can render.
type ContribRefusal struct {
	Status int
	Code   string
	Msg    string
}

// RescindCommittedAdd stages the fixed-reason removal petition for one
// committed mapping add and stamps the ledger row, returning the staged
// item id for the caller to send. The PTR page's button and the API route
// share it so their guards cannot drift apart.
func RescindCommittedAdd(svc PTRService, store *ptr.ContribStore, logID int64) (int64, *ContribRefusal) {
	row, err := store.LogRow(logID)
	if err != nil {
		return 0, &ContribRefusal{http.StatusInternalServerError, "internal_error", err.Error()}
	}
	if row == nil {
		return 0, &ContribRefusal{http.StatusNotFound, "not_found", "no ledger row with that id"}
	}
	if row.Kind != ptr.ContribMappingAdd {
		return 0, &ContribRefusal{http.StatusConflict, "not_rescindable", "only a committed mapping add can be rescinded"}
	}
	// Guard on a live withdrawal rather than the add's stamped outcome: if an
	// earlier rescind's petition failed and was cleared, the add stays
	// rescindable instead of being blocked forever by the stamp.
	if pending, _ := store.PendingMappingPetition(row.Tag, row.Hash); pending {
		return 0, &ContribRefusal{http.StatusConflict, "not_rescindable", "a removal for this add is already staged or awaiting review"}
	}
	current, err := svc.HashHasRaw(hexOrEmpty(row.Hash), row.Tag)
	if err != nil {
		return 0, &ContribRefusal{http.StatusConflict, "ptr_unavailable", err.Error()}
	}
	if !current {
		// An add committed after the index's coverage window is live
		// server-side but not yet replayed into the index, so it is still
		// ours to withdraw even though HashHasRaw can't see it yet.
		committed, _ := store.MappingAddCommittedSince(row.Tag, row.Hash, svc.Status().CoveredThrough)
		if !committed {
			return 0, &ContribRefusal{http.StatusConflict, "not_rescindable", "the mapping is already gone from the index"}
		}
	}
	itemID, dup, err := store.Stage(ptr.ContribItem{
		Kind: ptr.ContribMappingPetition, Tag: row.Tag, Hash: row.Hash, Reason: fixedRescindReason,
	})
	if err != nil {
		return 0, &ContribRefusal{http.StatusInternalServerError, "internal_error", err.Error()}
	}
	if dup {
		if existing, ok, _ := store.StagedID(ptr.ContribMappingPetition, row.Tag, "", row.Hash); ok {
			itemID = existing
		}
	}
	if err := store.SetOutcome(logID, ptr.OutcomeRescinded); err != nil {
		return 0, &ContribRefusal{http.StatusInternalServerError, "internal_error", err.Error()}
	}
	return itemID, nil
}

// hexOrEmpty renders an optional hash blob.
func hexOrEmpty(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return hex.EncodeToString(b)
}

// pairPreviewResponse tells monbooru which contribution direction a
// relation pair takes so its one PTR... action needs no guessing.
type pairPreviewResponse struct {
	APtr        string `json:"a_ptr"`
	BPtr        string `json:"b_ptr"`
	Direction   string `json:"direction"` // suggest | petition | pending | conflict | covered | ineligible
	Note        string `json:"note,omitempty"`
	Provisional bool   `json:"provisional"`
}

// ptrContribPairPreview handles POST /api/v1/ptr/contrib/pair-preview:
// resolve one sibling or parent pair's state against the index and the
// ledger. For a sibling a=bad, b=good; for a parent a=child, b=parent
// (monbooru owns the hydrus child/parent flip).
func (h *Handler) ptrContribPairPreview(w http.ResponseWriter, r *http.Request) {
	if !h.contribGate(w) {
		return
	}
	var body struct {
		Kind string `json:"kind"`
		A    string `json:"a"`
		B    string `json:"b"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.A) == "" || strings.TrimSpace(body.B) == "" {
		// Both are declared required; without this a field-name typo reads as
		// "the repository would rewrite this tag", which is a verdict about a
		// tag rather than about the request.
		apiError(w, http.StatusBadRequest, "invalid_request", "a and b are required")
		return
	}
	aMapped, bMapped := mapping.ContribTagFor(body.A), mapping.ContribTagFor(body.B)
	resp := pairPreviewResponse{Provisional: h.ptr.Provisional()}
	if !aMapped.Eligible() {
		resp.Direction, resp.Note = "ineligible", aMapped.Note
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if !bMapped.Eligible() {
		resp.Direction, resp.Note = "ineligible", bMapped.Note
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.APtr, resp.BPtr = aMapped.PTR, bMapped.PTR

	var currentFn func(a, b string) (bool, error)
	var suggestKind, petitionKind string
	switch body.Kind {
	case ptr.ContribSibling:
		suggestKind, petitionKind = ptr.ContribSibling, ptr.ContribSiblingPetition
		currentFn = h.ptr.SiblingCurrent
	case ptr.ContribParent:
		suggestKind, petitionKind = ptr.ContribParent, ptr.ContribParentPetition
		currentFn = h.ptr.ParentCurrent
	default:
		apiError(w, http.StatusBadRequest, "invalid_request", "kind must be sibling or parent")
		return
	}
	// Checked raw-first, per endpoint, exactly like the petition stage: a
	// pair whose stored spelling the mapper would rewrite reads as
	// petition (present) rather than suggest (absent), and the pending
	// lookup keys on the raw forms the stage would use.
	ra, rb, current, err := h.currentPetitionPair(currentFn, body.A, body.B)
	if err != nil {
		apiError(w, http.StatusConflict, "ptr_unavailable", err.Error())
		return
	}

	store := h.ptr.Contrib()
	if store != nil {
		if pending, _ := store.PendingLog(suggestKind, aMapped.PTR, bMapped.PTR); pending {
			resp.Direction = "pending"
			writeJSON(w, http.StatusOK, resp)
			return
		}
		if current {
			if pending, _ := store.PendingLog(petitionKind, ra, rb); pending {
				resp.Direction = "pending"
				writeJSON(w, http.StatusOK, resp)
				return
			}
		}
	}
	if current {
		resp.Direction = "petition"
	} else if body.Kind == ptr.ContribSibling {
		ia, err := h.endpointIdeals(body.A)
		if err != nil {
			apiError(w, http.StatusConflict, "ptr_unavailable", err.Error())
			return
		}
		ib, err := h.endpointIdeals(body.B)
		if err != nil {
			apiError(w, http.StatusConflict, "ptr_unavailable", err.Error())
			return
		}
		reverse, _ := h.ptr.SiblingCurrent(bMapped.PTR, aMapped.PTR)
		switch {
		case reverse:
			resp.Direction = "conflict"
		case sharesIdeal(ia, ib):
			// A spelling of each side already resolves to one shared ideal
			// on the PTR, so the alias is effectively there already - no
			// suggestion, no re-offer of a pulled fan-in alias.
			resp.Direction = "covered"
		default:
			resp.Direction = "suggest"
		}
	} else {
		reverse, _ := h.ptr.ParentCurrent(bMapped.PTR, aMapped.PTR)
		switch {
		case reverse:
			resp.Direction = "conflict"
		default:
			covered, err := h.parentEdgeCovered(body.A, body.B)
			if err != nil {
				apiError(w, http.StatusConflict, "ptr_unavailable", err.Error())
				return
			}
			if covered {
				resp.Direction = "covered"
			} else {
				resp.Direction = "suggest"
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// parentEdgeCovered reports whether the PTR carries "a implies b" once each end
// is resolved to its ideal across every spelling it could have. A monbooru-form
// name can map from more than one PTR tag (character:char_aznable and
// character:char aznable are distinct rows), and a bare tag resolves into a
// namespace (gundam -> series:gundam), so resolving through endpointIdeals is
// what lets the cluster check find an edge the raw pair misses.
func (h *Handler) parentEdgeCovered(a, b string) (bool, error) {
	ia, err := h.endpointIdeals(a)
	if err != nil {
		return false, err
	}
	ib, err := h.endpointIdeals(b)
	if err != nil {
		return false, err
	}
	for ca := range ia {
		for cb := range ib {
			covered, err := h.ptr.ParentEdgeCovered(ca, cb)
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
