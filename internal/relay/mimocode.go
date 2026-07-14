package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/looplj/axonhub/llm"
)

// MiMoCodeClient 通过 opencode 协议与 mimo serve 后端通信。
type MiMoCodeClient struct {
	BaseURL    string
	HTTPClient *http.Client
	AuthHeader string
}

func NewMiMoCodeClient(baseURL, password string) *MiMoCodeClient {
	authHeader := ""
	if password != "" {
		encoded := base64.StdEncoding.EncodeToString([]byte("mimocode:" + password))
		authHeader = "Basic " + encoded
	}
	return &MiMoCodeClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 5 * time.Minute},
		AuthHeader: authHeader,
	}
}

func (c *MiMoCodeClient) doRequest(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.AuthHeader != "" {
		req.Header.Set("Authorization", c.AuthHeader)
	}
	return c.HTTPClient.Do(req)
}

// --- Session ---

type miMoSession struct {
	ID string `json:"id"`
}

func (c *MiMoCodeClient) CreateSession(ctx context.Context) (string, error) {
	resp, err := c.doRequest(ctx, http.MethodPost, "/session", map[string]string{})
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create session %d: %s", resp.StatusCode, body)
	}
	var s miMoSession
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return "", fmt.Errorf("decode session: %w", err)
	}
	return s.ID, nil
}

func (c *MiMoCodeClient) DeleteSession(ctx context.Context, sessionID string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/session/"+sessionID, nil)
	if c.AuthHeader != "" {
		req.Header.Set("Authorization", c.AuthHeader)
	}
	resp, err := c.HTTPClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// --- Prompt ---

type MiMoCodePart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	MIME string `json:"mime,omitempty"`
	URL  string `json:"url,omitempty"`
}

type miMoPrompt struct {
	Model  miMoModel      `json:"model"`
	Parts  []MiMoCodePart `json:"parts"`
	System string         `json:"system,omitempty"`
}

type miMoModel struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

func (c *MiMoCodeClient) SendPrompt(ctx context.Context, sessionID string, prompt miMoPrompt) error {
	resp, err := c.doRequest(ctx, http.MethodPost, "/session/"+sessionID+"/prompt_async", prompt)
	if err != nil {
		return fmt.Errorf("send prompt: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send prompt %d: %s", resp.StatusCode, body)
	}
	return nil
}

// --- SSE Events ---

type miMoEvent struct {
	Type       string                 `json:"type"`
	Properties map[string]interface{} `json:"properties"`
}

type miMoPartUpdate struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Text string `json:"text"`
}

type miMoError struct {
	Name string `json:"name"`
	Data struct {
		Message string `json:"message"`
	} `json:"data"`
}

