package relay

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/price"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/looplj/axonhub/llm"
)

// RelayMetrics 负责最终的日志收集与持久化
type RelayMetrics struct {
	APIKeyID     int
	RequestModel string
	StartTime    time.Time

	// 首 Token 时间
	FirstTokenTime time.Time

	// 请求和最终响应体；InternalResponse 保存实际写回客户端或流式聚合后的 body，不再强制转换成 llm.Response。
	InternalRequest  *llm.Request
	InternalResponse []byte

	// 统计指标
	ActualModel string
	Stats       model.StatsMetrics

	// 参数覆盖
	ParamOverride string

	// 存储原始 usage，用于 auto 路由时修正模型后重算费用
	lastUsage *llm.Usage
}

func (m *RelayMetrics) applyActualModel() {
	if len(m.InternalResponse) == 0 {
		return
	}
	var resp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(m.InternalResponse, &resp); err != nil || resp.Model == "" {
		return
	}
	oldModel := m.ActualModel
	m.ActualModel = resp.Model
	if oldModel != resp.Model && m.lastUsage != nil {
		m.recalcCost(m.lastUsage)
	}
}

func (m *RelayMetrics) recalcCost(usage *llm.Usage) {
	modelPrice := price.GetLLMPrice(m.ActualModel)
	if modelPrice == nil {
		return
	}
	tokenDetails := usage.PromptTokensDetails
	if tokenDetails == nil {
		tokenDetails = &llm.PromptTokensDetails{}
	}
	nonCachedTokens := usage.PromptTokens - tokenDetails.CachedTokens - tokenDetails.WriteCachedTokens
	if nonCachedTokens < 0 {
		nonCachedTokens = usage.PromptTokens
	}
	m.Stats.InputCost = (float64(tokenDetails.CachedTokens)*modelPrice.CacheRead +
		float64(tokenDetails.WriteCachedTokens)*modelPrice.CacheWrite +
		float64(nonCachedTokens)*modelPrice.Input) * 1e-6
	m.Stats.OutputCost = float64(usage.CompletionTokens) * modelPrice.Output * 1e-6
}

func (m *RelayMetrics) RecordUsage(usage *llm.Usage) {
	if usage == nil {
		return
	}
	m.lastUsage = usage

	// usage 已由 axonhub/llm 标准化；octopus 仍使用本地模型价格表计算成本，所以这里只做用量落点和价格换算。
	m.Stats.InputToken = usage.PromptTokens
	m.Stats.OutputToken = usage.CompletionTokens

	modelPrice := price.GetLLMPrice(m.ActualModel)
	if modelPrice == nil {
		return
	}
	tokenDetails := usage.PromptTokensDetails
	if tokenDetails == nil {
		tokenDetails = &llm.PromptTokensDetails{}
	}
	// 缓存读、缓存写和普通输入的单价不同；如果上游返回的缓存明细超过总输入 token，就退回按全部输入 token 计费，避免出现负成本。
	nonCachedTokens := usage.PromptTokens - tokenDetails.CachedTokens - tokenDetails.WriteCachedTokens
	if nonCachedTokens < 0 {
		nonCachedTokens = usage.PromptTokens
	}
	m.Stats.InputCost = (float64(tokenDetails.CachedTokens)*modelPrice.CacheRead +
		float64(tokenDetails.WriteCachedTokens)*modelPrice.CacheWrite +
		float64(nonCachedTokens)*modelPrice.Input) * 1e-6
	m.Stats.OutputCost = float64(usage.CompletionTokens) * modelPrice.Output * 1e-6
}

