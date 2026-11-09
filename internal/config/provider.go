package config

import (
	"sync"
	"sync/atomic"
)

// Snapshots are shared and never mutated: a save Stores a changed Clone,
// so readers need no lock.
type Provider struct {
	p atomic.Pointer[Config]

	mu     sync.Mutex
	path   string
	base   string
	onSave func(prev, next *Config)
}

func NewProvider(cfg *Config) *Provider {
	var pr Provider
	pr.p.Store(cfg)
	return &pr
}

func (pr *Provider) Current() *Config { return pr.p.Load() }

func (pr *Provider) Store(cfg *Config) { pr.p.Store(cfg) }

// Left unset, a save stays in memory.
func (pr *Provider) Persist(path, portableBase string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.path, pr.base = path, portableBase
}

// OnSave runs after every Update, with the update's lock held.
func (pr *Provider) OnSave(fn func(prev, next *Config)) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.onSave = fn
}

// fn runs twice, on a copy of the runtime config and on the file layer, so it
// must be idempotent. The file layer is reloaded without MONLOADER_* overrides,
// so an env value is never baked into the file; portable paths stay relative.
func (pr *Provider) Update(fn func(*Config) error) error {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	prev := pr.Current()
	next := prev.Clone()
	if err := fn(next); err != nil {
		return err
	}
	if pr.path != "" {
		persist, err := LoadFromFileIn(pr.path, pr.base)
		if err != nil {
			return err
		}
		if err := fn(persist); err != nil {
			return err
		}
		RelativizeIn(persist, pr.base)
		if err := Save(persist, pr.path); err != nil {
			return err
		}
	}
	pr.Store(next)
	if pr.onSave != nil {
		pr.onSave(prev, next)
	}
	return nil
}
