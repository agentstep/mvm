package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agentstep/mvm/internal/agentclient"
)

// Client communicates with the mvm daemon via Unix socket.
type Client struct {
	httpClient *http.Client
	socketPath string
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

// DefaultClient returns a client for the default socket path.
func DefaultClient() *Client {
	return NewClient(DefaultSocketPath())
}

// IsAvailable checks if the daemon is running and responding.
func (c *Client) IsAvailable() bool {
	resp, err := c.httpClient.Get("http://mvm/health")
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
		fmt.Sprintf("http://mvm/vms/%s/exec", vmName),
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
		fmt.Sprintf("http://mvm/vms/%s/exec", vmName),
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
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", "http://mvm/vms", bytes.NewReader(body))
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
	req, _ := http.NewRequestWithContext(ctx, "DELETE", fmt.Sprintf("http://mvm/vms/%s", name), nil)
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
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://mvm/vms", nil)
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
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://mvm/pool", nil)
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
		fmt.Sprintf("http://mvm/vms/%s/stop", name), bytes.NewReader(body))
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
		fmt.Sprintf("http://mvm/vms/%s/pause", name), nil)
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
		fmt.Sprintf("http://mvm/vms/%s/resume", name), nil)
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

// PoolWarm triggers pool warming. Returns immediately; warming happens async.
func (c *Client) PoolWarm(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://mvm/pool/warm", nil)
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
	// 1. Dial the daemon's Unix socket directly — Go's http.Client cannot
	//    expose the underlying conn after an HTTP upgrade.
	conn, err := net.DialTimeout("unix", c.socketPath, 5*time.Second)
	if err != nil {
		return -1, fmt.Errorf("dial daemon: %w", err)
	}
	defer conn.Close()

	// 2. Write a raw HTTP POST with Connection: Upgrade.
	body, _ := json.Marshal(ExecRequest{Command: command, Interactive: true})
	var httpReq strings.Builder
	fmt.Fprintf(&httpReq, "POST /vms/%s/exec HTTP/1.1\r\n", vmName)
	httpReq.WriteString("Host: mvm\r\n")
	httpReq.WriteString("Content-Type: application/json\r\n")
	httpReq.WriteString("Connection: Upgrade\r\n")
	httpReq.WriteString("Upgrade: tty\r\n")
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
