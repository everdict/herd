//go:build linux

package cloudhypervisor_test

import (
	"testing"

	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/cloudhypervisor"
	"github.com/everdict/herd/vmm/vmmtest"
)

func TestStubAnswersNotImplemented(t *testing.T) {
	vmmtest.NotImplemented(t, cloudhypervisor.New(cloudhypervisor.Config{}), "cloud-hypervisor")
}

var _ vmm.VMM = (*cloudhypervisor.VMM)(nil)
