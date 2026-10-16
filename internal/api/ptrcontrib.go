package api

import (
	"cmp"
	"context"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/mapping"
	"github.com/monbooru/monloader/internal/ptr"
)

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

type contribToAdd struct {
	Tag        string `json:"tag"`
	PTR        string `json:"ptr"`
	Status     string `json:"status"`
	Note       string `json:"note,omitempty"`
	UnknownTag bool   `json:"unknown_tag,omitempty"`
}

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

func (h *Handler) contribGate(w http.ResponseWriter) bool {
	if h.ptr == nil || !h.ptr.Enabled() {
		apiError(w, http.StatusConflict, errPTRUnavailable, "the ptr index is not available")
		return false
	}
	return true
}

func (h *Handler) contribStore(w http.ResponseWriter) (*ptr.ContribStore, bool) {
	if !h.contribGate(w) {
		return nil, false
	}
	store := h.ptr.Contrib()
	if store == nil {
		apiError(w, http.StatusConflict, errPTRUnavailable, "the contribution store is not available")
		return nil, false
	}
	return store, true
}

func (h *Handler) ptrContribPreview(w http.ResponseWriter, r *http.Request) {
	if !h.contribGate(w) {
		return
	}
	var body struct {
		SHA256  string   `json:"sha256"`
		Tags    []string `json:"tags"`
		Implied []string `json:"implied"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "invalid JSON body")
		return
	}
	if !sha256HexRe.MatchString(body.SHA256) {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "sha256 must be 64 lowercase hex characters")
		return
	}
	// A missing field would answer an empty diff, which reads like a file
	// with nothing to contribute.
	if body.Tags == nil {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "tags is required; send an empty list for an untagged file")
		return
	}

	p, err := h.newContribPreview(body.SHA256, body.Tags)
	if err != nil {
		apiError(w, http.StatusConflict, errPTRUnavailable, err.Error())
		return
	}
	resp := contribPreviewResponse{
		Provisional: h.ptr.Provisional(),
		ToAdd:       p.adds(body.Tags),
		PTROnly:     []contribPTROnly{},
	}
	if err := p.resolveSubmitted(body.Implied); err != nil {
		apiError(w, http.StatusConflict, errPTRUnavailable, err.Error())
		return
	}
	if resp.PTROnly, err = p.removals(); err != nil {
		apiError(w, http.StatusConflict, errPTRUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type contribPreview struct {
	h         *Handler
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
	// The display view depends on the hash alone, so one derivation
	// answers the whole list.
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

// adds records each tag's PTR spelling, so it runs before the removal pass.
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

// The removal diff compares on ideals: a raw spelling of a tag the caller
// shows must not read as extra, or its petition would strip a right mapping.
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

func (p *contribPreview) removals() ([]contribPTROnly, error) {
	idealOfRaw := make(map[string]string, len(p.rawTags))
	idealCarriers := map[string]int{}
	for _, raw := range p.rawTags {
		ideal, ok, err := p.h.ptr.IdealTag(raw)
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

	out := []contribPTROnly{}
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
		out = append(out, contribPTROnly{Tag: mb, PTR: raw, Petitionable: petitionable})
	}
	return out, nil
}

func (p *contribPreview) suppressedRemoval(ideal, raw string, idealCarriers map[string]int) bool {
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

type contribStageResult struct {
	Kind   string `json:"kind"`
	Result string `json:"result"`
	Note   string `json:"note,omitempty"`
}

const fixedRescindReason = "rescinding my own earlier add"

func ContribSendRefusal(ctx context.Context, svc PTRService) *ContribRefusal {
	if svc == nil || !svc.Enabled() {
		return &ContribRefusal{http.StatusConflict, errPTRUnavailable, "the ptr index is not available"}
	}
	if !svc.HasPersonalKey() {
		return &ContribRefusal{http.StatusConflict, errPTRAccountRequired, "no personal account key is set"}
	}
	// Fresh, so a ban the janitors just set refuses the send now rather
	// than after a receipted handoff; bounded well inside monbooru's 10 s
	// send budget, past which the cached flag answers.
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	banned := false
	if acc, err := svc.RefreshAccount(rctx); err == nil {
		banned = acc.Banned
	} else if c := svc.Status().Contrib; c != nil {
		banned = c.Banned
	}
	if banned {
		return &ContribRefusal{http.StatusConflict, errPTRBanned, "the account is banned"}
	}
	if !svc.CaughtUp() {
		return &ContribRefusal{http.StatusConflict, errPTRSyncing, "the ptr index is not fully synced yet"}
	}
	return nil
}

func (h *Handler) contribAccountGate(ctx context.Context, w http.ResponseWriter) bool {
	if refusal := ContribSendRefusal(ctx, h.ptr); refusal != nil {
		apiError(w, refusal.Status, refusal.Code, refusal.Msg)
		return false
	}
	return true
}

func (h *Handler) ptrContribStage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Commit *bool              `json:"commit"`
		Origin string             `json:"origin"`
		Items  []contribStageItem `json:"items"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "invalid JSON body")
		return
	}
	commit := body.Commit == nil || *body.Commit
	if commit && !h.contribAccountGate(r.Context(), w) {
		return
	}
	// A sender that gave up waiting has already told its user the send
	// failed, so nothing may land behind its back.
	if r.Context().Err() != nil {
		return
	}
	store, ok := h.contribStore(w)
	if !ok {
		return
	}
	if len(body.Items) == 0 {
		apiError(w, http.StatusBadRequest, errInvalidRequest, "items must not be empty")
		return
	}

	results := make([]contribStageResult, 0, len(body.Items))
	var accepted []int64
	seen := map[int64]bool{}
	// Read once: it stats the index files behind the engine lock.
	covered := h.ptr.Status().CoveredThrough
	for _, item := range body.Items {
		result, id := h.stageOne(store, item, body.Origin, covered)
		if result.Result == "staged" && id != 0 {
			// The same item twice in one request is a duplicate; a row
			// left from an earlier request is a genuine resend.
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
	if commit && len(accepted) > 0 && r.Context().Err() == nil {
		resp["job_id"] = h.queue.EnqueueContrib(accepted, false)
		status = http.StatusAccepted
	}
	writeJSON(w, status, resp)
}

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

type contribStager struct {
	h      *Handler
	store  *ptr.ContribStore
	item   contribStageItem
	origin string
	reason string
	res    contribStageResult
}

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
		// Re-confirming an unsent row resends it rather than reporting a no-op.
		if existing, ok, _ := s.store.StagedID(it.Kind, it.Tag, it.Tag2, it.Hash); ok {
			s.res.Result = "staged"
			return s.res, existing
		}
		return s.fail("duplicate", "an identical item already waits unsent")
	}
	s.res.Result = "staged"
	return s.res, id
}

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
	// The verbatim spelling first: a stored raw whose namespace or
	// underscores the outbound mapper would rewrite stays petitionable.
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

