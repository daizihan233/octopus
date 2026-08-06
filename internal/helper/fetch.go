package helper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"runtime"
	"strings"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/dlclark/regexp2"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/transformer"
)

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
		fetchModel, err = fetchMiMoCodeModels(client, ctx, request)
	case model.ChannelTypeCopilot:
		fetchModel, err = fetchCopilotModels(client, ctx, request)
	default:
		fetchModel, err = fetchOpenAIModels(client, ctx, request)
	}
	if err != nil {
		return nil, err
	}
	if request.MatchRegex != nil && *request.MatchRegex != "" {
		matchModel := make([]string, 0)
		re, err := regexp2.Compile(*request.MatchRegex, regexp2.ECMAScript)
		if err != nil {
			return nil, err
		}
		for _, model := range fetchModel {
			matched, err := re.MatchString(model)
			if err != nil {
				return nil, err
			}
			if matched {
				matchModel = append(matchModel, model)
			}
		}
		return matchModel, nil
	}
	return fetchModel, nil
}

// refer: https://platform.openai.com/docs/api-reference/models/list
func fetchOpenAIModels(client *http.Client, ctx context.Context, request model.Channel) ([]string, error) {
	baseURL := transformer.NormalizeBaseURL(request.GetBaseUrl(), "v1")
	if request.Type == model.ChannelTypeDoubao {
		baseURL = transformer.NormalizeBaseURL(request.GetBaseUrl(), "v3")
	}
	req, _ := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		baseURL+"/models",
		nil,
	)
	req.Header.Set("Authorization", "Bearer "+request.GetChannelKey().ChannelKey)
	applyCustomHeaders(req, request)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result model.OpenAIModelList

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

// refer: https://ai.google.dev/api/models
func fetchGeminiModels(client *http.Client, ctx context.Context, request model.Channel) ([]string, error) {
	var allModels []string
	pageToken := ""
	baseURL := transformer.NormalizeBaseURL(request.GetBaseUrl(), "v1beta")
	// Gemini transformer 会保留用户显式填写的 /v1；这里同样处理，避免把 /v1 拼成 /v1/v1beta。
	if strings.HasSuffix(strings.TrimRight(request.GetBaseUrl(), "/"), "/v1") {
		baseURL = transformer.NormalizeBaseURL(request.GetBaseUrl(), "")
	}

	for {
		req, _ := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			baseURL+"/models",
			nil,
		)
		req.Header.Set("X-Goog-Api-Key", request.GetChannelKey().ChannelKey)
		applyCustomHeaders(req, request)
		if pageToken != "" {
			q := req.URL.Query()
			q.Add("pageToken", pageToken)
			req.URL.RawQuery = q.Encode()
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		var result model.GeminiModelList

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, err
		}

		for _, m := range result.Models {
			name := strings.TrimPrefix(m.Name, "models/")
			allModels = append(allModels, name)
		}

		if result.NextPageToken == "" {
			break
		}
		pageToken = result.NextPageToken
	}
	if len(allModels) == 0 {
		return fetchOpenAIModels(client, ctx, request)
	}
	return allModels, nil
}

// refer: https://platform.claude.com/docs
func fetchAnthropicModels(client *http.Client, ctx context.Context, request model.Channel) ([]string, error) {

	var allModels []string
	var afterID string
	baseURL := transformer.NormalizeBaseURL(request.GetBaseUrl(), "v1")
	for {

		req, _ := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			baseURL+"/models",
			nil,
		)
		req.Header.Set("X-Api-Key", request.GetChannelKey().ChannelKey)
		req.Header.Set("Anthropic-Version", "2023-06-01")
		applyCustomHeaders(req, request)
		// 设置多页参数
		q := req.URL.Query()

		if afterID != "" {
			q.Set("after_id", afterID)
		}
		req.URL.RawQuery = q.Encode()

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		var result model.AnthropicModelList

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, err
		}

		for _, m := range result.Data {
			allModels = append(allModels, m.ID)
		}

		if !result.HasMore {
			break
		}

		afterID = result.LastID
	}
	if len(allModels) == 0 {
		return fetchOpenAIModels(client, ctx, request)
	}
	return allModels, nil
}

