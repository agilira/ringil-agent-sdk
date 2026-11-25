// live.go: tools the host changes while the agent runs
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"sync"

	"github.com/agilira/go-errors"
)

// LiveRegistry is a Registry whose tools the host changes while the agent
// runs: a skill or an MCP server installed during a conversation is there
// at the next model call, not at the next session. A run takes the tools
// of the moment before each model call; the calls of that turn run on the
// tools the model was shown. Set it as Config.Tools; it is safe for
// concurrent use.
type LiveRegistry struct {
	mu      sync.RWMutex
	reg     Registry
	version uint64
}

// NewLiveRegistry starts from reg; nil is no tools.
func NewLiveRegistry(reg Registry) *LiveRegistry {
	if reg == nil {
		reg = &registry{entries: map[string]entry{}}
	}
	return &LiveRegistry{reg: reg}
}

// Set replaces every tool with reg's.
func (l *LiveRegistry) Set(reg Registry) {
	if reg == nil {
		reg = &registry{entries: map[string]entry{}}
	}
	l.mu.Lock()
	l.reg, l.version = reg, l.version+1
	l.mu.Unlock()
}

// Add adds tools; a name already taken refuses them all.
func (l *LiveRegistry) Add(tools ...Tool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	next, err := l.reg.With(tools...)
	if err != nil {
		return err
	}
	l.reg, l.version = next, l.version+1
	return nil
}

// Remove drops the named tools; names not registered are ignored.
func (l *LiveRegistry) Remove(names ...string) error {
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var keep []string
	for _, s := range l.reg.Specs() {
		if !drop[s.Name] {
			keep = append(keep, s.Name)
		}
	}
	next, err := l.reg.Select(keep...)
	if err != nil {
		return errors.Wrap(err, ErrToolConfig, "cannot remove tools")
	}
	l.reg, l.version = next, l.version+1
	return nil
}

// snapshot is the registry of the moment and its version.
func (l *LiveRegistry) snapshot() (Registry, uint64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.reg, l.version
}

func (l *LiveRegistry) current() Registry {
	reg, _ := l.snapshot()
	return reg
}

// Specs, Has and Execute answer for the tools of the moment.
func (l *LiveRegistry) Specs() []ToolSpec { return l.current().Specs() }

// Has reports whether name is registered now.
func (l *LiveRegistry) Has(name string) bool { return l.current().Has(name) }

// Execute runs call on the tools of the moment.
func (l *LiveRegistry) Execute(ctx context.Context, call ToolCall) (ToolResult, error) {
	return l.current().Execute(ctx, call)
}

// With and Select return a fixed registry made from the tools of the
// moment, as for a worker's subset; it does not follow later changes.
func (l *LiveRegistry) With(tools ...Tool) (Registry, error) { return l.current().With(tools...) }

// Select returns a fixed registry of the named tools of the moment.
func (l *LiveRegistry) Select(names ...string) (Registry, error) {
	return l.current().Select(names...)
}

func (l *LiveRegistry) canonicalArgs(call ToolCall) ([]byte, bool) {
	if c, ok := l.current().(argsCanonicalizer); ok {
		return c.canonicalArgs(call)
	}
	return nil, false
}
