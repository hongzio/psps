// Package client talks to the daemon's API over its unix socket.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/proxy"
)

type Client struct {
	hc *http.Client
}

func New(socket string) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	return &Client{hc: &http.Client{Transport: tr}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://gero"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e api.Error
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Health(ctx context.Context) (api.Health, error) {
	var h api.Health
	return h, c.do(ctx, "GET", "/v1/health", nil, &h)
}

func (c *Client) Isolations(ctx context.Context) ([]api.Isolation, error) {
	var v []api.Isolation
	return v, c.do(ctx, "GET", "/v1/isolations", nil, &v)
}

func (c *Client) Isolation(ctx context.Context, name string) (api.Isolation, error) {
	var v api.Isolation
	return v, c.do(ctx, "GET", "/v1/isolations/"+url.PathEscape(name), nil, &v)
}

func (c *Client) CreateIsolation(ctx context.Context, req api.CreateIsolation) (api.Isolation, error) {
	var v api.Isolation
	return v, c.do(ctx, "POST", "/v1/isolations", req, &v)
}

// UpdateIsolation renames an isolation or resizes its VM.
func (c *Client) UpdateIsolation(ctx context.Context, name string, req api.UpdateIsolation) (api.Isolation, error) {
	var v api.Isolation
	return v, c.do(ctx, "PATCH", "/v1/isolations/"+url.PathEscape(name), req, &v)
}

func (c *Client) UpgradeIsolation(ctx context.Context, name string) (api.Isolation, error) {
	var v api.Isolation
	return v, c.do(ctx, "POST", "/v1/isolations/"+url.PathEscape(name)+"/upgrade", nil, &v)
}

// Verify measures an isolation's guarantees now; it takes seconds.
func (c *Client) Verify(ctx context.Context, name string) (api.Verify, error) {
	var v api.Verify
	return v, c.do(ctx, "POST", "/v1/isolations/"+url.PathEscape(name)+"/verify", nil, &v)
}

func (c *Client) Accounts(ctx context.Context) ([]api.Account, error) {
	var v []api.Account
	return v, c.do(ctx, "GET", "/v1/accounts", nil, &v)
}

func (c *Client) AddAccount(ctx context.Context, name, kind, secret string) (api.Account, error) {
	var v api.Account
	return v, c.do(ctx, "POST", "/v1/accounts/"+url.PathEscape(name), api.PutAccount{Kind: kind, Secret: secret}, &v)
}

func (c *Client) SetAccountSecret(ctx context.Context, name, secret string) (api.Account, error) {
	var v api.Account
	return v, c.do(ctx, "PATCH", "/v1/accounts/"+url.PathEscape(name), api.PutAccount{Secret: secret}, &v)
}

func (c *Client) DeleteAccount(ctx context.Context, name string, force bool) error {
	q := ""
	if force {
		q = "?force=1"
	}
	return c.do(ctx, "DELETE", "/v1/accounts/"+url.PathEscape(name)+q, nil, nil)
}

func (c *Client) UseAccount(ctx context.Context, iso, tool, account string) error {
	return c.do(ctx, "PUT", cliPath(iso, tool)+"/account", api.UseAccount{Account: account}, nil)
}

func cliPath(iso, tool string) string {
	return "/v1/isolations/" + url.PathEscape(iso) + "/clis/" + url.PathEscape(tool)
}

func (c *Client) CLIs(ctx context.Context, iso string) ([]api.CLI, error) {
	var v []api.CLI
	return v, c.do(ctx, "GET", "/v1/isolations/"+url.PathEscape(iso)+"/clis", nil, &v)
}

func (c *Client) StartCLILogin(ctx context.Context, iso, tool string) (api.CLILogin, error) {
	var v api.CLILogin
	return v, c.do(ctx, "POST", cliPath(iso, tool)+"/login", nil, &v)
}

func (c *Client) CLILogin(ctx context.Context, iso, tool string) (api.CLILogin, error) {
	var v api.CLILogin
	return v, c.do(ctx, "GET", cliPath(iso, tool)+"/login", nil, &v)
}

func (c *Client) SendCLICode(ctx context.Context, iso, tool, code string) (api.CLILogin, error) {
	var v api.CLILogin
	return v, c.do(ctx, "POST", cliPath(iso, tool)+"/login/code", api.CLICode{Code: code}, &v)
}

func (c *Client) CancelCLILogin(ctx context.Context, iso, tool string) error {
	return c.do(ctx, "DELETE", cliPath(iso, tool)+"/login", nil, nil)
}

func (c *Client) CLILogout(ctx context.Context, iso, tool string) error {
	return c.do(ctx, "POST", cliPath(iso, tool)+"/logout", nil, nil)
}

func (c *Client) DeleteIsolation(ctx context.Context, name string, purge bool) error {
	q := ""
	if purge {
		q = "?purge=1"
	}
	return c.do(ctx, "DELETE", "/v1/isolations/"+url.PathEscape(name)+q, nil, nil)
}

func (c *Client) SetEnv(ctx context.Context, name string, set api.SetEnv) error {
	return c.do(ctx, "PATCH", "/v1/isolations/"+url.PathEscape(name)+"/env", set, nil)
}

func (c *Client) Agents(ctx context.Context) ([]api.Agent, error) {
	var v []api.Agent
	return v, c.do(ctx, "GET", "/v1/agents", nil, &v)
}

func (c *Client) Agent(ctx context.Context, id string) (api.Agent, error) {
	var v api.Agent
	return v, c.do(ctx, "GET", "/v1/agents/"+url.PathEscape(id), nil, &v)
}

func (c *Client) CreateAgent(ctx context.Context, req api.CreateAgent) (api.Agent, error) {
	var v api.Agent
	return v, c.do(ctx, "POST", "/v1/agents", req, &v)
}

func (c *Client) Send(ctx context.Context, id, text string) error {
	return c.do(ctx, "POST", "/v1/agents/"+url.PathEscape(id)+"/messages", api.Message{Text: text}, nil)
}

func (c *Client) Approve(ctx context.Context, id, approval string, d driver.Decision) error {
	return c.do(ctx, "POST", "/v1/agents/"+url.PathEscape(id)+"/approvals/"+url.PathEscape(approval), api.Decision{Decision: d}, nil)
}

func (c *Client) Interrupt(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/v1/agents/"+url.PathEscape(id)+"/interrupt", nil, nil)
}

func (c *Client) SetOptions(ctx context.Context, id string, o driver.Options) error {
	return c.do(ctx, "PUT", "/v1/agents/"+url.PathEscape(id)+"/options", o, nil)
}

func (c *Client) Archive(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/v1/agents/"+url.PathEscape(id)+"/archive", nil, nil)
}

// DeleteAgent forgets an archived agent; purge also removes its workspace
// and its CLI's session transcript from the isolation.
func (c *Client) DeleteAgent(ctx context.Context, id string, purge bool) error {
	q := ""
	if purge {
		q = "?purge=1"
	}
	return c.do(ctx, "DELETE", "/v1/agents/"+url.PathEscape(id)+q, nil, nil)
}

func (c *Client) Events(ctx context.Context, id string, after int64) ([]event.Event, error) {
	var v []event.Event
	return v, c.do(ctx, "GET", fmt.Sprintf("/v1/agents/%s/events?after=%d", url.PathEscape(id), after), nil, &v)
}

// Follow streams an agent's events after `after` (history first, then live)
// until ctx ends or fn returns an error. An empty id follows every agent,
// live only.
func (c *Client) Follow(ctx context.Context, id string, after int64, fn func(event.Event) error) error {
	path := "/v1/events?follow=1"
	if id != "" {
		path = fmt.Sprintf("/v1/agents/%s/events?follow=1&after=%d", url.PathEscape(id), after)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://gero"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e api.Error
		json.NewDecoder(resp.Body).Decode(&e)
		return errors.New(e.Error)
	}
	r := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		data, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "data: ")
		if !ok {
			continue
		}
		var e event.Event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
}

