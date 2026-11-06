//go:build linux

package agent

import (
	"time"

	"golang.org/x/sys/unix"
)

// setTime steps CLOCK_REALTIME to t (clock_settime(2), which needs CAP_SYS_TIME: herd-guest runs as the VM's root)
// and answers the clock it read just before.
func setTime(t time.Time) (time.Time, error) {
	before := time.Now()
	ts := unix.NsecToTimespec(t.UnixNano())
	return before, unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}
