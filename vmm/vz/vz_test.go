//go:build darwin

package vz_test

import (
	"testing"

	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/vmmtest"
	"github.com/everdict/herd/vmm/vz"
)

func TestStubAnswersNotImplemented(t *testing.T) {
	vmmtest.NotImplemented(t, vz.New(vz.Config{}), "vz")
}

var _ vmm.VMM = (*vz.VMM)(nil)
