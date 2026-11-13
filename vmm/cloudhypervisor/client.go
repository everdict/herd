//go:build linux

package cloudhypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/everdict/herd/vmm"
)

// The Cloud Hypervisor v54 REST endpoints this backend uses (vmm/src/api/openapi/cloud-hypervisor.yaml at v54.0).
const (
	epPing        = "vmm.ping"
	epVMMShutdown = "vmm.shutdown"
	epCreate      = "vm.create"
	epBoot        = "vm.boot"
	epPause       = "vm.pause"
	epResume      = "vm.resume"
	epSnapshot    = "vm.snapshot"
	epRestore     = "vm.restore"
	epShutdown    = "vm.shutdown"
)

// APIError is a non-2xx answer from Cloud Hypervisor: the VMM answered and refused.
type APIError struct {
	Endpoint string
	Status   int
	Body     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cloud-hypervisor: %s: HTTP %d: %s", e.Endpoint, e.Status, e.Body)
}

// client is a minimal Cloud Hypervisor API client over the VMM's unix socket (ADR A-9: no generated client module).
// Every call is bounded by its context. A call that did not get an answer is never reported as success: it fails
// with the context's error when the context ended, and with vmm.ErrUnreachable otherwise.
type client struct {
	socket string
	http   *http.Client
}

func newClient(socket string) *client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		// One connection per call: a VMM that was killed or hung leaves nothing pooled behind.
		DisableKeepAlives: true,
	}
	return &client{socket: socket, http: &http.Client{Transport: tr}}
}

// call sends one request. in, if not nil, is sent as JSON; out, if not nil, receives the JSON answer.
func (c *client) call(ctx context.Context, method, endpoint string, in, out any) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cloud-hypervisor: %s: %w", endpoint, err)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("cloud-hypervisor: %s: encode: %w", endpoint, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost/api/v1/"+endpoint, body)
	if err != nil {
		return fmt.Errorf("cloud-hypervisor: %s: %w", endpoint, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.noAnswer(ctx, endpoint, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return c.noAnswer(ctx, endpoint, err)
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{Endpoint: endpoint, Status: resp.StatusCode, Body: string(bytes.TrimSpace(data))}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("cloud-hypervisor: %s: decode answer: %w", endpoint, err)
		}
	}
	return nil
}

func (c *client) noAnswer(ctx context.Context, endpoint string, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("cloud-hypervisor: %s: %w (%v)", endpoint, cerr, err)
	}
	return fmt.Errorf("cloud-hypervisor: %s on %s: %w: %v", endpoint, c.socket, vmm.ErrUnreachable, err)
}

func (c *client) put(ctx context.Context, endpoint string, in any) error {
	return c.call(ctx, http.MethodPut, endpoint, in, nil)
}

// pingResponse is VmmPingResponse.
type pingResponse struct {
	BuildVersion string   `json:"build_version,omitempty"`
	Version      string   `json:"version"`
	PID          int64    `json:"pid,omitempty"`
	Features     []string `json:"features,omitempty"`
}

func (c *client) ping(ctx context.Context) (pingResponse, error) {
	var p pingResponse
	err := c.call(ctx, http.MethodGet, epPing, nil, &p)
	return p, err
}

// The subset of VmConfig herd sends.
type vmConfig struct {
	Payload payloadConfig  `json:"payload"`
	CPUs    cpusConfig     `json:"cpus"`
	Memory  memoryConfig   `json:"memory"`
	Balloon *balloonConfig `json:"balloon,omitempty"`
	Disks   []diskConfig   `json:"disks,omitempty"`
	Fs      []fsConfig     `json:"fs,omitempty"`
	Net     []netConfig    `json:"net,omitempty"`
	Vsock   *vsockConfig   `json:"vsock,omitempty"`
	Serial  *consoleConfig `json:"serial,omitempty"`
	Console *consoleConfig `json:"console,omitempty"`
}

type payloadConfig struct {
	Kernel    string `json:"kernel"`
	Initramfs string `json:"initramfs,omitempty"`
	Cmdline   string `json:"cmdline,omitempty"`
}

type cpusConfig struct {
	BootVCPUs int `json:"boot_vcpus"`
	MaxVCPUs  int `json:"max_vcpus"`
}

type memoryConfig struct {
	Size int64 `json:"size"`
	// Shared is required by vhost-user devices (virtio-fs) and by snapshots that carry them.
	Shared bool `json:"shared"`
}

// balloonConfig is a virtio-balloon device. Size is how much of the guest's memory the balloon takes (0: none);
// FreePageReporting has the guest report the pages it frees, which Cloud Hypervisor returns to the host.
type balloonConfig struct {
	Size              int64 `json:"size"`
	FreePageReporting bool  `json:"free_page_reporting"`
}

type diskConfig struct {
	Path string `json:"path"`
	// ImageType must be set for a raw image on v54 (R0 spike: image_type=raw).
	ImageType string `json:"image_type"`
	Readonly  bool   `json:"readonly,omitempty"`
}

type fsConfig struct {
	Tag       string `json:"tag"`
	Socket    string `json:"socket"`
	NumQueues int    `json:"num_queues"`
	QueueSize int    `json:"queue_size"`
}

// netConfig is a virtio-net device on an existing TAP (--net tap=<tap>,mac=<mac>). No ip or mask: the host side
// of the TAP is herdlet's (egress/tapnet), configured before the VMM starts.
type netConfig struct {
	Tap string `json:"tap"`
	MAC string `json:"mac"`
}

type vsockConfig struct {
	CID    int64  `json:"cid"`
	Socket string `json:"socket"`
}

type consoleConfig struct {
	Mode string `json:"mode"`
	File string `json:"file,omitempty"`
}

type snapshotConfig struct {
	DestinationURL string `json:"destination_url"`
}

type restoreConfig struct {
	SourceURL string `json:"source_url"`
	Prefault  bool   `json:"prefault"`
	// MemoryRestoreMode Copy is the eager restore: OnDemand (userfaultfd) is unavailable with vhost-user devices.
	MemoryRestoreMode string `json:"memory_restore_mode,omitempty"`
	Resume            bool   `json:"resume"`
}

// isAPIError reports whether err is an answer Cloud Hypervisor gave (as opposed to no answer).
func isAPIError(err error) bool {
	var ae *APIError
	return errors.As(err, &ae)
}
