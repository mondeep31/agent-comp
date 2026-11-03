package guesttest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/mondeep31/agent-comp/internal/golden"
	"github.com/mondeep31/agent-comp/internal/pool"
	"github.com/mondeep31/agent-comp/internal/vmm"
)

// Source is a fake sandbox.VMSource. Restores from a snapshot get a copy of
// the snapshotted agent's state.
type Source struct {
	Dir string
	// Err, when set, fails every Acquire and Restore.
	Err error

	n      atomic.Int64
	mu     sync.Mutex
	vms    []*VM
	states map[string]*Agent
}

func NewSource(dir string) *Source {
	return &Source{Dir: dir, states: map[string]*Agent{}}
}

func (s *Source) newVM(key golden.Key, agent *Agent) (*pool.Instance, error) {
	if s.Err != nil {
		return nil, s.Err
	}
	id := fmt.Sprintf("sb-test%04d", s.n.Add(1))
	overlay := filepath.Join(s.Dir, id+".ext4")
	if err := os.WriteFile(overlay, []byte("overlay"), 0o600); err != nil {
		return nil, err
	}
	vm := NewVM(id, agent)
	vm.Overlay = overlay
	vm.OnSnapshot = func(files vmm.SnapshotFiles, clone *Agent) {
		s.mu.Lock()
		s.states[files.State] = clone
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.vms = append(s.vms, vm)
	s.mu.Unlock()
	return pool.NewInstance(id, key, vm, vm.Stop), nil
}

func (s *Source) Acquire(_ context.Context, key golden.Key) (*pool.Instance, error) {
	return s.newVM(key, NewAgent())
}

func (s *Source) Restore(_ context.Context, snap *golden.Snapshot) (*pool.Instance, error) {
	s.mu.Lock()
	agent := s.states[snap.Files.State]
	s.mu.Unlock()
	if agent == nil {
		return nil, fmt.Errorf("guesttest: unknown snapshot %s", snap.Files.State)
	}
	return s.newVM(snap.Key, agent.Clone())
}

// VMs returns every VM created so far.
func (s *Source) VMs() []*VM {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*VM(nil), s.vms...)
}
