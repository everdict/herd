//go:build !linux

package agent

import (
	"errors"
	"time"
)

// setTime: herd-guest sets a clock only in a linux VM.
func setTime(time.Time) (time.Time, error) {
	return time.Now(), errors.New("setting the clock is linux only")
}
