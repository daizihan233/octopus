# MiMoCode Channel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use compose:subagent (recommended) or compose:execute to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a "mimocode" channel type to octopus that talks directly to a `mimo serve` backend via the opencode protocol, eliminating the need for a separate mimocode2api proxy.

**Architecture:** Register a new `ChannelTypeMiMoCode` channel type. When a request is routed to a MiMoCode channel, the relay bypasses the standard axonhub pipeline and directly implements the opencode protocol: create session → send prompt via `POST /session/{id}/prompt_async` → collect SSE events from `GET /event` → convert opencode parts to OpenAI response format. The user runs `mimo serve` externally; octopus just connects to it.

**Tech Stack:** Go, gin, standard `net/http` + `encoding/json` for opencode protocol, SSE parsing.

## Global Constraints

- Go 1.26+ (per go.mod)
- No new external dependencies — use stdlib `net/http`, `encoding/json`, `bufio`
- Follow existing code patterns (channel type const in model, transformer in relay, handler in relay)
- MiMoCode backend is user-managed externally (NOT started by octopus)
- The channel's `BaseUrls[0].URL` points to the `mimo serve` address (e.g. `http://127.0.0.1:10001`)
- The channel's `Keys[0].ChannelKey` is the `MIMOCODE_SERVER_PASSWORD` (for Basic auth); empty = no auth

---

## File Structure

| File | Action | Purpose |
|------|--------|---------|
| `internal/model/channel.go` | Modify | Add `ChannelTypeMiMoCode` const |
| `internal/relay/mimocode.go` | Create | MiMoCode protocol client + OpenAI↔opencode message conversion |
| `internal/relay/relay.go` | Modify | Branch `prepareAttempt`/`forward` for MiMoCode channels |
| `internal/relay/transformers.go` | Modify | Return nil outbound for MiMoCode in `newOutbound` |
| `internal/helper/fetch.go` | Modify | Add MiMoCode model fetch via `/config/providers` |

---

## Task 1: Register MiMoCode Channel Type

**Files:**
- Modify: `internal/model/channel.go:18`

- [ ] **Step 1: Add the channel type constant**

After `ChannelTypeDoubao`:

```go
const ChannelTypeDoubao llm.APIFormat = "doubao"
const ChannelTypeMiMoCode llm.APIFormat = "mimocode"
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./...`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/model/channel.go
git commit -m "feat(relay): add mimocode channel type constant (By AI)"
```

---

## Task 2: Create MiMoCode Protocol Client

**Files:**
- Create: `internal/relay/mimocode.go`

**Interfaces:**
- Produces: `MiMoCodeClient` struct with methods for the opencode protocol
- Produces: `ConvertMessages()` function for OpenAI→opencode message conversion
- Produces: `CollectResponse()` function for opencode SSE→OpenAI response conversion

- [ ] **Step 1: Create the MiMoCode protocol client**

Create `internal/relay/mimocode.go` with the following:

```go
package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/looplj/axonhub/llm"
)

// MiMoCodeClient communicates with a mimo serve backend via the opencode protocol.
type MiMoCodeClient struct {
	BaseURL    string // e.g. "http://127.0.0.1:10001"
	HTTPClient *http.Client
	AuthHeader string // "Basic ..." or empty
}

// NewMiMoCodeClient creates a client from channel config.
func NewMiMoCodeClient(baseURL, password string) *MiMoCodeClient {
	authHeader := ""
	if password != "" {
		token := fmt.Sprintf("opencode:%s", password)
		encoded := base64Encode(token)
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
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.AuthHeader != "" {
		req.Header.Set("Authorization", c.AuthHeader)
	}
	return c.HTTPClient.Do(req)
}

// MiMoCodeSession represents a session from POST /session.
type MiMoCodeSession struct {
	ID string `json:"id"`
}

// CreateSession creates a new opencode session.
func (c *MiMoCodeClient) CreateSession(ctx context.Context) (string, error) {
	resp, err := c.doRequest(ctx, http.MethodPost, "/session", map[string]string{})
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create session failed: %d %s", resp.StatusCode, string(body))
	}
	var session MiMoCodeSession
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return "", fmt.Errorf("decode session: %w", err)
	}
	return session.ID, nil
}

