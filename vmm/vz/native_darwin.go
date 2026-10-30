//go:build darwin && arm64 && cgo

package vz

import (
	"errors"
	"fmt"
	"net"

	vzf "github.com/Code-Hex/vz/v3"
)

// Native returns the Virtualization.framework hypervisor (Code-Hex/vz, ADR A-9). The binary must be signed with the
// com.apple.security.virtualization entitlement (an ad-hoc signature is enough to boot; see vmm/vz/vz.entitlements).
func Native() Hypervisor { return native{} }

type native struct{}

func (native) NewMachine(cfg MachineConfig) (Machine, error) {
	opts := []vzf.LinuxBootLoaderOption{vzf.WithCommandLine(cfg.Cmdline)}
	if cfg.Initrd != "" {
		opts = append(opts, vzf.WithInitrd(cfg.Initrd))
	}
	boot, err := vzf.NewLinuxBootLoader(cfg.Kernel, opts...)
	if err != nil {
		return nil, fmt.Errorf("boot loader: %w", err)
	}
	conf, err := vzf.NewVirtualMachineConfiguration(boot, uint(cfg.VCPUs), uint64(cfg.MemoryMiB)*1024*1024)
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}

	// A generic platform with a machine identifier the save file is bound to.
	var mid *vzf.GenericMachineIdentifier
	if len(cfg.MachineID) > 0 {
		mid, err = vzf.NewGenericMachineIdentifierWithData(cfg.MachineID)
	} else {
		mid, err = vzf.NewGenericMachineIdentifier()
	}
	if err != nil {
		return nil, fmt.Errorf("machine identifier: %w", err)
	}
	platform, err := vzf.NewGenericPlatformConfiguration(vzf.WithGenericMachineIdentifier(mid))
	if err != nil {
		return nil, fmt.Errorf("platform: %w", err)
	}
	conf.SetPlatformVirtualMachineConfiguration(platform)

	if cfg.Console != "" {
		att, err := vzf.NewFileSerialPortAttachment(cfg.Console, true)
		if err != nil {
			return nil, fmt.Errorf("console: %w", err)
		}
		port, err := vzf.NewVirtioConsoleDeviceSerialPortConfiguration(att)
		if err != nil {
			return nil, fmt.Errorf("console: %w", err)
		}
		conf.SetSerialPortsVirtualMachineConfiguration([]*vzf.VirtioConsoleDeviceSerialPortConfiguration{port})
	}

	var disks []vzf.StorageDeviceConfiguration
	for _, d := range cfg.Disks {
		att, err := vzf.NewDiskImageStorageDeviceAttachment(d.Path, d.ReadOnly)
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", d.Path, err)
		}
		blk, err := vzf.NewVirtioBlockDeviceConfiguration(att)
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", d.Path, err)
		}
		disks = append(disks, blk)
	}
	conf.SetStorageDevicesVirtualMachineConfiguration(disks)

	var shares []vzf.DirectorySharingDeviceConfiguration
	for _, s := range cfg.Shares {
		dev, err := vzf.NewVirtioFileSystemDeviceConfiguration(s.Tag)
		if err != nil {
			return nil, fmt.Errorf("share %s: %w", s.Tag, err)
		}
		dir, err := vzf.NewSharedDirectory(s.HostPath, s.ReadOnly)
		if err != nil {
			return nil, fmt.Errorf("share %s: %w", s.Tag, err)
		}
		single, err := vzf.NewSingleDirectoryShare(dir)
		if err != nil {
			return nil, fmt.Errorf("share %s: %w", s.Tag, err)
		}
		dev.SetDirectoryShare(single)
		shares = append(shares, dev)
	}
	conf.SetDirectorySharingDevicesVirtualMachineConfiguration(shares)

	sock, err := vzf.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vsock: %w", err)
	}
	conf.SetSocketDevicesVirtualMachineConfiguration([]vzf.SocketDeviceConfiguration{sock})

	entropy, err := vzf.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("entropy: %w", err)
	}
	conf.SetEntropyDevicesVirtualMachineConfiguration([]*vzf.VirtioEntropyDeviceConfiguration{entropy})

	if cfg.Network != nil {
		att, err := vzf.NewFileHandleNetworkDeviceAttachment(cfg.Network)
		if err != nil {
			return nil, fmt.Errorf("network: %w", err)
		}
		nic, err := vzf.NewVirtioNetworkDeviceConfiguration(att)
		if err != nil {
			return nil, fmt.Errorf("network: %w", err)
		}
		conf.SetNetworkDevicesVirtualMachineConfiguration([]*vzf.VirtioNetworkDeviceConfiguration{nic})
	}

	if ok, err := conf.Validate(); err != nil || !ok {
		return nil, fmt.Errorf("configuration not valid: %v", err)
	}
	vm, err := vzf.NewVirtualMachine(conf)
	if err != nil {
		return nil, err
	}
	return &nativeMachine{vm: vm, conf: conf, id: append([]byte(nil), mid.DataRepresentation()...)}, nil
}

type nativeMachine struct {
	vm   *vzf.VirtualMachine
	conf *vzf.VirtualMachineConfiguration
	id   []byte
}

func (m *nativeMachine) Start() error  { return m.vm.Start() }
func (m *nativeMachine) Pause() error  { return m.vm.Pause() }
func (m *nativeMachine) Resume() error { return m.vm.Resume() }
func (m *nativeMachine) Stop() error   { return m.vm.Stop() }

// State maps VZVirtualMachineState; Code-Hex/vz's numbering is the framework's.
func (m *nativeMachine) State() (MachineState, error) {
	switch st := m.vm.State(); st {
	case vzf.VirtualMachineStateStopped:
		return MachineStopped, nil
	case vzf.VirtualMachineStateRunning:
		return MachineRunning, nil
	case vzf.VirtualMachineStatePaused:
		return MachinePaused, nil
	case vzf.VirtualMachineStateError:
		return MachineError, nil
	case vzf.VirtualMachineStateStarting:
		return MachineStarting, nil
	case vzf.VirtualMachineStatePausing:
		return MachinePausing, nil
	case vzf.VirtualMachineStateResuming:
		return MachineResuming, nil
	case vzf.VirtualMachineStateStopping:
		return MachineStopping, nil
	case vzf.VirtualMachineStateSaving:
		return MachineSaving, nil
	case vzf.VirtualMachineStateRestoring:
		return MachineRestoring, nil
	default:
		return MachineError, fmt.Errorf("unknown VZVirtualMachineState %d", int(st))
	}
}

func (m *nativeMachine) ConnectVsock(port uint32) (net.Conn, error) {
	devs := m.vm.SocketDevices()
	if len(devs) == 0 {
		return nil, errors.New("no vsock device")
	}
	c, err := devs[0].Connect(port)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (m *nativeMachine) ValidateSaveRestore() (bool, error) {
	return m.conf.ValidateSaveRestoreSupport()
}

func (m *nativeMachine) SaveState(path string) error { return m.vm.SaveMachineStateToPath(path) }

func (m *nativeMachine) RestoreState(path string) error { return m.vm.RestoreMachineStateFromURL(path) }

func (m *nativeMachine) MachineID() []byte { return append([]byte(nil), m.id...) }
