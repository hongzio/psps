package notionproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var errUnauthorized = errors.New("unauthorized")

// mcpClient is just enough MCP client for the check: one session, JSON or
// SSE answers.
type mcpClient struct {
	o       oauth
	token   string
	session string
	nextID  int
}

func (c *mcpClient) post(ctx context.Context, msg map[string]any) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", c.o.issuer+MCPPath, bytes.NewReader(encode(msg)))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", ProtocolVersion)
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := c.o.client().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRewrite))
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	return resp, raw, nil
}

func (c *mcpClient) notify(ctx context.Context, method string) {
	c.post(ctx, map[string]any{"jsonrpc": "2.0", "method": method})
}

func (c *mcpClient) call(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	c.nextID++
	msg := map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method}
	if params != nil {
		msg["params"] = params
	}
	resp, raw, err := c.post(ctx, msg)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 401 {
		return nil, errUnauthorized
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("notion: %s answered HTTP %d", method, resp.StatusCode)
	}
	var candidates []map[string]any
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		for _, ev := range SSEMessages(raw) {
			candidates = append(candidates, ev.Message)
		}
	} else if m, err := decode(raw); err == nil {
		candidates = append(candidates, m)
	}
	for _, m := range candidates {
		if m != nil && sameID(m["id"], c.nextID) {
			if e, ok := m["error"].(map[string]any); ok {
				return nil, fmt.Errorf("notion: %s failed: %v", method, e["message"])
			}
			r, _ := m["result"].(map[string]any)
			if r == nil {
				r = map[string]any{}
			}
			return r, nil
		}
	}
	return nil, fmt.Errorf("notion: %s came back without an answer", method)
}

func (c *mcpClient) close(ctx context.Context) {
	if c.session == "" {
		return
	}
	req, _ := http.NewRequestWithContext(ctx, "DELETE", c.o.issuer+MCPPath, nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Mcp-Session-Id", c.session)
	if resp, err := c.o.client().Do(req); err == nil {
		resp.Body.Close()
	}
}

func probe(ctx context.Context, o oauth, token string) (map[string]any, []string, error) {
	c := &mcpClient{o: o, token: token}
	defer c.close(ctx)
	if _, err := c.call(ctx, "initialize", map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "gero-check", "version": "1"}}); err != nil {
		return nil, nil, err
	}
	c.notify(ctx, "notifications/initialized")
	me, err := c.call(ctx, "tools/call", map[string]any{"name": "notion-get-self", "arguments": map[string]any{}})
	if err != nil {
		return nil, nil, err
	}
	var tools []string
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		page, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, nil, err
		}
		list, _ := page["tools"].([]any)
		for _, t := range list {
			if m, ok := t.(map[string]any); ok {
				if n := str(m["name"]); n != "" {
					tools = append(tools, n)
				}
			}
		}
		if cursor = str(page["nextCursor"]); cursor == "" {
			return me, tools, nil
		}
	}
}
