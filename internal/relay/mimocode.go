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

	"github.com/gin-gonic/gin"
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

// StreamToClient 从 MiMoCode SSE 事件流中边读边转发给客户端，返回聚合内容用于日志。
func (c *MiMoCodeClient) StreamToClient(ctx context.Context, sessionID string, timeout time.Duration, model string, w gin.ResponseWriter) (content string, reasoning string, err error) {
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
	partTypeMap := map[string]string{} // partID → type (reasoning/text)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())

	writeChunk := func(delta map[string]interface{}) {
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]interface{}{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
			"choices": []map[string]interface{}{{"index": 0, "delta": delta, "finish_reason": nil}},
		}))
		w.Flush()
	}

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var event miMoEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			continue
		}

		if sid, _ := event.Properties["sessionID"].(string); sid != "" && sid != sessionID {
			continue
		}

		switch event.Type {
		case "session.idle":
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]interface{}{
				"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
				"choices": []map[string]interface{}{{"index": 0, "delta": map[string]interface{}{}, "finish_reason": "stop"}},
			}))
			fmt.Fprintf(w, "data: [DONE]\n\n")
			w.Flush()
			return contentB.String(), reasoningB.String(), nil

		case "session.error":
			var miMoErr miMoError
			if d, ok := event.Properties["error"].(map[string]interface{}); ok {
				b, _ := json.Marshal(d)
				json.Unmarshal(b, &miMoErr)
			}
			return contentB.String(), reasoningB.String(), fmt.Errorf("mimocode: %s: %s", miMoErr.Name, miMoErr.Data.Message)

		case "message.part.updated":
			// 记录 partID → type 映射，delta 事件不带 type 信息
			if part, ok := event.Properties["part"].(map[string]interface{}); ok {
				if pid, _ := part["id"].(string); pid != "" {
					if pt, _ := part["type"].(string); pt != "" {
						partTypeMap[pid] = pt
					}
				}
			}

		case "message.part.delta":
			partID, _ := event.Properties["partID"].(string)
			delta, _ := event.Properties["delta"].(string)
			if delta == "" {
				continue
			}
			if partTypeMap[partID] == "reasoning" {
				reasoningB.WriteString(delta)
				writeChunk(map[string]interface{}{"reasoning_content": delta})
			} else {
				contentB.WriteString(delta)
				writeChunk(map[string]interface{}{"content": delta})
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return contentB.String(), reasoningB.String(), fmt.Errorf("event stream read: %w", err)
	}
	return contentB.String(), reasoningB.String(), fmt.Errorf("event stream ended without session.idle")
}

// StreamStreamer 异步流式转发 SSE 事件。
type StreamStreamer struct {
	content   string
	reasoning string
	err       error
	done      chan struct{}
}

// StartStream 后台启动流式转发，边从 MiMoCode 读事件边写给客户端。
func (c *MiMoCodeClient) StartStream(ctx context.Context, sessionID string, timeout time.Duration, model string, w gin.ResponseWriter) *StreamStreamer {
	s := &StreamStreamer{done: make(chan struct{})}
	go func() {
		defer close(s.done)
		s.content, s.reasoning, s.err = c.StreamToClient(ctx, sessionID, timeout, model, w)
	}()
	return s
}

// Wait 阻塞等待流式转发完成。
func (s *StreamStreamer) Wait() (content string, reasoning string, err error) {
	<-s.done
	return s.content, s.reasoning, s.err
}

// ResponseCollector 异步收集 SSE 事件，支持先订阅后发 prompt 的流程。
type ResponseCollector struct {
	content  string
	reasoning string
	err      error
	done     chan struct{}
}

// StartCollect 后台启动 SSE 事件收集，调用方需在收集启动后发送 prompt。
func (c *MiMoCodeClient) StartCollect(ctx context.Context, sessionID string, timeout time.Duration) *ResponseCollector {
	rc := &ResponseCollector{done: make(chan struct{})}
	go func() {
		defer close(rc.done)
		rc.content, rc.reasoning, rc.err = c.CollectResponse(ctx, sessionID, timeout)
	}()
	return rc
}

// Wait 阻塞等待 SSE 收集完成。
func (rc *ResponseCollector) Wait() (content string, reasoning string, err error) {
	<-rc.done
	return rc.content, rc.reasoning, rc.err
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
	partTypeMap := map[string]string{}

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var event miMoEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			continue
		}

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

		case "message.part.updated":
			partJSON, ok := event.Properties["part"].(map[string]interface{})
			if !ok {
				continue
			}
			if pid, _ := partJSON["id"].(string); pid != "" {
				if pt, _ := partJSON["type"].(string); pt != "" {
					partTypeMap[pid] = pt
				}
			}
			partText, _ := partJSON["text"].(string)
			partType, _ := partJSON["type"].(string)
			if partText == "" {
				continue
			}
			switch partType {
			case "reasoning":
				reasoningB.Reset()
				reasoningB.WriteString(partText)
			case "text":
				contentB.Reset()
				contentB.WriteString(partText)
			}

		case "message.part.delta":
			partID, _ := event.Properties["partID"].(string)
			delta, _ := event.Properties["delta"].(string)
			if delta == "" {
				continue
			}
			if partTypeMap[partID] == "reasoning" {
				reasoningB.WriteString(delta)
			} else {
				contentB.WriteString(delta)
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