// DeleteSession deletes an opencode session.
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

// MiMoCodePart represents a part in the opencode protocol.
type MiMoCodePart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// For file parts
	MIME     string `json:"mime,omitempty"`
	URL      string `json:"url,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// MiMoCodePrompt is the body for POST /session/{id}/prompt_async.
type MiMoCodePrompt struct {
	Model MiMoCodeModel  `json:"model"`
	Parts []MiMoCodePart `json:"parts"`
	System string        `json:"system,omitempty"`
}

type MiMoCodeModel struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// SendPrompt sends an async prompt to the session.
func (c *MiMoCodeClient) SendPrompt(ctx context.Context, sessionID string, prompt MiMoCodePrompt) error {
	resp, err := c.doRequest(ctx, http.MethodPost, "/session/"+sessionID+"/prompt_async", prompt)
	if err != nil {
		return fmt.Errorf("send prompt: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send prompt failed: %d %s", resp.StatusCode, string(body))
	}
	return nil
}

// MiMoCodeEvent represents an SSE event from GET /event.
type MiMoCodeEvent struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Properties map[string]interface{} `json:"properties"`
}

// MiMoCodePartUpdate represents the part field in message.part.updated events.
type MiMoCodePartUpdate struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Reason    string `json:"reason,omitempty"`
	SessionID string `json:"sessionID"`
	Tokens    *struct {
		Total    int `json:"total"`
		Input    int `json:"input"`
		Output   int `json:"output"`
		Reasoning int `json:"reasoning"`
	} `json:"tokens,omitempty"`
}

// MiMoCodeError represents an error in session.error events.
type MiMoCodeError struct {
	Name string `json:"name"`
	Data struct {
		Message string `json:"message"`
	} `json:"data"`
}

// CollectResponse subscribes to the SSE event stream and collects the response.
// Returns content, reasoning, and error.
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
		return "", "", fmt.Errorf("event stream failed: %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	// Increase buffer for large SSE frames
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var contentBuilder, reasoningBuilder strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event MiMoCodeEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		// Filter events for our session
		props := event.Properties
		eventSessionID, _ := props["sessionID"].(string)
		if eventSessionID != "" && eventSessionID != sessionID {
			continue
		}

		switch event.Type {
		case "session.idle":
			return contentBuilder.String(), reasoningBuilder.String(), nil

		case "session.error":
			var miMoErr MiMoCodeError
			if errData, ok := props["error"].(map[string]interface{}); ok {
				errJSON, _ := json.Marshal(errData)
				json.Unmarshal(errJSON, &miMoErr)
			}
			return contentBuilder.String(), reasoningBuilder.String(),
				fmt.Errorf("mimocode error: %s: %s", miMoErr.Name, miMoErr.Data.Message)

		case "message.part.delta":
			field, _ := props["field"].(string)
			delta, _ := props["delta"].(string)
			if delta == "" {
				continue
			}
			if field == "reasoning" {
				reasoningBuilder.WriteString(delta)
			} else {
				contentBuilder.WriteString(delta)
			}

		case "message.part.updated":
			partJSON, ok := props["part"].(map[string]interface{})
			if !ok {
				continue
			}
			partType, _ := partJSON["type"].(string)
			partText, _ := partJSON["text"].(string)
			if partType == "reasoning" && partText != "" {
				// Use finalized text (may be more complete than deltas)
				reasoningBuilder.Reset()
				reasoningBuilder.WriteString(partText)
			} else if partType == "text" && partText != "" {
				contentBuilder.Reset()
				contentBuilder.WriteString(partText)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return contentBuilder.String(), reasoningBuilder.String(),
			fmt.Errorf("event stream read error: %w", err)
	}

	// Stream ended without session.idle — timeout or disconnect
	return contentBuilder.String(), reasoningBuilder.String(),
		fmt.Errorf("event stream ended without session.idle")
}

// MiMoCodeProviderModel represents a model from /config/providers.
type MiMoCodeProviderModel struct {
	Providers []struct {
		ID     string   `json:"id"`
		Models []string `json:"models"`
	} `json:"providers"`
}

// FetchModels fetches available models from the backend.
func (c *MiMoCodeClient) FetchModels(ctx context.Context) ([]string, error) {
	resp, err := c.doRequest(ctx, http.MethodGet, "/config/providers", nil)
	if err != nil {
		return nil, fmt.Errorf("fetch providers: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch providers failed: %d", resp.StatusCode)
	}
	var providers MiMoCodeProviderModel
	if err := json.NewDecoder(resp.Body).Decode(&providers); err != nil {
		return nil, fmt.Errorf("decode providers: %w", err)
	}
	var models []string
	for _, p := range providers.Providers {
		for _, m := range p.Models {
			models = append(models, p.ID+"/"+m)
		}
	}
	return models, nil
}

// base64Encode encodes a string to base64.
func base64Encode(s string) string {
	// Use encoding/base64 in production
	return fmt.Sprintf("%s", s) // placeholder — replaced in Task 2 step 2
}
```

