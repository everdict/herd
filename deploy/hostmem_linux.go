//go:build linux

package deploy

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// hostMemoryMiB is MemTotal from /proc/meminfo; 0 when it cannot be read.
func hostMemoryMiB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return kb / 1024
		}
	}
	return 0
}
