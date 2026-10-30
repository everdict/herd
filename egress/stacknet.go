package egress

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/netip"
	"os"
	"sync"
)

// Addresses every VM's link on a macOS host uses: each VM has a stack of its own, so they never meet.
var (
	DefaultStackGateway = netip.MustParseAddr("192.168.127.1")
	DefaultStackVM      = netip.MustParseAddr("192.168.127.2")
)

// StackNetwork gives each VM on a macOS host its network device: a Stack from Factory whose flows the Gateway
// serves as the VM's actor. Attach is vmm/vz's Config.Network and Release its Config.ReleaseNetwork; Assign tells it
// which actor a VM is (herdlet learns it from the work herd-api sends). A VM may be attached before it has an actor
// (a warm VM booted ahead of any actor): until Assign, every flow it opens is refused.
type StackNetwork struct {
	Gateway *Gateway
	Factory StackFactory
	// GatewayAddr and VMAddr are the link's addresses; zero means DefaultStackGateway and DefaultStackVM.
	GatewayAddr, VMAddr netip.Addr

	mu     sync.Mutex
	actors map[string]Actor
	vms    map[string]*stackVM
}

type stackVM struct {
	stack  Stack
	cancel context.CancelFunc
	done   chan struct{}
}

// Assign records that VM vm is actor a's and binds the actor in the gateway (deny-all until the Syncer reads its
// policy).
func (n *StackNetwork) Assign(vm string, a Actor) {
	n.mu.Lock()
	if n.actors == nil {
		n.actors = map[string]Actor{}
	}
	n.actors[vm] = a
	n.mu.Unlock()
	if n.Gateway != nil {
		n.Gateway.Bind(a)
	}
}

func (n *StackNetwork) actorOf(vm string) (Actor, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	a, ok := n.actors[vm]
	return a, ok
}

// Attach makes VM vm's stack, serves its flows through the gateway as the VM's actor, and returns the file the
// hypervisor attaches. The actor is looked up per flow: a flow of a VM with no actor yet is refused (closed).
func (n *StackNetwork) Attach(vm string) (*os.File, error) {
	if n.Gateway == nil || n.Factory == nil {
		return nil, errors.New("egress: StackNetwork needs a Gateway and a Factory")
	}
	n.mu.Lock()
	_, running := n.vms[vm]
	a := n.actors[vm]
	n.mu.Unlock()
	if running {
		return nil, fmt.Errorf("egress: VM %s already has a network", vm)
	}
	gw, addr := n.GatewayAddr, n.VMAddr
	if !gw.IsValid() {
		gw = DefaultStackGateway
	}
	if !addr.IsValid() {
		addr = DefaultStackVM
	}
	s, file, err := n.Factory.NewStack(a, StackConfig{Gateway: gw, VM: addr, DNS: SyntheticDNS(gw)})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	v := &stackVM{stack: s, cancel: cancel, done: make(chan struct{})}
	n.mu.Lock()
	if n.vms == nil {
		n.vms = map[string]*stackVM{}
	}
	n.vms[vm] = v
	n.mu.Unlock()
	go func() {
		defer close(v.done)
		stop := context.AfterFunc(ctx, func() { _ = s.Close() })
		defer stop()
		for {
			f, err := s.Accept(ctx)
			if err != nil {
				return
			}
			a, ok := n.actorOf(vm)
			if !ok {
				_ = f.Conn.Close()
				continue
			}
			go n.Gateway.ServeConn(ctx, a, f.Conn, f.Dst)
		}
	}()
	return file, nil
}

// Release ends VM vm's network: its stack closes and its actor is unbound and forgotten, so nothing it opened
// stays served.
func (n *StackNetwork) Release(vm string) {
	n.mu.Lock()
	v, ok := n.vms[vm]
	delete(n.vms, vm)
	a, assigned := n.actors[vm]
	delete(n.actors, vm)
	n.mu.Unlock()
	if ok {
		v.cancel()
		_ = v.stack.Close()
		<-v.done
	}
	if assigned {
		n.Gateway.Unbind(a)
	}
}

// Dropped is how many packets VM vm's stack dropped; zero for a VM without one.
func (n *StackNetwork) Dropped(vm string) int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	if v, ok := n.vms[vm]; ok {
		return v.stack.Dropped()
	}
	return 0
}

// StackMAC is the MAC address of VM vm's NIC on a macOS host: locally administered (02:68:76, "hv"), from the VM's
// id, so the hypervisor that attaches the NIC (vz.Config.NetworkMAC) and herd-guest's plan, which finds the NIC by
// it, agree without telling each other, also for a VM restored under its id.
func StackMAC(vm string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(vm))
	s := h.Sum32()
	return fmt.Sprintf("02:68:76:%02x:%02x:%02x", byte(s>>16), byte(s>>8), byte(s))
}