- [ ] **Step 2: Fix base64 import**

Replace the `base64Encode` placeholder with proper encoding:

```go
import "encoding/base64"

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
```

Remove the placeholder function.

- [ ] **Step 3: Verify it compiles**

Run: `go build ./...`
Expected: PASS (may have unused import warnings, that's fine — other files will use it)

- [ ] **Step 4: Commit**

```bash
git add internal/relay/mimocode.go
git commit -m "feat(relay): add mimocode protocol client (By AI)"
```

---

## Task 3: Modify Relay to Handle MiMoCode Channels

**Files:**
- Modify: `internal/relay/relay.go`
- Modify: `internal/relay/transformers.go`

**Interfaces:**
- Consumes: `ChannelTypeMiMoCode` from model
- Consumes: `MiMoCodeClient`, `ConvertMessages`, `CollectResponse` from mimocode.go
- Produces: Modified `prepareAttempt` and `forward` that branch for MiMoCode

- [ ] **Step 1: Modify `newOutbound` to return nil for MiMoCode**

In `internal/relay/transformers.go`, add to the `newOutbound` function's `case llm.RequestTypeChat:` block:

```go
case llm.RequestTypeChat:
	switch channelType {
	case llm.APIFormatOpenAIChatCompletion:
		return openai.NewOutboundTransformer(baseURL, key)
	case llm.APIFormatOpenAIResponse:
		return responses.NewOutboundTransformer(baseURL, key)
	case llm.APIFormatAnthropicMessage:
		return anthropic.NewOutboundTransformer(baseURL, key)
	case llm.APIFormatGeminiContents:
		return gemini.NewOutboundTransformer(baseURL, key)
	case dbmodel.ChannelTypeDoubao:
		return doubao.NewOutboundTransformer(baseURL, key)
	case dbmodel.ChannelTypeMiMoCode:
		// MiMoCode uses its own protocol, handled in relay.forward()
		return nil, nil
	default:
		return nil, fmt.Errorf("channel type %s is not compatible with %s request", channelType, requestType)
	}
```

Do the same for `RequestTypeEmbedding` and `RequestTypeImage` — return `nil, nil` for `ChannelTypeMiMoCode` (these request types are not supported by MiMoCode anyway).

- [ ] **Step 2: Modify `prepareAttempt` to handle nil outAdapter**

In `internal/relay/relay.go`, modify `prepareAttempt()` around line 148:

```go
outAdapter, err := newOutbound(channel.Type, r.internalRequest, channel.GetBaseUrl(), usedKey.ChannelKey)
if err != nil {
    r.iter.Skip(channel.ID, usedKey.ID, channel.Name, err.Error())
    return nil, nil
}

// MiMoCode: nil outAdapter means protocol is handled in forward()
```

The existing code already works — `outAdapter` will be nil for MiMoCode, and `forward()` will check for it.

- [ ] **Step 3: Modify `forward` to branch for MiMoCode**

In `internal/relay/relay.go`, modify the `forward()` method. After the existing pipeline code, add a MiMoCode branch at the top of the method:

```go
func (ra *relayAttempt) forward() (int, error) {
    // MiMoCode: bypass axonhub pipeline, use opencode protocol directly
    if ra.outAdapter == nil && ra.channel.Type == dbmodel.ChannelTypeMiMoCode {
        return ra.forwardMiMoCode()
    }

    // ... existing pipeline code unchanged ...
}
```

- [ ] **Step 4: Implement `forwardMiMoCode`**

Add this method to `relay.go` (or keep it in `mimocode.go` — either works):

```go
func (ra *relayAttempt) forwardMiMoCode() (int, error) {
    ctx := ra.c.Request.Context()
    client := NewMiMoCodeClient(ra.channel.GetBaseUrl(), ra.usedKey.ChannelKey)

    // 1. Create session
    sessionID, err := client.CreateSession(ctx)
    if err != nil {
        return 0, fmt.Errorf("mimocode create session: %w", err)
    }
    defer client.DeleteSession(context.Background(), sessionID)

    // 2. Convert messages to opencode parts
    systemMsg, parts := ConvertMessages(ra.internalRequest.Messages)
    if len(parts) == 0 {
        return 0, fmt.Errorf("mimocode: no non-system messages")
    }

    // 3. Build prompt
    modelParts := strings.SplitN(ra.internalRequest.Model, "/", 2)
    providerID := "opencode"
    modelID := ra.internalRequest.Model
    if len(modelParts) == 2 {
        providerID = modelParts[0]
        modelID = modelParts[1]
    }

    prompt := MiMoCodePrompt{
        Model: MiMoCodeModel{
            ProviderID: providerID,
            ModelID:    modelID,
        },
        Parts:  parts,
        System: systemMsg,
    }

    // 4. Send prompt
    if err := client.SendPrompt(ctx, sessionID, prompt); err != nil {
        return 0, fmt.Errorf("mimocode send prompt: %w", err)
    }

    // 5. Collect response via SSE
    content, reasoning, err := client.CollectResponse(ctx, sessionID, 3*time.Minute)
    if err != nil {
        return 0, fmt.Errorf("mimocode collect response: %w", err)
    }

    // 6. Build OpenAI response
    response := buildOpenAIResponse(ra.internalRequest.Model, content, reasoning)

    // 7. Write response
    ra.c.Header("Content-Type", "application/json")
    ra.c.JSON(http.StatusOK, response)

    return http.StatusOK, nil
}
```

- [ ] **Step 5: Add `buildOpenAIResponse` helper**

```go
func buildOpenAIResponse(model, content, reasoning string) map[string]interface{} {
    message := map[string]interface{}{
        "role":    "assistant",
        "content": content,
    }
    if reasoning != "" {
        message["reasoning_content"] = reasoning
    }

    return map[string]interface{}{
        "id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli()),
        "object":  "chat.completion",
        "created": time.Now().Unix(),
        "model":   model,
        "choices": []map[string]interface{}{
            {
                "index":         0,
                "message":       message,
                "finish_reason": "stop",
            },
        },
    }
}
```

- [ ] **Step 6: Add missing imports to relay.go**

Add to `relay.go` imports:
```go
dbmodel "github.com/bestruirui/octopus/internal/model"
```

And to `mimocode.go`:
```go
"encoding/base64"
"strings"
```

- [ ] **Step 7: Verify it compiles**

Run: `go build ./...`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add internal/relay/relay.go internal/relay/transformers.go
git commit -m "feat(relay): integrate mimocode protocol into relay pipeline (By AI)"
```

---

## Task 4: Add OpenAI→opencode Message Conversion

**Files:**
- Create or extend: `internal/relay/mimocode.go` (add `ConvertMessages` function)

**Interfaces:**
- Consumes: `llm.Message` from axonhub
- Produces: `system string`, `parts []MiMoCodePart`

- [ ] **Step 1: Implement ConvertMessages**

Add to `internal/relay/mimocode.go`:

```go
// ConvertMessages converts OpenAI-format messages to opencode parts + system prompt.
func ConvertMessages(messages []llm.Message) (string, []MiMoCodePart) {
    var systemChunks []string
    var parts []MiMoCodePart

    for _, m := range messages {
        role := strings.ToLower(string(m.Role))

        switch role {
        case "system":
            if text := normalizeTextContent(m.Content); text != "" {
                systemChunks = append(systemChunks, text)
            }

        case "user":
            text := normalizeTextContent(m.Content)
            if text != "" {
                parts = append(parts, MiMoCodePart{Type: "text", Text: "USER: " + text})
            }
            // Handle image content
            if contentArray, ok := m.Content.([]interface{}); ok {
                for _, item := range contentArray {
                    if partMap, ok := item.(map[string]interface{}); ok {
                        if partMap["type"] == "image_url" {
                            imageURL := ""
                            if urlStr, ok := partMap["image_url"].(string); ok {
                                imageURL = urlStr
                            } else if urlObj, ok := partMap["image_url"].(map[string]interface{}); ok {
                                imageURL, _ = urlObj["url"].(string)
                            }
                            if imageURL != "" {
                                mime := "image/jpeg"
                                if strings.HasPrefix(imageURL, "data:") {
                                    parts = append(parts, MiMoCodePart{
                                        Type: "file", MIME: mime, URL: imageURL, Filename: "image",
                                    })
                                }
                            }
                        }
                    }
                }
            }

        case "assistant":
            text := normalizeTextContent(m.Content)
            if text != "" {
                parts = append(parts, MiMoCodePart{Type: "text", Text: "ASSISTANT: " + text})
            }
            // Serialize tool_calls if present
            if len(m.ToolCalls) > 0 {
                toolCallsJSON, _ := json.Marshal(m.ToolCalls)
                parts = append(parts, MiMoCodePart{
                    Type: "text",
                    Text: fmt.Sprintf("ASSISTANT: <function_calls>%s</function_calls>", string(toolCallsJSON)),
                })
            }

        case "tool":
            text := normalizeTextContent(m.Content)
            if text != "" {
                toolResult := map[string]interface{}{
                    "tool_call_id": m.ToolCallID,
                    "name":         m.Name,
                    "content":      text,
                }
                resultJSON, _ := json.Marshal(toolResult)
                parts = append(parts, MiMoCodePart{
                    Type: "text",
                    Text: fmt.Sprintf("TOOL_RESULT: %s", string(resultJSON)),
                })
            }
        }
    }

    system := strings.Join(systemChunks, "\n\n")
    return system, parts
}

func normalizeTextContent(content interface{}) string {
    switch v := content.(type) {
    case string:
        return v
    case []interface{}:
        var texts []string
        for _, item := range v {
            if partMap, ok := item.(map[string]interface{}); ok {
                if partMap["type"] == "text" {
                    if text, ok := partMap["text"].(string); ok {
                        texts = append(texts, text)
                    }
                }
            }
        }
        return strings.Join(texts, "")
    default:
        return ""
    }
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./...`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/relay/mimocode.go
git commit -m "feat(relay): add OpenAI to opencode message conversion (By AI)"
```

---

## Task 5: Add MiMoCode Model Fetch Support

**Files:**
- Modify: `internal/helper/fetch.go`

**Interfaces:**
- Consumes: `ChannelTypeMiMoCode` from model
- Consumes: `MiMoCodeClient.FetchModels` from mimocode.go

- [ ] **Step 1: Add MiMoCode case to FetchModels**

In `internal/helper/fetch.go`, modify the `FetchModels` function:

```go
func FetchModels(ctx context.Context, request model.Channel) ([]string, error) {
    client, err := ChannelHttpClient(&request)
    if err != nil {
        return nil, err
    }
    fetchModel := make([]string, 0)
    switch request.Type {
    case llm.APIFormatAnthropicMessage:
        fetchModel, err = fetchAnthropicModels(client, ctx, request)
    case llm.APIFormatGeminiContents:
        fetchModel, err = fetchGeminiModels(client, ctx, request)
    case model.ChannelTypeMiMoCode:
        fetchModel, err = fetchMiMoCodeModels(ctx, request)
    default:
        fetchModel, err = fetchOpenAIModels(client, ctx, request)
    }
    // ... rest unchanged ...
}
```

- [ ] **Step 2: Implement fetchMiMoCodeModels**

Add to `internal/helper/fetch.go`:

```go
func fetchMiMoCodeModels(ctx context.Context, request model.Channel) ([]string, error) {
    password := ""
    if len(request.Keys) > 0 {
        password = request.Keys[0].ChannelKey
    }
    client := relay.NewMiMoCodeClient(request.GetBaseUrl(), password)
    return client.FetchModels(ctx)
}
```

This requires importing the relay package. If this creates a circular import, move `fetchMiMoCodeModels` to `internal/relay/mimocode.go` instead and have `fetch.go` call a function from relay.

**Alternative (avoid circular import):** Move the model fetch logic to a shared location or use an interface. The simplest approach: define `FetchMiMoCodeModels` as a package-level function variable in `helper/fetch.go` that gets set by `relay` during init.

Actually, the cleanest approach: put `FetchMiMoCodeModels` in `helper/fetch.go` itself, making the HTTP calls directly without importing relay:

```go
func fetchMiMoCodeModels(ctx context.Context, request model.Channel) ([]string, error) {
    password := ""
    if len(request.Keys) > 0 {
        password = request.Keys[0].ChannelKey
    }
    baseURL := strings.TrimRight(request.GetBaseUrl(), "/")

    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/config/providers", nil)
    req.Header.Set("Content-Type", "application/json")
    if password != "" {
        token := fmt.Sprintf("opencode:%s", password)
        encoded := base64.StdEncoding.EncodeToString([]byte(token))
        req.Header.Set("Authorization", "Basic "+encoded)
    }

    httpClient, err := ChannelHttpClient(&request)
    if err != nil {
        return nil, err
    }
    resp, err := httpClient.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    var providers struct {
        Providers []struct {
            ID     string   `json:"id"`
            Models []string `json:"models"`
        } `json:"providers"`
    }
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
```

Add imports: `encoding/base64`, `encoding/json`, `strings`.

- [ ] **Step 3: Verify it compiles**

Run: `go build ./...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/helper/fetch.go
git commit -m "feat(helper): add mimocode model fetch via /config/providers (By AI)"
```

---

## Task 6: Handle Streaming Responses (Optional Enhancement)

The current implementation returns non-streaming responses. For streaming support, the relay's `forwardMiMoCode` would need to write SSE events as they arrive. This is a follow-up enhancement.

For v1, non-streaming is sufficient — the client gets a complete response after the model finishes.

- [ ] **Step 1: Skip for now — document as future work**

No code changes. The non-streaming path in Task 3 step 3 handles the basic case.

- [ ] **Step 2: Commit (docs only)**

```bash
# No commit needed for this task
```

---

## Verification Plan

After all tasks:

1. **Unit test:** Start a local `mimo serve` instance. Create a channel in octopus with type "mimocode", base URL pointing to `mimo serve`, and no key (or the server password).

2. **Model fetch:** Use the "fetch model" API to verify models are returned from `/config/providers`.

3. **Chat completion:** Send a POST to `/v1/chat/completions` with a simple message. Verify the response is a valid OpenAI-format completion.

4. **Edge cases:** Test with empty messages, system prompts, and multi-turn conversations.
