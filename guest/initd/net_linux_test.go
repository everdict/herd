//go:build linux

package initd

import (
	"testing"

	"golang.org/x/sys/unix"
)

// routeRequest spells the rtnetlink constants out so it builds anywhere; on linux they must be the kernel's.
func TestRouteConstantsAreTheKernels(t *testing.T) {
	for name, c := range map[string][2]int{
		"RTM_NEWROUTE":      {rtmNewRoute, unix.RTM_NEWROUTE},
		"NLMSG_ERROR":       {nlmsgError, unix.NLMSG_ERROR},
		"NLM_F_REQUEST":     {nlmFRequest, unix.NLM_F_REQUEST},
		"NLM_F_ACK":         {nlmFAck, unix.NLM_F_ACK},
		"NLM_F_EXCL":        {nlmFExcl, unix.NLM_F_EXCL},
		"NLM_F_CREATE":      {nlmFCreate, unix.NLM_F_CREATE},
		"AF_INET":           {afInet, unix.AF_INET},
		"RT_TABLE_MAIN":     {rtTableMain, unix.RT_TABLE_MAIN},
		"RTPROT_BOOT":       {rtprotBoot, unix.RTPROT_BOOT},
		"RT_SCOPE_UNIVERSE": {rtScopeUniver, unix.RT_SCOPE_UNIVERSE},
		"RTN_UNICAST":       {rtnUnicast, unix.RTN_UNICAST},
		"RTA_OIF":           {rtaOIF, unix.RTA_OIF},
		"RTA_GATEWAY":       {rtaGateway, unix.RTA_GATEWAY},
		"NLMSG_HDRLEN":      {nlmsgHdrLen, unix.NLMSG_HDRLEN},
		"SizeofRtMsg":       {rtmsgLen, unix.SizeofRtMsg},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %d, the kernel's is %d", name, c[0], c[1])
		}
	}
}
