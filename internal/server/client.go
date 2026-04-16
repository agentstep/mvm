package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/agentstep/mvm/internal/agentclient"
	"github.com/agentstep/mvm/internal/firecracker"
)

// Client communicates with the mvm daemon via Unix socket or remote TCP+TLS.
type Client struct {
	httpClient *http.Client
	socketPath string
	remoteURL  string // e.g. "https://server:19876"
	apiKey     string
	caCertPath string
}

func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.DialTimeout("unix", socketPath, 2*time.Second)
				},
			},
		},
	}
}

// authRoundTripper wraps an http.RoundTripper to inject an Authorization header.
type authRoundTripper struct {
	base   http.RoundTripper
	apiKey string
}

func (a *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	return a.base.RoundTrip(req)
}

// NewRemoteClient creates a client that connects to a remote daemon over TCP (optionally TLS).
func NewRemoteClient(remoteURL, apiKey, caCertPath string) *Client {
	c := &Client{
		remoteURL:  strings.TrimRight(remoteURL, "/"),
		apiKey:     apiKey,
		caCertPath: caCertPath,
	}

	transport := &http.Transport{
		TLSClientConfig: c.tlsConfig(),
	}

	var rt http.RoundTripper = transport
	if apiKey != "" {
		rt = &authRoundTripper{base: transport, apiKey: apiKey}
	}

	c.httpClient = &http.Client{Transport: rt}
	return c
}

// url returns the full URL for a given API path, using the remote URL if set.
func (c *Client) url(path string) string {
	if c.remoteURL != "" {
		return c.remoteURL + path
	}
	return "http://mvm" + path
}

// tlsConfig returns a TLS configuration, optionally loading a custom CA cert.
func (c *Client) tlsConfig() *tls.Config {
	tlsConf := &tls.Config{}
	if c.caCertPath != "" {
		caCert, err := os.ReadFile(c.caCertPath)
		if err == nil {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(caCert)
			tlsConf.RootCAs = pool
		}
	}
	return tlsConf
}

// dial opens a raw connection to the daemon (Unix socket or TCP+TLS).
func (c *Client) dial() (net.Conn, error) {
	if c.remoteURL != "" {
		host := strings.TrimPrefix(strings.TrimPrefix(c.remoteURL, "https://"), "http://")
		if strings.HasPrefix(c.remoteURL, "https://") {
			return tls.DialWithDialer(
				&net.Dialer{Timeout: 5 * time.Second},
				"tcp", host,
				c.tlsConfig(),
			)
		}
		return net.DialTimeout("tcp", host, 5*time.Second)
	}
	return net.DialTimeout("unix", c.socketPath, 5*time.Second)
}

// DefaultClient returns a client for the default socket path,
// or a remote client if MVM_REMOTE is set.
func DefaultClient() *Client {
	if remote := os.Getenv("MVM_REMOTE"); remote != "" {
		return NewRemoteClient(
			remote,
			os.Getenv("MVM_API_KEY"),
			os.Getenv("MVM_CA_CERT"),
		)
	}
	return NewClient(DefaultSocketPath())
}

// IsAvailable checks if the daemon is running and responding.
func (c *Client) IsAvailable() bool {
	resp, err := c.httpClient.Get(c.url("/health"))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

// Exec sends an exec request and returns the result.
func (c *Client) Exec(ctx context.Context, vmName, command string) (string, int, error) {
	body, _ := json.Marshal(ExecRequest{Command: command})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		c.url(fmt.Sprintf("/vms/%s/exec", vmName)),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", -1, err
	}
	defer resp.Body.Close()

	var result ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", -1, err
	}
	if result.Error != "" {
		return "", -1, fmt.Errorf("%s", result.Error)
	}
	return result.Output, result.ExitCode, nil
}

