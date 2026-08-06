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
	"sync"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/log"
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
// 返回的是可通过 /chat/completions 真实调用的模型 id 列表：
// - 目录中的 auto / *-auto / *-free-auto 是 Auto 路由器别名，直接传会 400 model_not_supported；
// - Free/Student 账号目录还会列出 premium 模型，必须按计划白名单过滤（参考 The AI Counsel）。
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

	// 查询账号计划：Free/Student 的目录和付费账号差异很大，需要据此过滤。
	// 查询失败时降级按付费目录过滤，不阻塞模型拉取。
	isFreePlan, err := fetchCopilotAccountInfo(ctx, client, creds.AccessToken)
	if err != nil {
		log.Debugf("copilot account info lookup failed, assume paid plan: %v", err)
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
		Data []copilotModelEntry `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	// 只保留 chat/completions 可调用的模型 id。auto 等路由器别名和 Free 账号
	// 无权使用的 premium 模型都会被过滤，避免透传后上游 400 model_not_supported。
	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if !isCopilotCallableModel(m, isFreePlan) {
			continue
		}
		models = append(models, m.ID)
	}

	// Free/Student 账号目录常只返回 auto 或过滤后为空：兜底返回 chat/completions
	// 实测可用的种子模型（参考 The AI Counsel 的 COPILOT_MODEL_SEEDS）。
	if len(models) == 0 {
		models = append(models, "gpt-4.1", "gpt-4o")
	}

	// 预启用模型 policy（参考 oh-my-pi）：部分模型（如 Claude、Grok）首次
	// 使用前需要先接受 policy，否则即使模型名正确也会报 model_not_supported。
	// 逐个调 POST /models/{id}/policy 启用，失败静默不影响列表返回。
	enableCopilotModels(ctx, client, copilotToken, baseURL, models)

	return models, nil
}

// copilotModelEntry /models 响应的单条模型记录，只保留过滤所需的字段。
type copilotModelEntry struct {
	ID                  string   `json:"id"`
	SupportedEndpoints  []string `json:"supported_endpoints"`
	ModelPickerEnabled  *bool    `json:"model_picker_enabled"`
	Capabilities        struct {
		Family string `json:"family"`
	} `json:"capabilities"`
	Policy struct {
		State string `json:"state"`
	} `json:"policy"`
}

// copilotFreeSKUs 视为 Free/Student 计划的 access_type_sku（参考 The AI Counsel / relay-ai）。
var copilotFreeSKUs = map[string]bool{
	"free_limited_copilot":    true,
	"free_educational_quota":  true,
	"no_auth_limited_copilot": true,
}

// copilotFreeModelAllowlist Free/Student 计划在 chat/completions 实测可调的模型白名单；
// 目录会过度列出模型，Free 计划手动选择只允许这些（参考 The AI Counsel）。
var copilotFreeModelAllowlist = map[string]bool{
	"gpt-4.1":     true,
	"gpt-4o":      true,
	"gpt-4o-mini": true,
	"raptor-mini": true,
	"goldeneye":   true,
}

// copilotFreeModelBlocklist 目录常把这些标成可用，但 Free 计划的 chat/completions 会拒绝。
var copilotFreeModelBlocklist = map[string]bool{
	"gpt-5-mini": true,
}

// fetchCopilotAccountInfo 查询 GitHub 账号的 Copilot 计划（GET /copilot_internal/user）。
// 返回 true 表示 Free/Student 计划（需要按白名单限制模型选择）。
func fetchCopilotAccountInfo(ctx context.Context, client *http.Client, accessToken string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/copilot_internal/user", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("X-Github-Api-Version", "2025-04-01")

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("copilot account lookup %d: %s", resp.StatusCode, string(body))
	}

	var data struct {
		AccessTypeSKU string `json:"access_type_sku"`
		CopilotPlan   string `json:"copilot_plan"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return false, err
	}
	sku := strings.ToLower(strings.TrimSpace(data.AccessTypeSKU))
	plan := strings.ToLower(strings.TrimSpace(data.CopilotPlan))
	return copilotFreeSKUs[sku] || plan == "free", nil
}

// isCopilotCallableModel 判断模型能否通过 /chat/completions 调用。
// auto 及 *-auto / *-free-auto 是 Auto 路由器别名，不是可调用模型 id；
// Free/Student 计划只保留严格白名单（参考 The AI Counsel）。
func isCopilotCallableModel(m copilotModelEntry, isFreePlan bool) bool {
	mid := strings.ToLower(m.ID)
	if mid == "auto" || strings.HasSuffix(mid, "-auto") || strings.HasSuffix(mid, "-free-auto") {
		return false
	}
	if strings.Contains(mid, "embedding") {
		return false
	}
	if strings.Contains(strings.ToLower(m.Capabilities.Family), "embedding") {
		return false
	}
	// supported_endpoints 非空时必须包含 chat/completions（排除纯 embedding 等端点）
	if len(m.SupportedEndpoints) > 0 {
		chatOK := false
		for _, ep := range m.SupportedEndpoints {
			ep = strings.ToLower(strings.TrimRight(ep, "/"))
			if ep == "/chat/completions" || strings.HasSuffix(ep, "chat/completions") {
				chatOK = true
				break
			}
		}
		if !chatOK {
			return false
		}
	}
	if m.ModelPickerEnabled != nil && !*m.ModelPickerEnabled {
		return false
	}
	if strings.EqualFold(m.Policy.State, "disabled") {
		return false
	}
	if isFreePlan {
		if copilotFreeModelBlocklist[mid] {
			return false
		}
		return copilotFreeModelAllowlist[mid]
	}
	return true
}

// enableCopilotModels 预启用 Copilot 模型的 policy（POST /models/{id}/policy）。
// 参考 oh-my-pi：批量 5 个并发，失败静默（模型是否需启用由服务端决定）。
func enableCopilotModels(ctx context.Context, client *http.Client, copilotToken, baseURL string, models []string) {
	const batchSize = 5
	for i := 0; i < len(models); i += batchSize {
		end := i + batchSize
		if end > len(models) {
			end = len(models)
		}
		var wg sync.WaitGroup
		for _, id := range models[i:end] {
			wg.Add(1)
			go func(modelID string) {
				defer wg.Done()
				if err := enableCopilotModel(ctx, client, copilotToken, baseURL, modelID); err != nil {
					log.Debugf("copilot enable model %s policy: %v", modelID, err)
				}
			}(id)
		}
		wg.Wait()
	}
}

// enableCopilotModel 对单个模型发起 policy 启用请求。
// 端点与请求头参考 oh-my-pi 的 enableGitHubCopilotModel。
func enableCopilotModel(ctx context.Context, client *http.Client, copilotToken, baseURL, modelID string) error {
	body, err := json.Marshal(map[string]string{"state": "enabled"})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/models/"+modelID+"/policy", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+copilotToken)
	req.Header.Set("openai-intent", "chat-policy")
	req.Header.Set("x-interaction-type", "chat-policy")
	req.Header.Set("Editor-Version", "vscode/1.95.0")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.26.7")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("copilot-integration-id", "vscode-chat")
	req.Header.Set("x-github-api-version", "2025-04-01")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 4xx 通常是"无需启用"或"已启用"，不算错误；网络/5xx 才记录
	if resp.StatusCode >= 500 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("policy enable %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
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
