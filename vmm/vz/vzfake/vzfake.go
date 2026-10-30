// Package vzfake is an in-memory vz.Hypervisor: machines that move through Virtualization.framework's states, a
// guest that answers on vsock after a delay (or never), save files bound to the machine identifier, and injected
// faults (a call that hangs, one that does not answer, one that fails, a stop the VM survives). It lets the vz
// backend's logic run, and the vmmtest contract pass, on every platform.
package vzfake

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/vz"
)

// Call names a hypervisor call a fault can be put on.
type Call string

const (
	CallNewMachine Call = "new-machine"
	CallStart      Call = "start"
	CallPause      Call = "pause"
	CallResume     Call = "resume"
	CallStop       Call = "stop"
	CallState      Call = "state"
	CallConnect    Call = "connect"
	CallValidate   Call = "validate"
	CallSave       Call = "save"
	CallRestore    Call = "restore"
)

// ErrAbandoned is what a hung call returns once Clear releases it; it has no effect.
var ErrAbandoned = errors.New("vzfake: hung call released")

// ErrPermissionDenied is what the R0 spike's saveMachineStateTo answered from non-GUI sessions
// (VZErrorDomain Code=11).
var ErrPermissionDenied = errors.New("Error Domain=VZErrorDomain Code=11 \"permission denied\"")

// Hypervisor is one fake Mac.
type Hypervisor struct {
	mu          sync.Mutex
	hang        map[Call]bool
	unreachable map[Call]bool
	fail        map[Call]error
	leaveOnStop bool
	release     chan struct{}

	// SaveRestoreValid is validateSaveRestoreSupport's answer.
	saveRestoreValid bool
	// guestDelay is how long after start the guest begins to answer on vsock; guestSilent means never.
	guestDelay  time.Duration
	guestSilent bool

	machines []*Machine
}

var (
	_ vz.Hypervisor = (*Hypervisor)(nil)
	_ vz.Machine    = (*Machine)(nil)
)

// New returns a fake Mac whose machines validate save/restore and whose guest answers at once.
func New() *Hypervisor {
	h := &Hypervisor{saveRestoreValid: true}
	h.Clear()
	return h
}

// Hang makes c block until Clear, then fail with ErrAbandoned.
func (h *Hypervisor) Hang(c Call) { h.set(func() { h.hang[c] = true }) }

// Unreachable makes c fail at once with vmm.ErrUnreachable.
func (h *Hypervisor) Unreachable(c Call) { h.set(func() { h.unreachable[c] = true }) }

// Fail makes c fail with err.
func (h *Hypervisor) Fail(c Call, err error) { h.set(func() { h.fail[c] = err }) }

// LeaveRunningOnStop makes Stop answer success and leave the machine running.
func (h *Hypervisor) LeaveRunningOnStop() { h.set(func() { h.leaveOnStop = true }) }

// SetSaveRestoreValid sets validateSaveRestoreSupport's answer.
func (h *Hypervisor) SetSaveRestoreValid(ok bool) { h.set(func() { h.saveRestoreValid = ok }) }

// SetGuestDelay makes guests answer on vsock only d after they start.
func (h *Hypervisor) SetGuestDelay(d time.Duration) { h.set(func() { h.guestDelay = d }) }

// SetGuestSilent makes guests never answer on vsock.
func (h *Hypervisor) SetGuestSilent(silent bool) { h.set(func() { h.guestSilent = silent }) }

// Clear removes every fault and releases hung calls. Guest timing and the save/restore answer stay.
func (h *Hypervisor) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.release != nil {
		close(h.release)
	}
	h.release = make(chan struct{})
	h.hang = map[Call]bool{}
	h.unreachable = map[Call]bool{}
	h.fail = map[Call]error{}
	h.leaveOnStop = false
}

func (h *Hypervisor) set(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn()
}

// Machines returns every machine built so far.
func (h *Hypervisor) Machines() []*Machine {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*Machine(nil), h.machines...)
}

// enter applies c's fault.
func (h *Hypervisor) enter(c Call) error {
	h.mu.Lock()
	hang, unreachable, fail, release := h.hang[c], h.unreachable[c], h.fail[c], h.release
	h.mu.Unlock()
	switch {
	case hang:
		<-release
		return fmt.Errorf("vzfake: %s: %w", c, ErrAbandoned)
	case unreachable:
		return fmt.Errorf("vzfake: %s: %w", c, vmm.ErrUnreachable)
	case fail != nil:
		return fmt.Errorf("vzfake: %s: %w", c, fail)
	}
	return nil
}

func (h *Hypervisor) NewMachine(cfg vz.MachineConfig) (vz.Machine, error) {
	if err := h.enter(CallNewMachine); err != nil {
		return nil, err
	}
	if cfg.Kernel == "" || cfg.VCPUs < 1 || cfg.MemoryMiB < 1 {
		return nil, fmt.Errorf("vzfake: invalid configuration %+v", cfg)
	}
	id := cfg.MachineID
	if id == nil {
		id = make([]byte, 16)
		_, _ = rand.Read(id)
	}
	m := &Machine{h: h, cfg: cfg, id: append([]byte(nil), id...)}
	h.mu.Lock()
	h.machines = append(h.machines, m)
	h.mu.Unlock()
	return m, nil
}

