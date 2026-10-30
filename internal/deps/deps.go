// Package deps pins modules a coming change adopts: gvisor-tap-vsock is the userspace network stack behind the VZ
// file-handle attachment (R7, PLT-118). Vendored because delegate boxes cannot fetch modules; this file goes away once
// the vz network code imports it.
package deps

import _ "github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
