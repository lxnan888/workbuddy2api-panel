package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const webFetchPath = "/agenttool/v1/webfetch"

// WebFetch 抓取网页正文，返回上游原文 JSON（{url,title,content}）。
func (c *Client) WebFetch(ctx context.Context, a *auth.Auth, url string) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"url": url})
	if err != nil {
		return nil, err
	}
	return c.agentTool(ctx, a, webFetchPath, body)
}

// agentTool POST 到 agenttool 族端点（与 chat 同域同头）。
func (c *Client) agentTool(ctx context.Context, a *auth.Auth, path string, body []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.ChatHeaders(req, a, "", ChatMeta{})
	resp, err := c.do(req, a, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode,
			Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return json.RawMessage(raw), nil
}