// Machine is one fake VM.
type Machine struct {
	h   *Hypervisor
	cfg vz.MachineConfig
	id  []byte

	mu        sync.Mutex
	state     vz.MachineState
	startedAt time.Time
	answered  time.Time
}

// Config is the configuration the machine was built from.
func (m *Machine) Config() vz.MachineConfig { return m.cfg }

// Answered is when the guest first accepted a vsock connection; zero if it never did.
func (m *Machine) Answered() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.answered
}

// CurrentState is the machine's state without fault injection.
func (m *Machine) CurrentState() vz.MachineState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *Machine) transition(c Call, from []vz.MachineState, to vz.MachineState) error {
	if err := m.h.enter(c); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range from {
		if m.state == f {
			m.state = to
			if to == vz.MachineRunning && c == CallStart {
				m.startedAt = time.Now()
			}
			return nil
		}
	}
	return fmt.Errorf("vzfake: %s: invalid in state %s", c, m.state)
}

func (m *Machine) Start() error {
	return m.transition(CallStart, []vz.MachineState{vz.MachineStopped, vz.MachineError}, vz.MachineRunning)
}

func (m *Machine) Pause() error {
	return m.transition(CallPause, []vz.MachineState{vz.MachineRunning}, vz.MachinePaused)
}

func (m *Machine) Resume() error {
	return m.transition(CallResume, []vz.MachineState{vz.MachinePaused}, vz.MachineRunning)
}

func (m *Machine) Stop() error {
	if err := m.h.enter(CallStop); err != nil {
		return err
	}
	m.h.mu.Lock()
	leave := m.h.leaveOnStop
	m.h.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.state {
	case vz.MachineRunning, vz.MachinePaused:
		if !leave {
			m.state = vz.MachineStopped
		}
		return nil
	}
	return fmt.Errorf("vzfake: stop: invalid in state %s", m.state)
}

func (m *Machine) State() (vz.MachineState, error) {
	if err := m.h.enter(CallState); err != nil {
		return vz.MachineError, err
	}
	return m.CurrentState(), nil
}

func (m *Machine) ConnectVsock(port uint32) (net.Conn, error) {
	if err := m.h.enter(CallConnect); err != nil {
		return nil, err
	}
	m.h.mu.Lock()
	delay, silent := m.h.guestDelay, m.h.guestSilent
	m.h.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != vz.MachineRunning || silent || time.Since(m.startedAt) < delay {
		return nil, fmt.Errorf("vzfake: vsock port %d: connection refused", port)
	}
	if m.answered.IsZero() {
		m.answered = time.Now()
	}
	host, guest := net.Pipe()
	_ = guest.Close()
	return host, nil
}

func (m *Machine) ValidateSaveRestore() (bool, error) {
	if err := m.h.enter(CallValidate); err != nil {
		return false, err
	}
	m.h.mu.Lock()
	defer m.h.mu.Unlock()
	return m.h.saveRestoreValid, nil
}

// SaveState writes the machine identifier to path: the fake save file restores only into the same machine.
func (m *Machine) SaveState(path string) error {
	if err := m.h.enter(CallSave); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != vz.MachinePaused {
		return fmt.Errorf("vzfake: save: invalid in state %s", m.state)
	}
	return os.WriteFile(path, m.id, 0o600)
}

func (m *Machine) RestoreState(path string) error {
	if err := m.h.enter(CallRestore); err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("vzfake: restore: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != vz.MachineStopped {
		return fmt.Errorf("vzfake: restore: invalid in state %s", m.state)
	}
	if string(b) != string(m.id) {
		return errors.New("vzfake: restore: the save file belongs to another machine")
	}
	m.state = vz.MachinePaused
	// A restored guest is already up: it answers on vsock at once.
	m.startedAt = time.Now().Add(-24 * time.Hour)
	return nil
}

func (m *Machine) MachineID() []byte { return append([]byte(nil), m.id...) }

// Console is a fake console session: a user whose screen locks and unlocks, the login window, or a session that
// does not answer.
type Console struct {
	mu    sync.Mutex
	state vz.Console
	err   error
	calls int
}

var _ vz.Session = (*Console)(nil)

// UnlockedConsole is user "herd" at the console with the screen unlocked.
func UnlockedConsole() *Console { return &Console{state: vz.Console{User: "herd"}} }

// Set makes the session report c.
func (s *Console) Set(c vz.Console) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state, s.err = c, nil
}

// Lock locks the screen (true) or unlocks it (false), keeping the user.
func (s *Console) Lock(locked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Locked, s.err = locked, nil
}

// Fail makes the session answer err.
func (s *Console) Fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// Calls is how many times the session was asked.
func (s *Console) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// Console implements vz.Session.
func (s *Console) Console(context.Context) (vz.Console, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.state, s.err
}
