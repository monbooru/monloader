package ptr

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/logx"
)

// The engine's contribution-account surface: one personal account slot
// riding [ptr].access_key, shared by sync and uploads exactly like the
// hydrus client. Creation is the repository's open auto-creation flow;
// replacing an account stays deliberately manual (clear the key in
// settings), because spinning up fresh accounts to dodge a ban is the
// abuse the PTR bans for.

// ErrAccountExists refuses a second creation while a personal key is
// set.
var ErrAccountExists = errors.New("a personal account key is already set")

// ErrNoAutoCreation reports a repository with no auto-creatable type.
var ErrNoAutoCreation = errors.New("the repository offers no auto-creatable account type")

// ContribStatus is the contribution gate monbooru polls on ptr/status.
type ContribStatus struct {
	// Account is true when a personal (non-public) key is set; every
	// monbooru contribution surface gates on it.
	Account bool `json:"account"`
	Banned  bool `json:"banned"`
	Unsent  int  `json:"unsent"`
	Failed  int  `json:"failed"`
}

// accountCacheTTL bounds how often the lazily-polled account card can
// trigger a real GET /account against the repository.
const accountCacheTTL = time.Minute

// HasPersonalKey reports whether a personal (non-public) access key is
// configured.
func (e *Engine) HasPersonalKey() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hasPersonalKeyLocked()
}

func (e *Engine) hasPersonalKeyLocked() bool {
	return e.cfg.AccessKey != "" && e.cfg.AccessKey != config.PublicAccessKey
}

// AccessKey returns the configured personal key, empty when only the
// public key is in use. Rendered exactly once, by the PTR page's
// explicit reveal control; never logged.
func (e *Engine) AccessKey() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.hasPersonalKeyLocked() {
		return ""
	}
	return e.cfg.AccessKey
}

// SetAccessKey re-points the engine and its live client at key with no
// restart; the sync picks it up on its next request. The cached account
// state is dropped - it described the old key.
func (e *Engine) SetAccessKey(key string) {
	e.mu.Lock()
	e.cfg.AccessKey = key
	e.account = nil
	e.accountAt = time.Time{}
	e.mu.Unlock()
	e.client.SetAccessKey(config.PTRConfig{AccessKey: key}.EffectiveAccessKey())
}

// CreateContribAccount runs the repository's three-step auto-creation
// and re-points the engine at the new key. The caller persists the
// returned key to the config file; the registration key lives only on
// the stack here and is never logged. Refused while a personal key is
// already set.
func (e *Engine) CreateContribAccount(ctx context.Context) (string, error) {
	e.createMu.Lock()
	defer e.createMu.Unlock()
	if e.HasPersonalKey() {
		return "", ErrAccountExists
	}
	types, err := e.client.AccountTypes(ctx)
	if err != nil {
		return "", fmt.Errorf("listing account types: %w", err)
	}
	var chosen *AccountType
	for i := range types {
		if types[i].VelocityNum > 0 && types[i].CanCreateMappings() {
			chosen = &types[i]
			break
		}
	}
	if chosen == nil {
		return "", ErrNoAutoCreation
	}
	regKey, err := e.client.RegistrationKey(ctx, chosen.Key)
	if err != nil {
		return "", err
	}
	accessKey, err := e.client.RedeemAccessKey(ctx, regKey)
	if err != nil {
		return "", err
	}
	e.SetAccessKey(accessKey)
	// First authenticated use materializes the account row server-side
	// and seeds the cached state the status gate reads.
	if _, err := e.ContribAccount(ctx); err != nil {
		return accessKey, fmt.Errorf("account created but unreadable: %w", err)
	}
	return accessKey, nil
}

// ContribAccount returns the personal account's state, cached for
// accountCacheTTL so the page's poll cannot hammer the repository.
func (e *Engine) ContribAccount(ctx context.Context) (*Account, error) {
	e.mu.Lock()
	if !e.hasPersonalKeyLocked() {
		e.mu.Unlock()
		return nil, errors.New("no personal account key is set")
	}
	if e.account != nil && time.Since(e.accountAt) < accountCacheTTL {
		acc := e.account
		e.mu.Unlock()
		return acc, nil
	}
	e.mu.Unlock()

	acc, err := e.client.Account(ctx)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.account = acc
	e.accountAt = time.Now()
	e.mu.Unlock()
	return acc, nil
}