func (h *Handler) currentPetitionPair(current func(a, b string) (bool, error), a, b string) (ra, rb string, ok bool, err error) {
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
func (h *Handler) endpointIdeals(t string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, f := range mapping.PTRNamespacesFor(t) {
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

func sharesIdeal(a, b map[string]bool) bool {
	for ideal := range a {
		if b[ideal] {
			return true
		}
	}
	return false
}

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

func (h *Handler) ptrContribLedger(w http.ResponseWriter, r *http.Request) {
	store, ok := h.contribStore(w)
	if !ok {
		return
	}
	unsent, err := store.Unsent()
	if err != nil {
		apiError(w, http.StatusInternalServerError, errInternal, err.Error())
		return
	}
	logRows, err := store.LogRows(100)
	if err != nil {
		apiError(w, http.StatusInternalServerError, errInternal, err.Error())
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
			SHA256: hex.EncodeToString(it.Hash), Reason: it.Reason, Origin: it.Origin,
			Status: it.Status, Error: it.Error, CreatedAt: it.CreatedAt,
		})
	}
	logJSON := make([]contribLogJSON, 0, len(logRows))
	outcomes := map[string]int{}
	for _, row := range logRows {
		outcomes[cmp.Or(row.Outcome, "pending")]++
		logJSON = append(logJSON, contribLogJSON{
			ID: row.ID, Kind: row.Kind, Tag: row.Tag, Tag2: row.Tag2,
			SHA256: hex.EncodeToString(row.Hash), Reason: row.Reason,
			CommittedAt: row.CommittedAt, Outcome: row.Outcome, OutcomeAt: row.OutcomeAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"unsent": unsentJSON,
		"counts": map[string]any{"by_kind": countsByKind, "by_status": countsByStatus, "outcomes": outcomes},
		"log":    logJSON,
	})
}

func (h *Handler) ptrContribRescind(w http.ResponseWriter, r *http.Request) {
	store, ok := h.contribStore(w)
	if !ok {
		return
	}
	id, ok := apiPathInt64(w, r, "id")
	if !ok {
		return
	}
	found, err := store.Rescind(id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, errInternal, err.Error())
		return
	}
	if !found {
		apiError(w, http.StatusNotFound, errNotFound, "no unsent item with that id")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) ptrContribRescindAll(w http.ResponseWriter, r *http.Request) {
	store, ok := h.contribStore(w)
	if !ok {
		return
	}
	n, err := store.RescindAll()
	if err != nil {
		apiError(w, http.StatusInternalServerError, errInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rescinded": n})
}

// Only this path needs the already_running guard: per-confirm sends claim
// disjoint id sets.
func (h *Handler) ptrContribCommit(w http.ResponseWriter, r *http.Request) {
	if !h.contribAccountGate(r.Context(), w) {
		return
	}
	if h.queue.ContribSendLive() {
		apiError(w, http.StatusConflict, errAlreadyRunning, "a contribution send is already running")
		return
	}
	jobID := h.queue.EnqueueContrib(nil, true)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": jobID})
}

func (h *Handler) ptrContribLogRescind(w http.ResponseWriter, r *http.Request) {
	if !h.contribAccountGate(r.Context(), w) {
		return
	}
	store, ok := h.contribStore(w)
	if !ok {
		return
	}
	id, ok := apiPathInt64(w, r, "id")
	if !ok {
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

type ContribRefusal struct {
	Status int
	Code   string
	Msg    string
}

// RescindCommittedAdd stages the removal petition and stamps the ledger
// row; the caller must send the returned item.
func RescindCommittedAdd(svc PTRService, store *ptr.ContribStore, logID int64) (int64, *ContribRefusal) {
	row, err := store.LogRow(logID)
	if err != nil {
		return 0, &ContribRefusal{http.StatusInternalServerError, errInternal, err.Error()}
	}
	if row == nil {
		return 0, &ContribRefusal{http.StatusNotFound, errNotFound, "no ledger row with that id"}
	}
	if row.Kind != ptr.ContribMappingAdd {
		return 0, &ContribRefusal{http.StatusConflict, errNotRescindable, "only a committed mapping add can be rescinded"}
	}
	// Not the stamped outcome: after an earlier rescind's petition failed
	// and was cleared, the add must stay rescindable.
	if pending, _ := store.PendingMappingPetition(row.Tag, row.Hash); pending {
		return 0, &ContribRefusal{http.StatusConflict, errNotRescindable, "a removal for this add is already staged or awaiting review"}
	}
	current, err := svc.HashHasRaw(hex.EncodeToString(row.Hash), row.Tag)
	if err != nil {
		return 0, &ContribRefusal{http.StatusConflict, errPTRUnavailable, err.Error()}
	}
	if !current {
		// An add committed after the coverage window is live server-side
		// but not yet in the index.
		committed, _ := store.MappingAddCommittedSince(row.Tag, row.Hash, svc.Status().CoveredThrough)
		if !committed {
			return 0, &ContribRefusal{http.StatusConflict, errNotRescindable, "the mapping is already gone from the index"}
		}
	}
	itemID, dup, err := store.Stage(ptr.ContribItem{
		Kind: ptr.ContribMappingPetition, Tag: row.Tag, Hash: row.Hash, Reason: fixedRescindReason,
	})
	if err != nil {
		return 0, &ContribRefusal{http.StatusInternalServerError, errInternal, err.Error()}
	}
	if dup {
		if existing, ok, _ := store.StagedID(ptr.ContribMappingPetition, row.Tag, "", row.Hash); ok {
			itemID = existing
		}
	}
	if err := store.SetOutcome(logID, ptr.OutcomeRescinded); err != nil {
		return 0, &ContribRefusal{http.StatusInternalServerError, errInternal, err.Error()}
	}
	return itemID, nil
}

type pairPreviewResponse struct {
	APtr        string `json:"a_ptr"`
	BPtr        string `json:"b_ptr"`
	Direction   string `json:"direction"`
	Note        string `json:"note,omitempty"`
	Provisional bool   `json:"provisional"`
}

// A parent pair arrives as a=child, b=parent: monbooru owns the hydrus flip.
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
		apiError(w, http.StatusBadRequest, errInvalidRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.A) == "" || strings.TrimSpace(body.B) == "" {
		// Without this a field-name typo reads as a verdict about a tag
		// rather than about the request.
		apiError(w, http.StatusBadRequest, errInvalidRequest, "a and b are required")
		return
	}
	aMapped, bMapped := mapping.ContribTagFor(body.A), mapping.ContribTagFor(body.B)
	resp := pairPreviewResponse{Provisional: h.ptr.Provisional()}
	for _, m := range []mapping.ContribTag{aMapped, bMapped} {
		if !m.Eligible() {
			resp.Direction, resp.Note = "ineligible", m.Note
			writeJSON(w, http.StatusOK, resp)
			return
		}
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
		apiError(w, http.StatusBadRequest, errInvalidRequest, "kind must be sibling or parent")
		return
	}
	// Raw-first like the petition stage: a stored spelling the mapper
	// would rewrite reads as petition, not suggest, and the pending
	// lookup keys on the raws the stage uses.
	ra, rb, current, err := h.currentPetitionPair(currentFn, body.A, body.B)
	if err != nil {
		apiError(w, http.StatusConflict, errPTRUnavailable, err.Error())
		return
	}

	if store := h.ptr.Contrib(); store != nil {
		pending, _ := store.PendingLog(suggestKind, aMapped.PTR, bMapped.PTR)
		if !pending && current {
			pending, _ = store.PendingLog(petitionKind, ra, rb)
		}
		if pending {
			resp.Direction = "pending"
			writeJSON(w, http.StatusOK, resp)
			return
		}
	}
	if current {
		resp.Direction = "petition"
	} else if body.Kind == ptr.ContribSibling {
		ia, err := h.endpointIdeals(body.A)
		if err != nil {
			apiError(w, http.StatusConflict, errPTRUnavailable, err.Error())
			return
		}
		ib, err := h.endpointIdeals(body.B)
		if err != nil {
			apiError(w, http.StatusConflict, errPTRUnavailable, err.Error())
			return
		}
		reverse, _ := h.ptr.SiblingCurrent(bMapped.PTR, aMapped.PTR)
		switch {
		case reverse:
			resp.Direction = "conflict"
		case sharesIdeal(ia, ib):
			resp.Direction = "covered"
		default:
			resp.Direction = "suggest"
		}
	} else {
		if reverse, _ := h.ptr.ParentCurrent(bMapped.PTR, aMapped.PTR); reverse {
			resp.Direction = "conflict"
		} else {
			covered, err := h.parentEdgeCovered(body.A, body.B)
			if err != nil {
				apiError(w, http.StatusConflict, errPTRUnavailable, err.Error())
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
