package tapnet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
)

// Binding is one entry of herd-egress's bindings file (-bindings): flows from Source are the actor's.
type Binding struct {
	Space  string `json:"space"`
	Name   string `json:"name"`
	UID    string `json:"uid"`
	Source string `json:"source"`
}

// Bindings is the bindings file herd-egress re-reads every interval, written by the herdlet whose VMs it serves.
// Every change rewrites the file atomically from what it holds on disk, so a herdlet that restarted keeps the
// bindings of the VMs that still run, and each change is read back before it is answered.
type Bindings struct {
	Path string

	mu sync.Mutex
}

func (b *Bindings) read() (map[string]Binding, error) {
	data, err := os.ReadFile(b.Path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Binding{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tapnet: bindings: %w", err)
	}
	var list []Binding
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("tapnet: bindings %s: %w", b.Path, err)
	}
	out := map[string]Binding{}
	for _, e := range list {
		out[e.Source] = e
	}
	return out, nil
}

func (b *Bindings) write(m map[string]Binding) error {
	list := make([]Binding, 0, len(m))
	for _, e := range m {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Source < list[j].Source })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(b.Path, data)
}

// Set binds e.Source to e's actor, replacing whatever the source was bound to.
func (b *Bindings) Set(e Binding) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, err := b.read()
	if err != nil {
		return err
	}
	if m[e.Source] == e {
		return nil
	}
	m[e.Source] = e
	if err := b.write(m); err != nil {
		return err
	}
	if got, err := b.read(); err != nil || got[e.Source] != e {
		return fmt.Errorf("tapnet: bindings %s: %s not bound after writing (%v)", b.Path, e.Source, err)
	}
	return nil
}

// Remove unbinds source and reads the file back: nil means the source is verified unbound.
func (b *Bindings) Remove(source string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, err := b.read()
	if err != nil {
		return err
	}
	if _, ok := m[source]; ok {
		delete(m, source)
		if err := b.write(m); err != nil {
			return err
		}
	}
	got, err := b.read()
	if err != nil {
		return err
	}
	if _, ok := got[source]; ok {
		return fmt.Errorf("tapnet: bindings %s: %s still bound after its removal", b.Path, source)
	}
	return nil
}

// Get answers the binding of source, if any.
func (b *Bindings) Get(source string) (Binding, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, err := b.read()
	if err != nil {
		return Binding{}, false, err
	}
	e, ok := m[source]
	return e, ok, nil
}
