package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	miMoBootstrapPath = "/api/free-ai/bootstrap"
	miMoChatPath      = "/api/free-ai/openai/chat"
	miMoUserAgent     = "mimocode/0.1.0 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
	miMoXSource       = "mimocode-cli-free"

	// 上游网关要求请求携带此前缀，否则返回 403。
	miMoMagicPrefix = "# Memory system\n\nYou have a persistent file-based memory system. Four file types"
)

// --- JWT 认证 ---

type miMoJWTManager struct {
	baseURL    string
	httpClient *http.Client
	clientID   string
	mu         sync.Mutex
	jwt        string
	exp        time.Time
}

func newMiMoJWTManager(baseURL string, httpClient *http.Client) *miMoJWTManager {
	return &miMoJWTManager{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
		clientID:   computeMiMoFingerprint(),
	}
}

func (m *miMoJWTManager) getJWT(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jwt != "" && time.Now().Before(m.exp.Add(-60*time.Second)) {
		return m.jwt, nil
	}
	return m.refresh(ctx)
}

func (m *miMoJWTManager) refresh(ctx context.Context) (string, error) {
	payload, _ := json.Marshal(map[string]string{"client": m.clientID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+miMoBootstrapPath, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", miMoUserAgent)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("bootstrap: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("bootstrap %d: %s", resp.StatusCode, body)
	}
	var data struct {
		JWT string `json:"jwt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", fmt.Errorf("decode bootstrap: %w", err)
	}
	if data.JWT == "" {
		return "", fmt.Errorf("bootstrap: empty jwt")
	}
	m.jwt = data.JWT
	m.exp = jwtDecodeExp(data.JWT)
	return m.jwt, nil
}

func jwtDecodeExp(jwt string) time.Time {
	parts := strings.SplitN(jwt, ".", 3)
	if len(parts) < 2 {
		return time.Time{}
	}
	b := parts[1]
	if mod := len(b) % 4; mod != 0 {
		b += strings.Repeat("=", 4-mod)
	}
	raw, err := base64.URLEncoding.DecodeString(b)
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// --- 硬件指纹 ---

func computeMiMoFingerprint() string {
	hostname, _ := os.Hostname()
	cpu := runtime.GOARCH // ponytail: 近似即可，精确 cpu model 需要 platform-specific 代码
	username := ""
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	payload := strings.Join([]string{hostname, runtime.GOOS, runtime.GOARCH, cpu, username}, "|")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
}

// --- 请求头 ---

func miMoChatHeaders(jwt string, sessionAffinity string) http.Header {
	return http.Header{
		"Authorization":      {"Bearer " + jwt},
		"Content-Type":       {"application/json"},
		"User-Agent":         {miMoUserAgent},
		"X-Mimo-Source":      {miMoXSource},
		"x-session-affinity": {sessionAffinity},
		"Accept":             {"*/*"},
	}
}

// --- Magic prefix 注入（操作原始 JSON）---

func injectMiMoMagicPrefixRaw(messages []any) []any {
	if len(messages) == 0 {
		return []any{map[string]any{"role": "system", "content": miMoMagicPrefix}}
	}
	first, ok := messages[0].(map[string]any)
	if !ok {
		return append([]any{map[string]any{"role": "system", "content": miMoMagicPrefix}}, messages...)
	}
	if strings.ToLower(first["role"].(string)) != "system" {
		return append([]any{map[string]any{"role": "system", "content": miMoMagicPrefix}}, messages...)
	}
	text, _ := first["content"].(string)
	if !strings.HasPrefix(text, miMoMagicPrefix) {
		first["content"] = miMoMagicPrefix + "\n\n" + text
	}
	return messages
}

// --- 上游请求 ---

func miMoChatRaw(ctx context.Context, jwtMgr *miMoJWTManager, baseURL string, body map[string]any, sessionAffinity string) (*http.Response, error) {
	jwt, err := jwtMgr.getJWT(ctx)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(body)
	url := strings.TrimRight(baseURL, "/") + miMoChatPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header = miMoChatHeaders(jwt, sessionAffinity)
	resp, err := jwtMgr.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 401 {
		resp.Body.Close()
		jwt, err = jwtMgr.refresh(ctx)
		if err != nil {
			return nil, err
		}
		req.Header = miMoChatHeaders(jwt, sessionAffinity)
		req.Body = io.NopCloser(bytes.NewReader(payload))
		resp, err = jwtMgr.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// --- SSE 聚合 ---

func miMoAggregateSSE(body io.Reader) []map[string]any {
	var chunks []map[string]any
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) == nil {
			chunks = append(chunks, chunk)
		}
	}
	return chunks
}

func miMoAggregateChunks(chunks []map[string]any, model string) map[string]any {
	var content, reasoning strings.Builder
	var finishReason string
	var usage map[string]any
	var toolCalls []map[string]any

	for _, chunk := range chunks {
		if m, ok := chunk["model"].(string); ok && m != "" {
			model = m
		}
		for _, choice := range extractChoices(chunk) {
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if s, ok := delta["content"].(string); ok {
				content.WriteString(s)
			}
			if s, ok := delta["reasoning_content"].(string); ok {
				reasoning.WriteString(s)
			}
			if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			// 聚合 tool_calls delta
			if rawTC, ok := delta["tool_calls"].([]any); ok {
				for _, tcRaw := range rawTC {
					tc, ok := tcRaw.(map[string]any)
					if !ok {
						continue
					}
					idx := 0
					if i, ok := tc["index"].(float64); ok {
						idx = int(i)
					}
					// 扩展 tool_calls 切片以容纳此 index
					for len(toolCalls) <= idx {
						toolCalls = append(toolCalls, map[string]any{
							"id":   "",
							"type": "function",
							"function": map[string]any{
								"name":      "",
								"arguments": "",
							},
						})
					}
					existing := toolCalls[idx]
					if id, ok := tc["id"].(string); ok && id != "" {
						existing["id"] = id
					}
					if tp, ok := tc["type"].(string); ok && tp != "" {
						existing["type"] = tp
					}
					if funcObj, ok := tc["function"].(map[string]any); ok {
						fn, _ := existing["function"].(map[string]any)
						if name, ok := funcObj["name"].(string); ok && name != "" {
							fn["name"] = name
						}
						if args, ok := funcObj["arguments"].(string); ok {
							fn["arguments"] = fn["arguments"].(string) + args
						}
					}
				}
			}
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
	}

	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		// 有 tool_calls 时，content 可能为 null
		if content.Len() == 0 {
			msg["content"] = nil
		}
	}
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finishReason}},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp
}

func extractChoices(chunk map[string]any) []map[string]any {
	raw, ok := chunk["choices"]
	if !ok || raw == nil {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	var result []map[string]any
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			result = append(result, m)
		}
	}
	return result
}
