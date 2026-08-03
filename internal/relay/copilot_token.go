package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/looplj/axonhub/llm/httpclient"
)

const (
	copilotTokenEndpoint = "https://api.github.com/copilot_internal/v2/token"
	copilotRefreshMargin = 5 * time.Minute
)

// copilotTokenResponse 是 api.github.com/copilot_internal/v2/token 的响应体。
// refresh_in 字段略（当前用 expires_at 精确控制刷新时机）。
type copilotTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// copilotTokenProvider 实现 axonhub copilot.TokenProvider 接口。
// channel key 里填的是 GitHub PAT（含 Copilot scope），这里用它换成短期 Copilot JWT。
// server 端不走 OAuth Device Flow——PAT 直接可用。
type copilotTokenProvider struct {
	httpClient  *httpclient.HttpClient
	githubToken string

	mu           sync.RWMutex
	copilotToken string
	expiresAt    time.Time
}

// refreshSingleflight 防止并发 relay 同时触发多个 token 交换请求。
var refreshSingleflight singleflight.Group

func newCopilotTokenProvider(githubToken string, httpClient *http.Client) *copilotTokenProvider {
	return &copilotTokenProvider{
		httpClient:  httpclient.NewHttpClientWithClient(httpClient),
		githubToken: githubToken,
	}
}

// GetToken 返回可用的 Copilot JWT；过期则用 GitHub token 交换并缓存。
func (p *copilotTokenProvider) GetToken(ctx context.Context) (string, error) {
	p.mu.RLock()
	if p.copilotToken != "" && time.Until(p.expiresAt) > copilotRefreshMargin {
		token := p.copilotToken
		p.mu.RUnlock()
		return token, nil
	}
	p.mu.RUnlock()

	sfKey := sha256Hex(p.githubToken)
	token, err, _ := refreshSingleflight.Do(sfKey, func() (any, error) {
		// 等待期间可能已被另一个 goroutine 换好了，二次检查避免雪崩式重复调用
		p.mu.RLock()
		if p.copilotToken != "" && time.Until(p.expiresAt) > copilotRefreshMargin {
			token := p.copilotToken
			p.mu.RUnlock()
			return token, nil
		}
		p.mu.RUnlock()

		// WithoutCancel：避免第一个调用者的 ctx 取消把其他等待者也拖下水。
		return p.exchange(context.WithoutCancel(ctx))
	})
	if err != nil {
		return "", err
	}
	return token.(string), nil
}

// exchange 用 GitHub PAT 向上游交换 Copilot JWT。
// 响应示例：{"token":"gho_...","expires_at":1753948800,"refresh_in":1000}
func (p *copilotTokenProvider) exchange(ctx context.Context) (string, error) {
	req := httpclient.NewRequestBuilder().
		WithMethod(http.MethodGet).
		WithURL(copilotTokenEndpoint).
		WithHeader("Authorization", "token "+p.githubToken).
		WithHeader("Accept", "application/json").
		Build()

	resp, err := p.httpClient.Do(ctx, req)
	if err != nil {
		return "", fmt.Errorf("copilot token exchange failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body := string(resp.Body)
		if len(body) > 200 {
			body = body[:200] + "..."
		}
		return "", fmt.Errorf("copilot token exchange returned %d: %s", resp.StatusCode, body)
	}

	var r copilotTokenResponse
	if err := json.Unmarshal(resp.Body, &r); err != nil {
		return "", fmt.Errorf("failed to parse copilot token response: %w", err)
	}
	if r.Token == "" {
		return "", fmt.Errorf("copilot token is empty in response")
	}
	if r.ExpiresAt == 0 {
		return "", fmt.Errorf("copilot token expires_at is missing")
	}

	p.mu.Lock()
	p.copilotToken = r.Token
	p.expiresAt = time.Unix(r.ExpiresAt, 0)
	p.mu.Unlock()
	return r.Token, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
