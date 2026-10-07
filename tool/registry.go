package tool

import (
	"sort"
	"sync"
)

type Registry struct {
	mu      sync.RWMutex
	tools   map[string]Tool
	plugins map[string]Plugin
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool), plugins: make(map[string]Plugin)}
}

func (r *Registry) Mount(p Plugin) {
	if r == nil || p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plugins[p.Name()] = p
	for _, t := range p.Tools() {
		if t != nil {
			r.tools[t.ID()] = t
		}
	}
}

func (r *Registry) Unmount(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.plugins[name]
	if !ok {
		return
	}
	delete(r.plugins, name)
	for _, t := range p.Tools() {
		delete(r.tools, t.ID())
	}
}

func (r *Registry) Register(t Tool) {
	if r == nil || t == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.ID()] = t
}

func (r *Registry) Get(id string) (Tool, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[id]
	return t, ok
}

func (r *Registry) All() []Tool {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, t)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID() < result[j].ID() })
	return result
}

// Schemas exports registered tools in the common function-calling schema.
func (r *Registry) Schemas() []map[string]any {
	tools := r.All()
	result := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		result = append(result, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": t.ID(), "description": t.Description(), "parameters": t.Parameters(),
			},
		})
	}
	return result
}