func (m *RelayMetrics) Save(ctx context.Context, success bool, err error, attempts []model.ChannelAttempt) {
	// 以响应中的实际 model 为准（部分提供商支持 auto 智能路由）
	m.applyActualModel()
	duration := time.Since(m.StartTime)

	globalStats := model.StatsMetrics{
		WaitTime:    duration.Milliseconds(),
		InputToken:  m.Stats.InputToken,
		OutputToken: m.Stats.OutputToken,
		InputCost:   m.Stats.InputCost,
		OutputCost:  m.Stats.OutputCost,
	}
	if success {
		globalStats.RequestSuccess = 1
	} else {
		globalStats.RequestFailed = 1
	}

	channelID, channelName := finalChannel(attempts)
	op.StatsTotalUpdate(globalStats)
	op.StatsHourlyUpdate(globalStats)
	op.StatsDailyUpdate(context.Background(), globalStats)
	op.StatsAPIKeyUpdate(m.APIKeyID, globalStats)
	if channelID > 0 {
		// 通道成功/失败和等待时间在每次 attempt 结束时已记录；这里仅把最终响应的用量成本归到实际通道，避免重复计数。
		op.StatsChannelUpdate(channelID, model.StatsMetrics{
			InputToken:  m.Stats.InputToken,
			OutputToken: m.Stats.OutputToken,
			InputCost:   m.Stats.InputCost,
			OutputCost:  m.Stats.OutputCost,
		})
	}

	log.Infof("relay complete: model=%s, channel=%d(%s), success=%t, duration=%dms, input_token=%d, output_token=%d, input_cost=%f, output_cost=%f, total_cost=%f, attempts=%d",
		m.RequestModel, channelID, channelName, success, duration.Milliseconds(),
		m.Stats.InputToken, m.Stats.OutputToken,
		m.Stats.InputCost, m.Stats.OutputCost, m.Stats.InputCost+m.Stats.OutputCost,
		len(attempts))

	// 客户端断开或请求上下文取消后仍要保存最终审计日志，因此持久化阶段主动脱离请求取消信号。
	m.saveLog(context.WithoutCancel(ctx), err, duration, attempts, channelID, channelName)
	m.saveMonitor(ctx, err, duration, attempts)
}

// monitorCallStatus 按需求把每次渠道尝试分类为竖条颜色状态。
// 绿=成功响应；黄=上游429或所有key都429(no available key)；红=其他错误；灰=context canceled。
func monitorCallStatus(a model.ChannelAttempt) string {
	// 客户端断开/请求上下文取消导致的失败不是渠道真实故障，竖条显示灰色（cancel）。
	msg := strings.ToLower(a.Msg)
	if strings.Contains(msg, "context canceled") || strings.Contains(msg, "context cancelled") {
		return "cancel"
	}
	switch a.Status {
	case model.AttemptSuccess:
		return "ok"
	case model.AttemptFailed:
		if a.StatusCode == http.StatusTooManyRequests {
			return "429"
		}
		return "error"
	case model.AttemptSkipped:
		if strings.Contains(a.Msg, "no available key") {
			return "429"
		}
		return "error"
	case model.AttemptCircuitBreak:
		// 熔断跳过：根据触发熔断时的上游 HTTP 状态码区分原因。
		// 429 = 上游限流触发熔断（黄色），其他 = 内部错误触发熔断（红色）。
		if a.StatusCode == http.StatusTooManyRequests {
			return "429"
		}
		return "error"
	default:
		return "error"
	}
}

// saveMonitor 把本次请求的每次渠道尝试写入可用性监控缓存。
// 成功尝试的令牌/耗时/费用整条归到唯一成功通道；(channel, modelName) 用分组项的
// 模型名（auto 模型的候选即 "auto"），满足"auto 记到对应渠道的 auto 模型下"。
func (m *RelayMetrics) saveMonitor(ctx context.Context, err error, duration time.Duration, attempts []model.ChannelAttempt) {
	apiKeyName := m.requestSourceName()
	ts := m.StartTime.Unix()

	for _, a := range attempts {
		call := op.MonitorCall{Time: ts}
		if a.Status == model.AttemptSuccess {
			call.Status = "ok"
			if !m.FirstTokenTime.IsZero() {
				call.Ftut = int64(m.FirstTokenTime.Sub(m.StartTime).Milliseconds())
				if call.Ftut < 0 {
					call.Ftut = 0
				}
			}
			call.UseTime = duration.Milliseconds()
			call.Input = m.Stats.InputToken
			call.Output = m.Stats.OutputToken
			call.Cost = m.Stats.InputCost + m.Stats.OutputCost
		} else {
			call.Status = monitorCallStatus(a)
			call.UseTime = int64(a.Duration)
		}
		op.MonitorCallAdd(a.ChannelID, a.ChannelName, a.ModelName, call, apiKeyName)
	}
}

