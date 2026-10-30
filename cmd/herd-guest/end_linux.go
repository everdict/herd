package main

import "golang.org/x/sys/unix"

// endVM ends the microVM at once: a restart, which Cloud Hypervisor and Virtualization.framework both answer by
// ending the VM (as panic=-1 does), and which needs neither ACPI nor PSCI power-off in the guest kernel. It returns
// only if the kernel refused.
func endVM() error {
	unix.Sync()
	return unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
}