func (c *MiMoCodeClient) CollectResponse(ctx context.Context, sessionID string, timeout time.Duration) (content string, reasoning string, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/event", nil)
	if err != nil {
		return "", "", fmt.Errorf("create event request: %w", err)
	}
	if c.AuthHeader != "" {
		req.Header.Set("Authorization", c.AuthHeader)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("connect event stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("event stream %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var contentB, reasoningB strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var event miMoEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			continue
		}

		// 过滤其他 session 的事件
		if eventSessionID, _ := event.Properties["sessionID"].(string); eventSessionID != "" && eventSessionID != sessionID {
			continue
		}

		switch event.Type {
		case "session.idle":
			return contentB.String(), reasoningB.String(), nil

		case "session.error":
			var miMoErr miMoError
			if errData, ok := event.Properties["error"].(map[string]interface{}); ok {
				errJSON, _ := json.Marshal(errData)
				json.Unmarshal(errJSON, &miMoErr)
			}
			return contentB.String(), reasoningB.String(),
				fmt.Errorf("mimocode: %s: %s", miMoErr.Name, miMoErr.Data.Message)

		case "message.part.delta":
			field, _ := event.Properties["field"].(string)
			delta, _ := event.Properties["delta"].(string)
			if delta == "" {
				continue
			}
			if field == "reasoning" {
				reasoningB.WriteString(delta)
			} else {
				contentB.WriteString(delta)
			}

		case "message.part.updated":
			partJSON, ok := event.Properties["part"].(map[string]interface{})
			if !ok {
				continue
			}
			partType, _ := partJSON["type"].(string)
			partText, _ := partJSON["text"].(string)
			if partText == "" {
				continue
			}
			// 用 finalized text 覆盖 delta 累积（更完整）
			switch partType {
			case "reasoning":
				reasoningB.Reset()
				reasoningB.WriteString(partText)
			case "text":
				contentB.Reset()
				contentB.WriteString(partText)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return contentB.String(), reasoningB.String(), fmt.Errorf("event stream read: %w", err)
	}
	return contentB.String(), reasoningB.String(), fmt.Errorf("event stream ended without session.idle")
}

// --- Model Fetch ---

type miMoProviders struct {
	Providers []struct {
		ID     string   `json:"id"`
		Models []string `json:"models"`
	} `json:"providers"`
	Default map[string]string `json:"default"`
}

// ResolveModel 解析模型名，若 backend 返回了实际使用的模型则以响应为准。
func (c *MiMoCodeClient) ResolveModel(ctx context.Context, requestedModel string) (providerID, modelID string) {
	providerID = "mimo"
	modelID = requestedModel
	if idx := strings.Index(modelID, "/"); idx > 0 {
		providerID = modelID[:idx]
		modelID = modelID[idx+1:]
	}

	// 若是 auto 或无法识别的模型，尝试用 backend 默认模型
	resp, err := c.doRequest(ctx, http.MethodGet, "/config/providers", nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var providers miMoProviders
	if err := json.NewDecoder(resp.Body).Decode(&providers); err != nil {
		return
	}
	if defaultModel, ok := providers.Default[providerID]; ok && (modelID == "auto" || modelID == "") {
		modelID = defaultModel
	}
	return
}

func (c *MiMoCodeClient) FetchModels(ctx context.Context) ([]string, error) {
	resp, err := c.doRequest(ctx, http.MethodGet, "/config/providers", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch providers %d", resp.StatusCode)
	}
	var providers miMoProviders
	if err := json.NewDecoder(resp.Body).Decode(&providers); err != nil {
		return nil, err
	}
	var models []string
	for _, p := range providers.Providers {
		for _, m := range p.Models {
			models = append(models, p.ID+"/"+m)
		}
	}
	return models, nil
}

// --- Message Conversion ---

// ConvertMessages 把 OpenAI 格式的 messages 转成 opencode 的 system + parts。
func ConvertMessages(messages []llm.Message) (string, []MiMoCodePart) {
	var systemChunks []string
	var parts []MiMoCodePart

	for _, m := range messages {
		role := strings.ToLower(m.Role)

		switch role {
		case "system":
			if text := extractText(m.Content); text != "" {
				systemChunks = append(systemChunks, text)
			}

		case "user":
			text := extractText(m.Content)
			if text != "" {
				parts = append(parts, MiMoCodePart{Type: "text", Text: "USER: " + text})
			}
			// 图片
			for _, imgURL := range extractImageURLs(m.Content) {
				mime := "image/jpeg"
				if strings.HasPrefix(imgURL, "data:") {
					if idx := strings.Index(imgURL, ";"); idx > 0 {
						mime = imgURL[len("data:"):idx]
					}
				}
				parts = append(parts, MiMoCodePart{Type: "file", MIME: mime, URL: imgURL})
			}

		case "assistant":
			text := extractText(m.Content)
			if text != "" {
				parts = append(parts, MiMoCodePart{Type: "text", Text: "ASSISTANT: " + text})
			}
			if len(m.ToolCalls) > 0 {
				tcJSON, _ := json.Marshal(m.ToolCalls)
				parts = append(parts, MiMoCodePart{
					Type: "text",
					Text: fmt.Sprintf("ASSISTANT: <function_calls>%s</function_calls>", tcJSON),
				})
			}

		case "tool":
			text := extractText(m.Content)
			if text != "" {
				id := ""
				if m.ToolCallID != nil {
					id = *m.ToolCallID
				}
				name := ""
				if m.ToolCallName != nil {
					name = *m.ToolCallName
				}
				result, _ := json.Marshal(map[string]string{
					"tool_call_id": id,
					"name":         name,
					"content":      text,
				})
				parts = append(parts, MiMoCodePart{Type: "text", Text: "TOOL_RESULT: " + string(result)})
			}
		}
	}

	return strings.Join(systemChunks, "\n\n"), parts
}

func extractText(content llm.MessageContent) string {
	if content.Content != nil {
		return *content.Content
	}
	var texts []string
	for _, p := range content.MultipleContent {
		if p.Type == "text" && p.Text != nil {
			texts = append(texts, *p.Text)
		}
	}
	return strings.Join(texts, "")
}

func extractImageURLs(content llm.MessageContent) []string {
	if len(content.MultipleContent) == 0 {
		return nil
	}
	var urls []string
	for _, p := range content.MultipleContent {
		if p.Type == "image_url" && p.ImageURL != nil {
			urls = append(urls, p.ImageURL.URL)
		}
	}
	return urls
}

// BuildOpenAIResponse 把 opencode 返回的 content/reathing 组装成 OpenAI 格式响应。
func BuildOpenAIResponse(model, content, reasoning string, usage *miMoUsage) map[string]interface{} {
	msg := map[string]interface{}{
		"role":    "assistant",
		"content": content,
	}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}

	resp := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": "stop",
			},
		},
	}

	if usage != nil {
		resp["usage"] = map[string]interface{}{
			"prompt_tokens":     usage.Input,
			"completion_tokens": usage.Output + usage.Reasoning,
			"total_tokens":      usage.Input + usage.Output + usage.Reasoning,
			"completion_tokens_details": map[string]interface{}{
				"reasoning_tokens": usage.Reasoning,
			},
		}
	}

	return resp
}

type miMoUsage struct {
	Input     int
	Output    int
	Reasoning int
}