func (c *Client) Services(ctx context.Context) ([]api.Service, error) {
	var v []api.Service
	return v, c.do(ctx, "GET", "/v1/services", nil, &v)
}

func (c *Client) Envs(ctx context.Context, service string) ([]api.Env, error) {
	var v []api.Env
	return v, c.do(ctx, "GET", "/v1/envs?service="+url.QueryEscape(service), nil, &v)
}

func envPath(service, name string) string {
	return "/v1/envs/" + url.PathEscape(service) + "/" + url.PathEscape(name)
}

func (c *Client) AddEnv(ctx context.Context, service, name string, p proxy.Patch) (api.Env, error) {
	var v api.Env
	return v, c.do(ctx, "POST", envPath(service, name), p, &v)
}

func (c *Client) EditEnv(ctx context.Context, service, name string, p proxy.Patch) (api.Env, error) {
	var v api.Env
	return v, c.do(ctx, "PATCH", envPath(service, name), p, &v)
}

func (c *Client) DeleteEnv(ctx context.Context, service, name string, force bool) error {
	q := ""
	if force {
		q = "?force=1"
	}
	return c.do(ctx, "DELETE", envPath(service, name)+q, nil, nil)
}

// SetSecret stores an env's credential; nil removes it.
func (c *Client) SetSecret(ctx context.Context, service, name string, values map[string]string) error {
	if values == nil {
		return c.do(ctx, "DELETE", envPath(service, name)+"/secret", nil, nil)
	}
	return c.do(ctx, "PUT", envPath(service, name)+"/secret", values, nil)
}

func (c *Client) StartLogin(ctx context.Context, service, name, redirect string) (api.Login, error) {
	var v api.Login
	return v, c.do(ctx, "POST", envPath(service, name)+"/login", api.LoginStart{Redirect: redirect}, &v)
}

func (c *Client) FinishLogin(ctx context.Context, service, name, id, callback string) error {
	return c.do(ctx, "POST", envPath(service, name)+"/login/"+url.PathEscape(id), api.LoginFinish{Callback: callback}, nil)
}

func (c *Client) Choices(ctx context.Context, service, field string, req api.ChoicesRequest) ([]proxy.Choice, error) {
	var v []proxy.Choice
	return v, c.do(ctx, "POST", "/v1/services/"+url.PathEscape(service)+"/choices/"+url.PathEscape(field), req, &v)
}

func (c *Client) CheckEnv(ctx context.Context, service, name string) (api.Check, error) {
	var v api.Check
	return v, c.do(ctx, "POST", envPath(service, name)+"/check", nil, &v)
}

func (c *Client) Exposures(ctx context.Context, iso string) ([]api.Exposure, error) {
	var v []api.Exposure
	return v, c.do(ctx, "GET", "/v1/isolations/"+url.PathEscape(iso)+"/services", nil, &v)
}

func (c *Client) Expose(ctx context.Context, iso, service string, req api.Expose) (api.Exposure, error) {
	var v api.Exposure
	return v, c.do(ctx, "PUT", "/v1/isolations/"+url.PathEscape(iso)+"/services/"+url.PathEscape(service), req, &v)
}

func (c *Client) Unexpose(ctx context.Context, iso, service string) error {
	return c.do(ctx, "DELETE", "/v1/isolations/"+url.PathEscape(iso)+"/services/"+url.PathEscape(service), nil, nil)
}
