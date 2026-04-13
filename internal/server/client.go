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
	"time"
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
