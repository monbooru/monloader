package ptr

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/logx"
)

var ErrAccountExists = errors.New("a personal account key is already set")

var errNoAutoCreation = errors.New("the repository offers no auto-creatable account type")

type ContribStatus struct {
	Account bool `json:"account"`
	Banned  bool `json:"banned"`
	Unsent  int  `json:"unsent"`
	Failed  int  `json:"failed"`
}

const accountCacheTTL = time.Minute

func (e *Engine) HasPersonalKey() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hasPersonalKeyLocked()
}

func (e *Engine) hasPersonalKeyLocked() bool {
	return e.cfg.AccessKey != "" && e.cfg.AccessKey != config.PublicAccessKey
}

// AccessKey is a credential: show it only on explicit reveal, never log it.
func (e *Engine) AccessKey() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.hasPersonalKeyLocked() {
		return ""
	}
	return e.cfg.AccessKey
}

func (e *Engine) SetAccessKey(key string) {
	e.mu.Lock()
	e.cfg.AccessKey = key
	e.account = nil
	e.accountAt = time.Time{}
	e.mu.Unlock()
	e.client.SetAccessKey(config.PTRConfig{AccessKey: key}.EffectiveAccessKey())
}

// CreateContribAccount's caller must persist the key. Replacing an account
// stays manual: fresh accounts to dodge a ban are what the PTR bans for.
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
		return "", errNoAutoCreation
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
	// The first authenticated use materializes the account server-side
	// and seeds the cached state. A failed read is no failed creation: the
	// redemption is single-use, so the caller must still save the key.
	if _, err := e.ContribAccount(ctx); err != nil {
		logx.Warnf("ptr: account created but not yet readable: %v", err)
	}
	return accessKey, nil
}

func (e *Engine) ContribAccount(ctx context.Context) (*Account, error) {
	return e.fetchAccount(ctx, false)
}

func (e *Engine) fetchAccount(ctx context.Context, fresh bool) (*Account, error) {
	e.mu.Lock()
	if !e.hasPersonalKeyLocked() {
		e.mu.Unlock()
		return nil, errors.New("no personal account key is set")
	}
	if !fresh && e.account != nil && time.Since(e.accountAt) < accountCacheTTL {
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

// Contrib returns nil while the PTR is disabled or its store failed to open.
func (e *Engine) Contrib() *ContribStore {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.contrib
}

var errIndexUnavailable = errors.New("the ptr index is not available")

func (e *Engine) indexStore() (*Store, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.store == nil {
		return nil, errIndexUnavailable
	}
	return e.store, nil
}

func onIndex[T any](e *Engine, fn func(*Store) (T, error)) (T, error) {
	s, err := e.indexStore()
	if err != nil {
		var zero T
		return zero, err
	}
	return fn(s)
}

func (e *Engine) HashHasIdeal(hashHex, tag string) (bool, error) {
	return onIndex(e, func(s *Store) (bool, error) { return s.HashHasIdeal(hashHex, tag) })
}

func (e *Engine) HashHasIdeals(hashHex string, tags []string) (map[string]bool, error) {
	return onIndex(e, func(s *Store) (map[string]bool, error) { return s.HashHasIdeals(hashHex, tags) })
}

func (e *Engine) IdealTag(tag string) (string, bool, error) {
	s, err := e.indexStore()
	if err != nil {
		return "", false, err
	}
	return s.IdealTag(tag)
}

func (e *Engine) HashHasRaw(hashHex, tag string) (bool, error) {
	return onIndex(e, func(s *Store) (bool, error) { return s.HashHasRaw(hashHex, tag) })
}

func (e *Engine) RawTagsForHash(hashHex string) ([]string, error) {
	return onIndex(e, func(s *Store) ([]string, error) { return s.RawTagsForHash(hashHex) })
}

func (e *Engine) SiblingCurrent(bad, good string) (bool, error) {
	return onIndex(e, func(s *Store) (bool, error) { return s.SiblingCurrent(bad, good) })
}

func (e *Engine) ParentCurrent(child, parent string) (bool, error) {
	return onIndex(e, func(s *Store) (bool, error) { return s.ParentCurrent(child, parent) })
}

func (e *Engine) ParentEdgeCovered(child, parent string) (bool, error) {
	return onIndex(e, func(s *Store) (bool, error) { return s.ParentEdgeCovered(child, parent) })
}

func (e *Engine) Syncing() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == StateSyncing
}

func (e *Engine) Provisional() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.everReady
}

// CaughtUp holds through a poll, an error or a pause. Only a partial
// build or a backlog replay refuses: its partial tag set would read as
// the whole truth.
func (e *Engine) CaughtUp() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.caughtUpLocked()
}

func (e *Engine) caughtUpLocked() bool {
	return e.everReady && e.state != StateSyncing
}

// TagFilterCached returns nil until a send has fetched the filter.
func (e *Engine) TagFilterCached() *TagFilter {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tagFilter
}

func (e *Engine) SetTagFilterCache(f *TagFilter) {
	e.mu.Lock()
	e.tagFilter = f
	e.mu.Unlock()
}

// RefreshTagFilter runs before each send's packaging, so a rule the
// janitors add applies with no release.
func (e *Engine) RefreshTagFilter(ctx context.Context) (*TagFilter, error) {
	f, err := e.client.FetchTagFilter(ctx)
	if err != nil {
		return nil, err
	}
	e.SetTagFilterCache(f)
	return f, nil
}

func (e *Engine) PostUpdate(ctx context.Context, u *C2SUpdate) error {
	return e.client.PostUpdate(ctx, u)
}

// RefreshAccount skips the cache: a send must see a ban at once, not a
// TTL later. A failed fetch keeps the cached account, whose ban the send
// gate falls back on.
func (e *Engine) RefreshAccount(ctx context.Context) (*Account, error) {
	return e.fetchAccount(ctx, true)
}

// A denial is invisible in the protocol, so rows are only ever stamped
// approved or removed.
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
