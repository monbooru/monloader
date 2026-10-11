package config

import "sync/atomic"

// Snapshots are shared and never mutated: a save Stores a changed Clone,
// so readers need no lock.
type Provider struct {
	p atomic.Pointer[Config]
}

func NewProvider(cfg *Config) *Provider {
	var pr Provider
	pr.p.Store(cfg)
	return &pr
}

func (pr *Provider) Current() *Config { return pr.p.Load() }

func (pr *Provider) Store(cfg *Config) { pr.p.Store(cfg) }
