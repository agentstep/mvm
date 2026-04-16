package sdk

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Client communicates with the mvm daemon over HTTP.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sets the Bearer token sent with every request.
func WithAPIKey(key string) Option {
	return func(c *Client) { c.apiKey = key }
}

// WithCACert adds a custom CA certificate (PEM file) to the TLS root pool.
// This is useful when the daemon uses a self-signed certificate.
func WithCACert(path string) Option {
	return func(c *Client) {
		caCert, err := os.ReadFile(path)
		if err != nil {
			return
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM(caCert)

		transport := c.httpClient.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		// Clone the transport so we don't mutate the default.
		cloned := transport.(*http.Transport).Clone()
		if cloned.TLSClientConfig == nil {
			cloned.TLSClientConfig = &tls.Config{}
		}
		cloned.TLSClientConfig.RootCAs = pool
		c.httpClient.Transport = cloned
	}
}

// WithHTTPClient replaces the default http.Client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// New creates a Client that targets the given base URL (e.g. "https://localhost:19876").
// Options are applied in order; WithHTTPClient should come before WithCACert
// if both are used.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// APIError is returned when the daemon responds with a non-2xx status.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("mvm api %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("mvm api %d", e.StatusCode)
}

// do executes an HTTP request, decoding the JSON response into out (if non-nil).
// It returns an *APIError for non-2xx responses.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		return &APIError{StatusCode: resp.StatusCode, Message: errBody.Error}
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	} else {
		// Drain body so connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return nil
}

// Health checks whether the daemon is reachable and healthy.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/health", nil, nil)
}

// CreateVM creates a new virtual machine.
func (c *Client) CreateVM(ctx context.Context, req CreateVMRequest) (*VMResponse, error) {
	var out VMResponse
	if err := c.do(ctx, http.MethodPost, "/vms", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListVMs returns all known VMs.
func (c *Client) ListVMs(ctx context.Context) ([]VMResponse, error) {
	var out []VMResponse
	if err := c.do(ctx, http.MethodGet, "/vms", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteVM destroys a VM by name.
func (c *Client) DeleteVM(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/vms/%s", name), nil, nil)
}

// StopVM stops a VM. If force is true the VM is killed immediately.
func (c *Client) StopVM(ctx context.Context, name string, force bool) error {
	body := struct {
		Force bool `json:"force,omitempty"`
	}{Force: force}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/vms/%s/stop", name), body, nil)
}

// PauseVM pauses a running VM.
func (c *Client) PauseVM(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/vms/%s/pause", name), nil, nil)
}

// ResumeVM resumes a paused VM.
func (c *Client) ResumeVM(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/vms/%s/resume", name), nil, nil)
}

// Exec runs a command inside a VM and returns the result.
func (c *Client) Exec(ctx context.Context, name, command string) (*ExecResult, error) {
	body := struct {
		Command string `json:"command"`
	}{Command: command}
	var out ExecResult
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/vms/%s/exec", name), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SnapshotCreate creates a snapshot of a VM.
func (c *Client) SnapshotCreate(ctx context.Context, vmName, snapName string) error {
	body := struct {
		Name string `json:"name,omitempty"`
	}{Name: snapName}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/vms/%s/snapshot", vmName), body, nil)
}

// SnapshotRestore restores a VM from a named snapshot.
func (c *Client) SnapshotRestore(ctx context.Context, vmName, snapName string) error {
	body := struct {
		Name string `json:"name"`
	}{Name: snapName}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/vms/%s/restore", vmName), body, nil)
}

// SnapshotList returns all available snapshots.
func (c *Client) SnapshotList(ctx context.Context) ([]SnapshotInfo, error) {
	var out []SnapshotInfo
	if err := c.do(ctx, http.MethodGet, "/snapshots", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SnapshotDelete removes a named snapshot.
func (c *Client) SnapshotDelete(ctx context.Context, snapName string) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/snapshots/%s", snapName), nil, nil)
}

// Build creates a custom rootfs image from a list of build steps.
func (c *Client) Build(ctx context.Context, imageName string, steps []BuildStep, sizeMB int) error {
	body := struct {
		ImageName string      `json:"image_name"`
		Steps     []BuildStep `json:"steps"`
		SizeMB    int         `json:"size_mb,omitempty"`
	}{ImageName: imageName, Steps: steps, SizeMB: sizeMB}
	return c.do(ctx, http.MethodPost, "/build", body, nil)
}

// ImageList returns all available custom rootfs images.
func (c *Client) ImageList(ctx context.Context) ([]ImageInfo, error) {
	var out []ImageInfo
	if err := c.do(ctx, http.MethodGet, "/images", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ImageDelete removes a custom rootfs image by name.
func (c *Client) ImageDelete(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/images/%s", name), nil, nil)
}

// PoolStatus returns the warm pool status.
func (c *Client) PoolStatus(ctx context.Context) (*PoolStatus, error) {
	var out PoolStatus
	if err := c.do(ctx, http.MethodGet, "/pool", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PoolWarm triggers pool warming. Returns immediately; warming happens asynchronously.
func (c *Client) PoolWarm(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/pool/warm", nil, nil)
}