func (m *RelayMetrics) requestSourceName() string {
	if apiKey, err := op.APIKeyGet(m.APIKeyID, context.Background()); err == nil {
		return apiKey.Name
	}
	return ""
}

func finalChannel(attempts []model.ChannelAttempt) (int, string) {
	var lastID int
	var lastName string
	for i := len(attempts) - 1; i >= 0; i-- {
		a := attempts[i]
		if a.Status == model.AttemptSuccess {
			return a.ChannelID, a.ChannelName
		}
		if a.Status == model.AttemptFailed && lastID == 0 {
			lastID = a.ChannelID
			lastName = a.ChannelName
		}
	}
	return lastID, lastName
}

func (m *RelayMetrics) saveLog(ctx context.Context, err error, duration time.Duration, attempts []model.ChannelAttempt, channelID int, channelName string) {
	relayLog := model.RelayLog{
		Time:             m.StartTime.Unix(),
		RequestModelName: m.RequestModel,
		ChannelName:      channelName,
		ChannelId:        channelID,
		ActualModelName:  m.ActualModel,
		UseTime:          int(duration.Milliseconds()),
		Attempts:         attempts,
		TotalAttempts:    len(attempts),
	}

	if apiKey, getErr := op.APIKeyGet(m.APIKeyID, ctx); getErr == nil {
		relayLog.RequestAPIKeyName = apiKey.Name
	}

	// 首字时间
	if !m.FirstTokenTime.IsZero() {
		relayLog.Ftut = int(m.FirstTokenTime.Sub(m.StartTime).Milliseconds())
	}

	// 用量
	if m.Stats.InputToken > 0 || m.Stats.OutputToken > 0 {
		relayLog.InputTokens = int(m.Stats.InputToken)
		relayLog.OutputTokens = int(m.Stats.OutputToken)
		relayLog.Cost = m.Stats.InputCost + m.Stats.OutputCost
	}

	relayLog.RequestContent = m.requestContent()
	if len(m.InternalResponse) > 0 {
		relayLog.ResponseContent = string(m.InternalResponse)
	}
	if err != nil {
		relayLog.Error = err.Error()
	}

	if logErr := op.RelayLogAdd(ctx, relayLog); logErr != nil {
		log.Warnf("failed to save relay log: %v", logErr)
	}
}

func (m *RelayMetrics) requestContent() string {
	if m.InternalRequest == nil {
		return ""
	}

	reqJSON, err := json.Marshal(filterRequestForLog(m.InternalRequest))
	if err != nil {
		return ""
	}
	if m.ParamOverride == "" {
		return string(reqJSON)
	}

	var reqMap map[string]any
	if err := json.Unmarshal(reqJSON, &reqMap); err != nil {
		return string(reqJSON)
	}
	var override map[string]any
	if err := json.Unmarshal([]byte(m.ParamOverride), &override); err != nil {
		return string(reqJSON)
	}

	// 日志里的请求体要反映本次实际发给上游的参数覆盖，但失败解析时保留原始可审计内容。
	maps.Copy(reqMap, override)
	finalJSON, err := json.Marshal(reqMap)
	if err != nil {
		return string(reqJSON)
	}
	return string(finalJSON)
}

// filterRequestForLog 去掉 RawRequest 和图片二进制字段，避免 multipart 原始 body 或图片内容落库。
func filterRequestForLog(req *llm.Request) *llm.Request {
	if req == nil {
		return nil
	}
	filtered := *req
	filtered.RawRequest = nil
	if req.Image != nil {
		img := *req.Image
		if len(img.Images) > 0 {
			img.Images = nil
		}
		if len(img.Mask) > 0 {
			img.Mask = nil
		}
		filtered.Image = &img
	}
	return &filtered
}