// CachedAccount returns the last fetched account state without a
// network round trip; nil when none has been fetched yet.
func (e *Engine) CachedAccount() *Account {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.account
}

// contribStatusLocked builds the status gate from config and the cached
// account. Unsent/Failed ride the contribution store once it exists.
// Caller holds e.mu.
func (e *Engine) contribStatusLocked() *ContribStatus {
	cs := &ContribStatus{Account: e.hasPersonalKeyLocked()}
	if e.account != nil {
		cs.Banned = e.account.Banned
	}
	if e.contrib != nil {
		if unsent, failed, err := e.contrib.Counts(); err == nil {
			cs.Unsent, cs.Failed = unsent, failed
		}
	}
	return cs
}

// Contrib returns the contribution store, nil while the PTR is
// disabled or the store failed to open.
func (e *Engine) Contrib() *ContribStore {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.contrib
}

// errIndexUnavailable reports diff checks attempted while the index is
// not open.
var errIndexUnavailable = errors.New("the ptr index is not available")

// indexStore returns the open index under the engine lock.
func (e *Engine) indexStore() (*Store, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.store == nil {
		return nil, errIndexUnavailable
	}
	return e.store, nil
}

// onIndex runs a query against the open index, answering errIndexUnavailable
// with the zero value while the PTR is off. Every contribution and lookup
// query the API layer makes goes through it.
func onIndex[T any](e *Engine, fn func(*Store) (T, error)) (T, error) {
	s, err := e.indexStore()
	if err != nil {
		var zero T
		return zero, err
	}
	return fn(s)
}

// HashHasIdeal answers the display-view mapping check on the index.
func (e *Engine) HashHasIdeal(hashHex, tag string) (bool, error) {
	return onIndex(e, func(s *Store) (bool, error) { return s.HashHasIdeal(hashHex, tag) })
}

// HashHasIdeals answers the display-view mapping check for a whole tag
// list against one hash.
func (e *Engine) HashHasIdeals(hashHex string, tags []string) (map[string]bool, error) {
	return onIndex(e, func(s *Store) (map[string]bool, error) { return s.HashHasIdeals(hashHex, tags) })
}

// IdealTag answers the sibling resolution of one raw spelling. It is the one
// query with a third return, so it calls indexStore directly rather than
// wrapping its answer in a struct to fit onIndex.
func (e *Engine) IdealTag(tag string) (string, bool, error) {
	s, err := e.indexStore()
	if err != nil {
		return "", false, err
	}
	return s.IdealTag(tag)
}

// HashHasRaw answers the raw-mapping check on the index.
func (e *Engine) HashHasRaw(hashHex, tag string) (bool, error) {
	return onIndex(e, func(s *Store) (bool, error) { return s.HashHasRaw(hashHex, tag) })
}

// RawTagsForHash lists the hash's current raw mappings.
func (e *Engine) RawTagsForHash(hashHex string) ([]string, error) {
	return onIndex(e, func(s *Store) ([]string, error) { return s.RawTagsForHash(hashHex) })
}

// SiblingCurrent answers the exact-pair sibling check on the index.
func (e *Engine) SiblingCurrent(bad, good string) (bool, error) {
	return onIndex(e, func(s *Store) (bool, error) { return s.SiblingCurrent(bad, good) })
}

// ParentCurrent answers the exact-pair parent check on the index.
func (e *Engine) ParentCurrent(child, parent string) (bool, error) {
	return onIndex(e, func(s *Store) (bool, error) { return s.ParentCurrent(child, parent) })
}

// ParentEdgeCovered answers the sibling-resolved parent check on the index.
func (e *Engine) ParentEdgeCovered(child, parent string) (bool, error) {
	return onIndex(e, func(s *Store) (bool, error) { return s.ParentEdgeCovered(child, parent) })
}

// Syncing reports whether the index is mid-build, so diff answers are
// provisional: a partial index can only overstate what is new.
func (e *Engine) Syncing() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == StateSyncing
}