func applyCustomHeaders(req *http.Request, channel model.Channel) {
	for _, header := range channel.CustomHeader {
		if header.HeaderKey != "" {
			req.Header.Set(header.HeaderKey, header.HeaderValue)
		}
	}
}

func fetchMiMoCodeModels(client *http.Client, ctx context.Context, request model.Channel) ([]string, error) {
	baseURL := strings.TrimRight(request.GetBaseUrl(), "/")
	jwt, err := fetchMiMoJWT(ctx, client, baseURL)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/free-ai/openai/models", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("User-Agent", "mimocode/0.1.0")
	req.Header.Set("X-Mimo-Source", "mimocode-cli-free")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mimocode models %d", resp.StatusCode)
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

func fetchMiMoJWT(ctx context.Context, client *http.Client, baseURL string) (string, error) {
	hostname, _ := os.Hostname()
	username := ""
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	payload := strings.Join([]string{hostname, runtime.GOOS, runtime.GOARCH, runtime.GOARCH, username}, "|")
	clientID := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
	body, _ := json.Marshal(map[string]string{"client": clientID})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/free-ai/bootstrap", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mimocode/0.1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bootstrap %d", resp.StatusCode)
	}
	var data struct {
		JWT string `json:"jwt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	return data.JWT, nil
}

// fetchCopilotModels 调用 Copilot 的 /models 端点获取当前账号可用模型。
// channel key 是 OAuthCredentials JSON，从中取出 access_token 换 Copilot JWT 再拉模型。
// 返回的是 /models 的真实模型 id 列表（如 gpt-4.1、gpt-5-mini），不含任何占位名。
func fetchCopilotModels(client *http.Client, ctx context.Context, request model.Channel) ([]string, error) {
	credsJSON := request.GetChannelKey().ChannelKey
	if credsJSON == "" {
		return nil, fmt.Errorf("copilot channel missing credentials")
	}
	creds, err := parseOAuthCredentials(credsJSON)
	if err != nil {
		return nil, fmt.Errorf("invalid copilot credentials: %w", err)
	}
	if creds.AccessToken == "" {
		return nil, fmt.Errorf("copilot credentials missing access_token")
	}

	copilotToken, err := fetchCopilotToken(ctx, client, creds.AccessToken)
	if err != nil {
		return nil, err
	}

	baseURL := strings.TrimRight(request.GetBaseUrl(), "/")
	if baseURL == "" {
		baseURL = "https://api.githubcopilot.com"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+copilotToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Editor-Version", "vscode/1.95.0")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.26.7")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("copilot-integration-id", "vscode-chat")
	req.Header.Set("x-github-api-version", "2025-04-01")
	applyCustomHeaders(req, request)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("copilot models %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	// 直接使用 /models 返回的真实模型 id。不要追加 "auto" 之类的占位名——
	// Copilot 上游只接受目录中的具体模型 id（gpt-4.1、gpt-5-mini 等），
	// 传不存在的名字会返回 model_not_supported。
	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

// fetchCopilotToken 用 GitHub token 换取 Copilot JWT。
func fetchCopilotToken(ctx context.Context, client *http.Client, accessToken string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/copilot_internal/v2/token", nil)
	req.Header.Set("Authorization", "token "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Editor-Version", "vscode/1.95.0")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.26.7")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("copilot token exchange failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("copilot token exchange %d: %s", resp.StatusCode, string(body))
	}

	var data struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	if data.Token == "" {
		return "", fmt.Errorf("copilot token is empty in response")
	}
	return data.Token, nil
}

// parseOAuthCredentials 从 JSON 反序列化 OAuthCredentials。
// 用于从 channel key 读取 device flow 授权结果。
type oauthCredentialsJSON struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ClientID     string `json:"client_id"`
	TokenType    string `json:"token_type"`
	Scopes       []string `json:"scopes"`
	ExpiresAt    string `json:"expires_at"`
}

func parseOAuthCredentials(jsonStr string) (*oauthCredentialsJSON, error) {
	var creds oauthCredentialsJSON
	if err := json.Unmarshal([]byte(jsonStr), &creds); err != nil {
		return nil, err
	}
	return &creds, nil
}