// ExecStream sends a streaming exec request and writes output to stdout/stderr.
func (c *Client) ExecStream(ctx context.Context, vmName, command string, stdout, stderr io.Writer) (int, error) {
	body, _ := json.Marshal(ExecRequest{Command: command, Stream: true})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		c.url(fmt.Sprintf("/vms/%s/exec", vmName)),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Type") != "application/x-ndjson" {
		var result ExecResponse
		json.NewDecoder(resp.Body).Decode(&result)
		if result.Output != "" && stdout != nil {
			stdout.Write([]byte(result.Output))
		}
		return result.ExitCode, nil
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		var frame struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			ExitCode int    `json:"exit_code"`
			Error    string `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			continue
		}
		switch frame.Type {
		case "stdout":
			if stdout != nil {
				stdout.Write([]byte(frame.Data))
			}
		case "stderr":
			if stderr != nil {
				stderr.Write([]byte(frame.Data))
			}
		case "exit":
			if frame.Error != "" {
				return frame.ExitCode, fmt.Errorf("%s", frame.Error)
			}
			return frame.ExitCode, nil
		}
	}
	return -1, fmt.Errorf("stream ended without exit frame")
}

// CreateVM sends a create request.
func (c *Client) CreateVM(ctx context.Context, req CreateVMRequest) (*VMResponse, error) {
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", c.url("/vms"), bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result VMResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("create failed: %s", result.Error)
	}
	return &result, nil
}

// DeleteVM sends a delete request.
func (c *Client) DeleteVM(ctx context.Context, name string) error {
	req, _ := http.NewRequestWithContext(ctx, "DELETE", c.url(fmt.Sprintf("/vms/%s", name)), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("delete failed: status %d", resp.StatusCode)
	}
	return nil
}

// ListVMs lists all VMs.
func (c *Client) ListVMs(ctx context.Context) ([]VMResponse, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.url("/vms"), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result []VMResponse
	json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}

// PoolStatusResponse holds warm pool counts.
type PoolStatusResponse struct {
	Ready int `json:"ready"`
	Total int `json:"total"`
}

// PoolStatus returns the warm pool status.
func (c *Client) PoolStatus(ctx context.Context) (*PoolStatusResponse, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.url("/pool"), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result PoolStatusResponse
	json.NewDecoder(resp.Body).Decode(&result)
	return &result, nil
}

// StopVM stops a VM. If force is true, the VM is killed immediately.
func (c *Client) StopVM(ctx context.Context, name string, force bool) error {
	body, _ := json.Marshal(StopVMRequest{Force: force})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		c.url(fmt.Sprintf("/vms/%s/stop", name)), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var errResp struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&errResp)
		return fmt.Errorf("stop failed: %s", errResp.Error)
	}
	return nil
}

// PauseVM pauses a running VM.
func (c *Client) PauseVM(ctx context.Context, name string) error {
	req, _ := http.NewRequestWithContext(ctx, "POST",
		c.url(fmt.Sprintf("/vms/%s/pause", name)), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var errResp struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&errResp)
		return fmt.Errorf("pause failed: %s", errResp.Error)
	}
	return nil
}

// ResumeVM resumes a paused VM.
func (c *Client) ResumeVM(ctx context.Context, name string) error {
	req, _ := http.NewRequestWithContext(ctx, "POST",
		c.url(fmt.Sprintf("/vms/%s/resume", name)), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var errResp struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&errResp)
		return fmt.Errorf("resume failed: %s", errResp.Error)
	}
	return nil
}

// SnapshotCreate creates a snapshot of a running or paused VM.
func (c *Client) SnapshotCreate(ctx context.Context, vmName, snapName string) error {
	body, _ := json.Marshal(SnapshotCreateRequest{Name: snapName})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		c.url(fmt.Sprintf("/vms/%s/snapshot", vmName)), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var errResp struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&errResp)
		return fmt.Errorf("snapshot create failed: %s", errResp.Error)
	}
	return nil
}

// SnapshotRestore restores a VM from a named snapshot.
func (c *Client) SnapshotRestore(ctx context.Context, vmName, snapName string) error {
	body, _ := json.Marshal(SnapshotRestoreRequest{Name: snapName})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		c.url(fmt.Sprintf("/vms/%s/restore", vmName)), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var errResp struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&errResp)
		return fmt.Errorf("snapshot restore failed: %s", errResp.Error)
	}
	return nil
}

// SnapshotList returns all available snapshots.
func (c *Client) SnapshotList(ctx context.Context) ([]SnapshotInfo, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.url("/snapshots"), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result []SnapshotInfo
	json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}

// SnapshotDelete removes a named snapshot.
func (c *Client) SnapshotDelete(ctx context.Context, snapName string) error {
	req, _ := http.NewRequestWithContext(ctx, "DELETE",
		c.url(fmt.Sprintf("/snapshots/%s", snapName)), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		var errResp struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&errResp)
		return fmt.Errorf("snapshot delete failed: %s", errResp.Error)
	}
	return nil
}

// Build sends a build request to create a custom rootfs image.
func (c *Client) Build(ctx context.Context, imageName string, steps []firecracker.BuildStep, sizeMB int) error {
	body, _ := json.Marshal(BuildRequest{
		ImageName: imageName,
		Steps:     steps,
		SizeMB:    sizeMB,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", c.url("/build"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var errResp struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&errResp)
		return fmt.Errorf("build failed: %s", errResp.Error)
	}
	return nil
}

// ImageList returns all available custom rootfs images.
func (c *Client) ImageList(ctx context.Context) ([]ImageInfo, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.url("/images"), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result []ImageInfo
	json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}

// ImageDelete removes a custom rootfs image by name.
func (c *Client) ImageDelete(ctx context.Context, name string) error {
	req, _ := http.NewRequestWithContext(ctx, "DELETE",
		c.url(fmt.Sprintf("/images/%s", name)), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		var errResp struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&errResp)
		return fmt.Errorf("image delete failed: %s", errResp.Error)
	}
	return nil
}

// PoolWarm triggers pool warming. Returns immediately; warming happens async.
func (c *Client) PoolWarm(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, "POST", c.url("/pool/warm"), nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ExecInteractive opens a raw TCP-over-Unix connection to the daemon, triggers
// an HTTP 101 upgrade, and then relays length-prefixed JSON frames between the
// local terminal (stdin/stdout) and the guest agent's PTY.
//
// The caller is responsible for putting the terminal in raw mode before calling
// this method and restoring it afterward.
func (c *Client) ExecInteractive(ctx context.Context, vmName, command string, stdin io.Reader, stdout io.Writer) (int, error) {
	// 1. Dial the daemon directly — Go's http.Client cannot
	//    expose the underlying conn after an HTTP upgrade.
	conn, err := c.dial()
	if err != nil {
		return -1, fmt.Errorf("dial daemon: %w", err)
	}
	defer conn.Close()

	// 2. Write a raw HTTP POST with Connection: Upgrade.
	body, _ := json.Marshal(ExecRequest{Command: command, Interactive: true})

	host := "mvm"
	if c.remoteURL != "" {
		host = strings.TrimPrefix(strings.TrimPrefix(c.remoteURL, "https://"), "http://")
	}

	var httpReq strings.Builder
	fmt.Fprintf(&httpReq, "POST /vms/%s/exec HTTP/1.1\r\n", vmName)
	fmt.Fprintf(&httpReq, "Host: %s\r\n", host)
	httpReq.WriteString("Content-Type: application/json\r\n")
	httpReq.WriteString("Connection: Upgrade\r\n")
	httpReq.WriteString("Upgrade: tty\r\n")
	if c.apiKey != "" {
		fmt.Fprintf(&httpReq, "Authorization: Bearer %s\r\n", c.apiKey)
	}
	fmt.Fprintf(&httpReq, "Content-Length: %d\r\n", len(body))
	httpReq.WriteString("\r\n")
	httpReq.Write(body)

	if _, err := conn.Write([]byte(httpReq.String())); err != nil {
		return -1, fmt.Errorf("write HTTP request: %w", err)
	}

	// 3. Read HTTP response — look for "101" status.
	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		return -1, fmt.Errorf("read HTTP status: %w", err)
	}
	if !strings.Contains(statusLine, "101") {
		return -1, fmt.Errorf("upgrade failed: %s", strings.TrimSpace(statusLine))
	}
	// Skip remaining headers until empty line.
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return -1, fmt.Errorf("read HTTP headers: %w", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	// 4. The conn is now a raw bidirectional stream to the agent (via daemon relay).
	//    Any buffered data in the bufio.Reader needs to be combined with the conn.
	var stream io.ReadWriter
	if reader.Buffered() > 0 {
		// Wrap so we drain the bufio.Reader first, then read from conn directly.
		stream = &readWriter{
			Reader: io.MultiReader(reader, conn),
			Writer: conn,
		}
	} else {
		stream = conn
	}

	var (
		exitCode = -1
		mu       sync.Mutex
		wg       sync.WaitGroup
	)

	// 5a. Read frames from agent (via daemon relay) — write stdout data to stdout.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			var frame agentclient.PtyFrame
			if err := agentclient.ReadFrame(stream, &frame); err != nil {
				return
			}
			switch frame.Type {
			case "stdout":
				if len(frame.Data) > 0 {
					stdout.Write(frame.Data)
				}
			case "exit":
				mu.Lock()
				exitCode = frame.ExitCode
				mu.Unlock()
				return
			}
		}
	}()

	// 5b. Read from stdin, wrap in stdin frames, write to conn.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				frame := agentclient.PtyFrame{
					Type: "stdin",
					Data: buf[:n],
				}
				if wErr := agentclient.WriteFrame(stream, frame); wErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	return exitCode, nil
}

// SendResize sends a terminal resize frame over the interactive exec connection.
// This is exposed so the CLI can call it when SIGWINCH is received, but the
// initial implementation defers resize support to a follow-up.

// readWriter combines a separate Reader and Writer into an io.ReadWriter.
type readWriter struct {
	io.Reader
	io.Writer
}