// Provisional reports whether contribution diffs may be unreliable because
// the index is still on its initial partial build - true whenever it has
// never reached the caught-up state, including an initial sync now paused or
// errored, and false once it has fully synced.
func (e *Engine) Provisional() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.everReady
}

// CaughtUp reports whether the index may answer: it has completed a full sync
// and is not replaying a backlog. A half-built index is what the refusals
// guard against - it answers a partial tag set that reads like the whole
// truth - so a complete one still answers while a poll is in flight, has
// failed, or is paused, exactly as it does between two daily polls.
func (e *Engine) CaughtUp() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.everReady && e.state != StateSyncing
}

// TagFilterCached returns the last tag filter a commit fetched; nil
// until one has run. The preview reads only the cache - it never
// triggers a live fetch.
func (e *Engine) TagFilterCached() *TagFilter {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tagFilter
}

// SetTagFilterCache stores a freshly fetched tag filter for the
// preview's filtered verdicts.
func (e *Engine) SetTagFilterCache(f *TagFilter) {
	e.mu.Lock()
	e.tagFilter = f
	e.mu.Unlock()
}

// RefreshTagFilter fetches the repository's live filter and caches it
// for the preview. Called by each send before packaging, so a rule the
// janitors add tomorrow is honored with no release.
func (e *Engine) RefreshTagFilter(ctx context.Context) (*TagFilter, error) {
	f, err := e.client.FetchTagFilter(ctx)
	if err != nil {
		return nil, err
	}
	e.SetTagFilterCache(f)
	return f, nil
}

// PostUpdate uploads one chunk under the personal key.
func (e *Engine) PostUpdate(ctx context.Context, u *C2SUpdate) error {
	return e.client.PostUpdate(ctx, u)
}

// RefreshAccount bypasses the cache: the send job's start check must
// see a ban the moment the janitors set it, not up to a TTL later.
func (e *Engine) RefreshAccount(ctx context.Context) (*Account, error) {
	e.mu.Lock()
	e.account = nil
	e.mu.Unlock()
	return e.ContribAccount(ctx)
}

// trackContribOutcomes annotates ledger rows as the sync learns their
// fate, piggybacking the end of each caught-up cycle with zero extra
// repository requests: a suggestion whose pair is now current was
// approved (by a janitor, or someone else's identical suggestion -
// indistinguishable, equally good); a petition whose target is gone
// succeeded. A denial is invisible in the protocol, so nothing is ever
// stamped "denied".
func (e *Engine) trackContribOutcomes() {
	e.mu.Lock()
	store, contrib := e.store, e.contrib
	e.mu.Unlock()
	if store == nil || contrib == nil {
		return
	}
	rows, err := contrib.PendingOutcomeRows()
	if err != nil {
		logx.Warnf("ptr: reading pending contribution outcomes: %v", err)
		return
	}
	for _, row := range rows {
		var outcome string
		switch row.Kind {
		case ContribSibling:
			if cur, err := store.SiblingCurrent(row.Tag, row.Tag2); err == nil && cur {
				outcome = OutcomeApproved
			}
		case ContribParent:
			if cur, err := store.ParentCurrent(row.Tag, row.Tag2); err == nil && cur {
				outcome = OutcomeApproved
			}
		case ContribMappingPetition:
			if cur, err := store.HashHasRaw(hex.EncodeToString(row.Hash), row.Tag); err == nil && !cur {
				outcome = OutcomeRemoved
			}
		case ContribSiblingPetition:
			if cur, err := store.SiblingCurrent(row.Tag, row.Tag2); err == nil && !cur {
				outcome = OutcomeRemoved
			}
		case ContribParentPetition:
			if cur, err := store.ParentCurrent(row.Tag, row.Tag2); err == nil && !cur {
				outcome = OutcomeRemoved
			}
		}
		if outcome == "" {
			continue
		}
		if err := contrib.SetOutcome(row.ID, outcome); err != nil {
			logx.Warnf("ptr: stamping outcome on ledger row %d: %v", row.ID, err)
		}
	}
}
