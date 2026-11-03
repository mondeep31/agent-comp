// Package firecracker implements vmm.Driver with Firecracker microVMs, each
// running under jailer in its own chroot, cgroup and (optionally) netns.
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// apiClient talks to one Firecracker process over its API unix socket.
type apiClient struct {
	http *http.Client
}

func newAPIClient(socketPath string) *apiClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:    1,
		IdleConnTimeout: 30 * time.Second,
	}
	return &apiClient{http: &http.Client{Transport: transport}}
}

// APIError is a non-2xx response from Firecracker.
type APIError struct {
	Method, Path string
	Status       int
	Fault        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("firecracker %s %s: %d %s", e.Method, e.Path, e.Status, e.Fault)
}

func (c *apiClient) do(ctx context.Context, method, path string, body any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding %s %s: %w", method, path, err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	var fault struct {
		FaultMessage string `json:"fault_message"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(raw, &fault) != nil || fault.FaultMessage == "" {
		fault.FaultMessage = string(raw)
	}
	return &APIError{Method: method, Path: path, Status: resp.StatusCode, Fault: fault.FaultMessage}
}

func (c *apiClient) put(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPut, path, body)
}

func (c *apiClient) patch(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPatch, path, body)
}

func (c *apiClient) close() {
	c.http.CloseIdleConnections()
}

// Request bodies for the Firecracker API (v1.x). Only the fields we use.

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

type tokenBucket struct {
	Size       int64 `json:"size"`
	RefillTime int64 `json:"refill_time"`
}

type rateLimiter struct {
	Bandwidth *tokenBucket `json:"bandwidth,omitempty"`
}

// bandwidthLimiter returns a limiter of bytesPerSec, or nil for unlimited.
func bandwidthLimiter(bytesPerSec int64) *rateLimiter {
	if bytesPerSec <= 0 {
		return nil
	}
	return &rateLimiter{Bandwidth: &tokenBucket{Size: bytesPerSec, RefillTime: 1000}}
}

type drive struct {
	DriveID      string       `json:"drive_id"`
	PathOnHost   string       `json:"path_on_host"`
	IsRootDevice bool         `json:"is_root_device"`
	IsReadOnly   bool         `json:"is_read_only"`
	RateLimiter  *rateLimiter `json:"rate_limiter,omitempty"`
}

type machineConfig struct {
	VCPUCount  int  `json:"vcpu_count"`
	MemSizeMiB int  `json:"mem_size_mib"`
	SMT        bool `json:"smt"`
}

type networkInterface struct {
	IfaceID       string       `json:"iface_id"`
	HostDevName   string       `json:"host_dev_name"`
	GuestMAC      string       `json:"guest_mac,omitempty"`
	RxRateLimiter *rateLimiter `json:"rx_rate_limiter,omitempty"`
	TxRateLimiter *rateLimiter `json:"tx_rate_limiter,omitempty"`
}

type vsockDevice struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type action struct {
	ActionType string `json:"action_type"`
}

type vmState struct {
	State string `json:"state"`
}

type snapshotCreate struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

type memBackend struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

type snapshotLoad struct {
	SnapshotPath string     `json:"snapshot_path"`
	MemBackend   memBackend `json:"mem_backend"`
	ResumeVM     bool       `json:"resume_vm"`
}
