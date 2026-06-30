package main

import "sync/atomic"

// ConfigHolder holds the live *Config and allows lock-free reads with atomic swap.
type ConfigHolder struct {
	p atomic.Pointer[Config]
}

func NewConfigHolder(c *Config) *ConfigHolder {
	h := &ConfigHolder{}
	h.p.Store(c)
	return h
}

func (h *ConfigHolder) Get() *Config  { return h.p.Load() }
func (h *ConfigHolder) Set(c *Config) { h.p.Store(c) }
