package handlers

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/oauth"
)

const (
	copilotClientID   = "Iv1.b507a08c87ecfe98"
	copilotDeviceAuth = "https://github.com/login/device/code"
	copilotTokenURL   = "https://github.com/login/oauth/access_token"
	copilotScopes     = "read:user"
)

// deviceFlowState 保存单次 device flow 会话的设备码状态，由前端轮询用。
type deviceFlowState struct {
	code   string
	provider *oauth.DeviceFlowProvider
	expiry time.Time
}

var (
	deviceFlowMu     sync.Mutex
	deviceFlowStates = make(map[string]*deviceFlowState) // device_code → state
)

// copilotStart 发起 GitHub Copilot OAuth Device Flow，返回 user_code 和验证链接。
func copilotStart(c *gin.Context) {
	httpClient, err := getHTTPClient()
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, "failed to get http client")
		return
	}

	provider := oauth.NewDeviceFlowProvider(oauth.DeviceFlowProviderParams{
		Config: oauth.DeviceFlowConfig{
			DeviceAuthURL: copilotDeviceAuth,
			TokenURL:      copilotTokenURL,
			ClientID:      copilotClientID,
			Scopes:        []string{copilotScopes},
		},
		HTTPClient: httpClient,
	})

	respFlow, err := provider.Start(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, "failed to start device flow: "+err.Error())
		return
	}

	// 保存设备码状态，前端 poll 时需要
	deviceFlowMu.Lock()
	deviceFlowStates[respFlow.DeviceCode] = &deviceFlowState{
		code:     respFlow.DeviceCode,
		provider: provider,
		expiry:   time.Now().Add(time.Duration(respFlow.ExpiresIn) * time.Second),
	}
	deviceFlowMu.Unlock()

	resp.Success(c, gin.H{
		"user_code":        respFlow.UserCode,
		"verification_uri": respFlow.VerificationURI,
		"device_code":      respFlow.DeviceCode,
		"expires_in":       respFlow.ExpiresIn,
		"interval":         respFlow.Interval,
	})
}

// copilotPoll 轮询 device flow 状态：用户完成授权后返回 credentials JSON。
func copilotPoll(c *gin.Context) {
	var req struct {
		DeviceCode string `json:"device_code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, "device_code required")
		return
	}

	deviceFlowMu.Lock()
	state, ok := deviceFlowStates[req.DeviceCode]
	deviceFlowMu.Unlock()

	if !ok {
		resp.Error(c, http.StatusBadRequest, "unknown device_code")
		return
	}

	// 检查是否过期
	if time.Now().After(state.expiry) {
		deviceFlowMu.Lock()
		delete(deviceFlowStates, req.DeviceCode)
		deviceFlowMu.Unlock()
		resp.Success(c, gin.H{"status": "expired"})
		return
	}

	creds, err := state.provider.Poll(c.Request.Context(), req.DeviceCode)
	if err != nil {
		errMsg := err.Error()

		switch {
		case errMsg == "authorization_pending":
			resp.Success(c, gin.H{"status": "pending"})
		case errMsg == "slow_down":
			resp.Success(c, gin.H{"status": "slow_down"})
		case errMsg == "expired_token":
			deviceFlowMu.Lock()
			delete(deviceFlowStates, req.DeviceCode)
			deviceFlowMu.Unlock()
			resp.Success(c, gin.H{"status": "expired"})
		case errMsg == "access_denied":
			deviceFlowMu.Lock()
			delete(deviceFlowStates, req.DeviceCode)
			deviceFlowMu.Unlock()
			resp.Success(c, gin.H{"status": "denied"})
		default:
			resp.Error(c, http.StatusInternalServerError, "poll failed: "+errMsg)
		}
		return
	}

	// 授权成功，序列化 credentials 并清理状态
	credsJSON, err := json.Marshal(creds)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, "failed to serialize credentials")
		return
	}

	deviceFlowMu.Lock()
	delete(deviceFlowStates, req.DeviceCode)
	deviceFlowMu.Unlock()

	// 把 credentials 写入 channel key（如果请求里有 channel_id）
	var patchReq struct {
		DeviceCode string `json:"device_code"`
		ChannelID  int    `json:"channel_id,omitempty"`
	}
	_ = c.ShouldBindJSON(&patchReq)

	resp.Success(c, gin.H{
		"status":     "done",
		"key":        string(credsJSON),
		"channel_id": patchReq.ChannelID,
	})
}

// getHTTPClient 获取系统默认 HTTP客户端（含代理配置）。
func getHTTPClient() (*httpclient.HttpClient, error) {
	return httpclient.NewHttpClient(), nil
}
