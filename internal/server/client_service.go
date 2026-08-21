package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/agentstep/mvm/internal/agentclient"
)

// Bounce restarts user code inside a VM without rebooting it.
func (c *Client) Bounce(ctx context.Context, name string) error {
	return c.simpleCall(ctx, "POST",
		fmt.Sprintf("/v1/vms/%s/bounce", url.PathEscape(name)), nil)
}

// ServiceList reports a VM's supervised services.
func (c *Client) ServiceList(ctx context.Context, name string) ([]agentclient.ServiceState, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		c.url(fmt.Sprintf("/v1/vms/%s/services", url.PathEscape(name))), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	var out []agentclient.ServiceState
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// ServiceAdd declares a service and starts supervising it.
func (c *Client) ServiceAdd(ctx context.Context, vmName string, svc ServiceRequest) error {
	body, err := json.Marshal(svc)
	if err != nil {
		return err
	}
	return c.simpleCall(ctx, "POST",
		fmt.Sprintf("/v1/vms/%s/services", url.PathEscape(vmName)), body)
}

// ServiceRemove stops a service and drops its declaration.
func (c *Client) ServiceRemove(ctx context.Context, vmName, svc string) error {
	return c.simpleCall(ctx, "DELETE",
		fmt.Sprintf("/v1/vms/%s/services/%s", url.PathEscape(vmName), url.PathEscape(svc)), nil)
}

// ServiceRestart restarts one service now.
func (c *Client) ServiceRestart(ctx context.Context, vmName, svc string) error {
	return c.simpleCall(ctx, "POST",
		fmt.Sprintf("/v1/vms/%s/services/%s/restart", url.PathEscape(vmName), url.PathEscape(svc)), nil)
}

// ServiceLogs returns a service's retained output.
func (c *Client) ServiceLogs(ctx context.Context, vmName, svc string, tail int) ([]agentclient.LogLine, error) {
	u := fmt.Sprintf("/v1/vms/%s/services/%s/logs?tail=%d",
		url.PathEscape(vmName), url.PathEscape(svc), tail)
	req, err := http.NewRequestWithContext(ctx, "GET", c.url(u), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	var out []agentclient.LogLine
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// simpleCall issues a request whose response carries no body worth decoding.
//
// Names are percent-escaped: unescaped, a name containing an invalid escape
// makes NewRequest return a nil request, and passing that to Do panics rather
// than surfacing the daemon's error.
func (c *Client) simpleCall(ctx context.Context, method, path string, body []byte) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkStatus(resp)
}
