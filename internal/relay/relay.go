package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/pipeline/stream"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

// Handler 返回处理入站请求并转发到上游服务的 Gin handler。
func Handler(inboundType llm.APIFormat) gin.HandlerFunc {
	inAdapter := newInbound(inboundType)
	return func(c *gin.Context) {
		run, err := newRelayRun(c, inboundType, inAdapter)
		if err != nil {
			return
		}
		run.run()
	}
}

func newRelayRun(c *gin.Context, inboundType llm.APIFormat, inAdapter transformer.Inbound) (*relayRun, error) {
	internalRequest, err := parseRequest(c, inboundType, inAdapter)
	if err != nil {
		return nil, err
	}

	if supportedModels := c.GetString("supported_models"); supportedModels != "" {
		if !slices.Contains(strings.Split(supportedModels, ","), internalRequest.Model) {
			err := errors.New("model not supported")
			resp.Error(c, http.StatusBadRequest, err.Error())
			return nil, err
		}
	}

	group, err := op.GroupGetEnabledMap(internalRequest.Model, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusNotFound, "model not found")
		return nil, err
	}

	apiKeyID := c.GetInt("api_key_id")
	iter := balancer.NewIterator(group, apiKeyID, internalRequest.Model)
	if iter.Len() == 0 {
		err := errors.New("no available channel")
		resp.Error(c, http.StatusServiceUnavailable, err.Error())
		return nil, err
	}

	return &relayRun{
		c:               c,
		inAdapter:       inAdapter,
		internalRequest: internalRequest,
		metrics: &RelayMetrics{
			APIKeyID:        apiKeyID,
			RequestModel:    internalRequest.Model,
			ActualModel:     internalRequest.Model,
			StartTime:       time.Now(),
			InternalRequest: internalRequest,
		},
		iter:  iter,
		group: group,
	}, nil
}

func (r *relayRun) run() {
	ctx := r.c.Request.Context()
	var lastErr error

	for r.iter.Next() {
		select {
		case <-ctx.Done():
			log.Infof("request context canceled, stopping retry")
			r.metrics.Save(ctx, false, context.Canceled, r.iter.Attempts())
			return
		default:
		}

		attempt, err := r.prepareAttempt()
		if err != nil {
			lastErr = err
			continue
		}
		if attempt == nil {
			continue
		}

		written, err := attempt.run()
		if err == nil {
			r.metrics.Save(ctx, true, nil, r.iter.Attempts())
			return
		}
		if written {
			r.metrics.Save(ctx, false, err, r.iter.Attempts())
			return
		}
		lastErr = err
	}

	if lastErr == nil {
		lastErr = errors.New("all channels failed")
	}
	r.metrics.Save(ctx, false, lastErr, r.iter.Attempts())
	resp.Error(r.c, http.StatusBadGateway, lastErr.Error())
}

func (r *relayRun) prepareAttempt() (*relayAttempt, error) {
	item := r.iter.Item()
	channel, err := op.ChannelGet(item.ChannelID, r.c.Request.Context())
	if err != nil {
		log.Warnf("failed to get channel %d: %v", item.ChannelID, err)
		r.iter.Skip(item.ChannelID, 0, fmt.Sprintf("channel_%d", item.ChannelID), fmt.Sprintf("channel not found: %v", err))
		return nil, err
	}
	if !channel.Enabled {
		r.iter.Skip(channel.ID, 0, channel.Name, "channel disabled")
		return nil, nil
	}

	usedKey := channel.GetChannelKey()
	if usedKey.ChannelKey == "" {
		r.iter.Skip(channel.ID, 0, channel.Name, "no available key")
		return nil, nil
	}
	if r.iter.SkipCircuitBreak(channel.ID, usedKey.ID, channel.Name) {
		return nil, nil
	}

	httpClient, err := helper.ChannelHttpClient(channel)
	if err != nil {
		r.iter.Skip(channel.ID, usedKey.ID, channel.Name, err.Error())
		return nil, nil
	}

	outAdapter, err := newOutbound(channel.Type, r.internalRequest, channel.GetBaseUrl(), usedKey.ChannelKey, httpClient)
	if err != nil {
		r.iter.Skip(channel.ID, usedKey.ID, channel.Name, err.Error())
		return nil, nil
	}

	// 每次尝试都把客户端模型改成本次候选的实际上游模型；重试时会被下一候选覆盖。
	r.internalRequest.Model = item.ModelName
	r.metrics.ActualModel = item.ModelName
	r.metrics.ParamOverride = ""
	log.Infof("request model %s, mode: %d, forwarding to channel: %s model: %s (attempt %d/%d, sticky=%t)",
		r.metrics.RequestModel, r.group.Mode, channel.Name, item.ModelName,
		r.iter.Index()+1, r.iter.Len(), r.iter.IsSticky())

	return &relayAttempt{
		relayRun:   r,
		outAdapter: outAdapter,
		channel:    channel,
		usedKey:    usedKey,
	}, nil
}

// run 统一管理一次通道尝试的完整生命周期。
func (ra *relayAttempt) run() (bool, error) {
	span := ra.iter.StartAttempt(ra.channel.ID, ra.usedKey.ID, ra.channel.Name)

	upstreamStatusCode, fwdErr := ra.forward()
	if fwdErr == nil && upstreamStatusCode == 0 {
		upstreamStatusCode = http.StatusOK
	}
	ra.usedKey.StatusCode = upstreamStatusCode
	ra.usedKey.LastUseTimeStamp = time.Now().Unix()

	if fwdErr == nil {
		ra.usedKey.TotalCost += ra.metrics.Stats.InputCost + ra.metrics.Stats.OutputCost
		op.ChannelKeyUpdate(ra.usedKey)

		span.End(dbmodel.AttemptSuccess, "")
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:       span.Duration().Milliseconds(),
			RequestSuccess: 1,
		})
		balancer.RecordSuccess(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
		balancer.SetSticky(ra.metrics.APIKeyID, ra.metrics.RequestModel, ra.channel.ID, ra.usedKey.ID)
		return false, nil
	}

	op.ChannelKeyUpdate(ra.usedKey)
	span.SetStatusCode(upstreamStatusCode)
	span.End(dbmodel.AttemptFailed, fwdErr.Error())
	op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
		WaitTime:      span.Duration().Milliseconds(),
		RequestFailed: 1,
	})
	balancer.RecordFailure(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)

	return ra.c.Writer.Written(), fmt.Errorf("channel %s failed: %v", ra.channel.Name, fwdErr)
}

// parseRequest 解析并验证入站请求
func parseRequest(c *gin.Context, inboundType llm.APIFormat, inAdapter transformer.Inbound) (*llm.Request, error) {
	if inAdapter == nil {
		err := fmt.Errorf("unsupported inbound type: %s", inboundType)
		resp.Error(c, http.StatusBadRequest, err.Error())
		return nil, err
	}

	httpRequest, err := httpclient.ReadHTTPRequest(c.Request)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return nil, err
	}

	internalRequest, err := inAdapter.TransformRequest(c.Request.Context(), httpRequest)
	if err != nil {
		statusCode := http.StatusInternalServerError
		if errors.Is(err, transformer.ErrInvalidRequest) {
			statusCode = http.StatusBadRequest
		}
		resp.Error(c, statusCode, err.Error())
		return nil, err
	}
	if internalRequest.RawRequest == nil {
		internalRequest.RawRequest = httpRequest
	}

	return internalRequest, nil
}

// forward 转发请求到上游服务
func (ra *relayAttempt) forward() (int, error) {
	// MiMoCode: 绕过 axonhub pipeline，直接走 opencode 协议
	if ra.outAdapter == nil && ra.channel.Type == dbmodel.ChannelTypeMiMoCode {
		return ra.forwardMiMoCode()
	}

	ctx := ra.c.Request.Context()
	if ra.internalRequest.RawRequest == nil {
		return 0, fmt.Errorf("missing raw request")
	}

	httpClient, err := helper.ChannelHttpClient(ra.channel)
	if err != nil {
		log.Warnf("failed to get http client: %v", err)
		return 0, err
	}

	relayMiddleware := &relayPipelineMiddleware{attempt: ra}

	pipelineOpts := []pipeline.Option{
		pipeline.WithMiddlewares(stream.EnsureUsage(), relayMiddleware),
		pipeline.WithEmptyResponseDetection(),
	}
	// 将分组的首 token 超时传入 pipeline，覆盖从 HTTP 请求发出到收到首个
	// 事件的完整窗口（T0→T2），在 writeStream 定时器启动之前就能通过
	// context 取消中断上游阻塞的 HTTP 连接。
	if ra.group.FirstTokenTimeOut > 0 {
		pipelineOpts = append(pipelineOpts, pipeline.WithResponseTimeouts(
			time.Duration(ra.group.FirstTokenTimeOut)*time.Second, // 流式首事件超时
			0, // 非流式超时：由 pipeline 默认处理，此处不覆盖
		))
	}

	result, err := pipeline.NewFactory(httpclient.NewHttpClientWithClient(httpClient)).
		Pipeline(
			&parsedRequestInbound{Inbound: ra.inAdapter, request: ra.internalRequest},
			ra.outAdapter,
			pipelineOpts...,
		).
		Process(ctx, ra.internalRequest.RawRequest)
	if err != nil {
		return relayMiddleware.upstreamStatusCode, err
	}
	if result == nil {
		return 0, fmt.Errorf("empty pipeline result")
	}
	if result.Stream {
		if err := ra.writeStream(ctx, result.EventStream); err != nil {
			return http.StatusOK, err
		}
		return http.StatusOK, nil
	}
	if result.Response == nil {
		return 0, fmt.Errorf("empty pipeline response")
	}
	ra.metrics.InternalResponse = result.Response.Body
	statusCode := result.Response.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	contentType := "application/json"
	if result.Response.Headers != nil {
		for key, values := range result.Response.Headers {
			for _, value := range values {
				ra.c.Header(key, value)
			}
		}
		if result.Response.Headers.Get("Content-Type") != "" {
			contentType = result.Response.Headers.Get("Content-Type")
		}
	}
	ra.c.Data(statusCode, contentType, result.Response.Body)
	return statusCode, nil
}

func (ra *relayAttempt) applyChannelRequestOptions(outboundRequest *httpclient.Request) {
	// ParamOverride 只覆盖 JSON 请求体；multipart 图片编辑等请求不能按 map 合并。
	if ra.channel.ParamOverride != nil && *ra.channel.ParamOverride != "" && strings.Contains(strings.ToLower(outboundRequest.Headers.Get("Content-Type")+" "+outboundRequest.ContentType), "application/json") {
		var bodyMap map[string]any
		if err := json.Unmarshal(outboundRequest.Body, &bodyMap); err != nil {
			log.Warnf("failed to unmarshal request body: %v, skipping param_override", err)
		} else {
			var override map[string]any
			if err := json.Unmarshal([]byte(*ra.channel.ParamOverride), &override); err != nil {
				log.Warnf("failed to unmarshal param_override: %v, skipping", err)
			} else {
				maps.Copy(bodyMap, override)
				modifiedBody, err := json.Marshal(bodyMap)
				if err != nil {
					log.Warnf("failed to marshal modified body: %v, skipping param_override", err)
				} else {
					outboundRequest.Body = modifiedBody
					ra.metrics.ParamOverride = *ra.channel.ParamOverride
				}
			}
		}
	}
	for _, header := range ra.channel.CustomHeader {
		// pipeline 在 raw request middleware 前已经写入 Auth；同名敏感头保持认证配置优先，延续旧 BuildHttpRequest 的覆盖顺序。
		if outboundRequest.Headers.Get(header.HeaderKey) != "" && httpclient.IsSensitiveHeader(header.HeaderKey) {
			continue
		}
		outboundRequest.Headers.Set(header.HeaderKey, header.HeaderValue)
	}
}

// writeStream 把 pipeline 输出的客户端格式流写回请求方，并保留首 token 超时切换通道的行为。
func (ra *relayAttempt) writeStream(ctx context.Context, clientStream streams.Stream[*httpclient.StreamEvent]) error {
	if clientStream == nil {
		return fmt.Errorf("empty pipeline stream")
	}

	// 设置 SSE 响应头
	ra.c.Header("Content-Type", "text/event-stream")
	ra.c.Header("Cache-Control", "no-cache")
	ra.c.Header("Connection", "keep-alive")
	ra.c.Header("X-Accel-Buffering", "no")

	firstToken := true
	// responseEvents 仅用于日志聚合，若不设上限，恶意或超长流会让请求进程
	// 为审计日志线性累积内存。透传不受影响，超限后不再收集分片即可。
	const (
		maxStreamLogBytes    = 1 << 20  // 1 MiB，防御超长内容流
		maxStreamLogEvents   = 1 << 14  // 16k 条，防御海量小分片事件
		maxStreamUsageEvents = 8        // 截断后仍保留的 usage 事件条数上限
		maxStreamUsageBytes  = 64 << 10 // 64 KiB，截断后保留的 usage 事件字节上限
	)
	responseEvents := make([]*httpclient.StreamEvent, 0, 8)
	responseLogBytes := 0
	usageEvents := 0
	usageLogBytes := 0
	type sseReadResult struct {
		event *httpclient.StreamEvent
		err   error
	}
	results := make(chan sseReadResult, 1)
	done := make(chan struct{})

	// 兜底聚合日志的最终响应体。流结束后 results channel 关闭（!ok）和客户端断开
	// 触发的 ctx.Done() 几乎同时就绪，select 可能随机选中 ctx.Done 分支直接返回，
	// 从而跳过下面的聚合逻辑，导致流式日志的 ResponseContent 长期为空。用 defer
	// 保证无论从哪条路径退出，只要已经向客户端写出过事件，就聚合一次给日志落库。
	defer func() {
		if len(responseEvents) == 0 {
			return
		}
		responseBody, meta, aggErr := ra.inAdapter.AggregateStreamChunks(context.WithoutCancel(ctx), responseEvents)
		if aggErr != nil {
			log.Warnf("failed to aggregate stream response for log: %v", aggErr)
			return
		}
		ra.metrics.InternalResponse = responseBody
		ra.metrics.RecordUsage(meta.Usage)
	}()
	// LIFO：该 defer 后注册，返回时先执行，尽早关闭 done 让后端读取 goroutine 退出，
	// 随后上方的聚合 defer 再执行，避免聚合期间阻塞读取协程。
	defer close(done)

	go func() {
		defer close(results)
		defer clientStream.Close()
		defer func() {
			if r := recover(); r != nil {
				log.Warnf("stream reader panic: %v", r)
				select {
				case results <- sseReadResult{err: fmt.Errorf("stream reader panic: %v", r)}:
				case <-done:
				case <-ctx.Done():
				}
			}
		}()
		// Next 可能阻塞等待上游 token；放到协程里让首 token 超时和客户端断开都能及时打断本次通道尝试。
		for clientStream.Next() {
			select {
			case results <- sseReadResult{event: clientStream.Current()}:
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
		if err := clientStream.Err(); err != nil {
			select {
			case results <- sseReadResult{err: err}:
			case <-done:
			case <-ctx.Done():
			}
		}
	}()

	firstTokenTimeoutSec := ra.group.FirstTokenTimeOut
	var firstTokenTimer *time.Timer
	var firstTokenC <-chan time.Time
	if firstTokenTimeoutSec > 0 {
		firstTokenTimer = time.NewTimer(time.Duration(firstTokenTimeoutSec) * time.Second)
		firstTokenC = firstTokenTimer.C
		defer func() {
			if firstTokenTimer != nil {
				firstTokenTimer.Stop()
			}
		}()
	}

	for {
		select {
		case <-ctx.Done():
			log.Infof("client disconnected, stopping stream")
			_ = clientStream.Close()
			return nil
		case <-firstTokenC:
			log.Warnf("first token timeout (%ds), switching channel", firstTokenTimeoutSec)
			_ = clientStream.Close()
			return fmt.Errorf("first token timeout (%ds)", firstTokenTimeoutSec)
		case r, ok := <-results:
			if !ok {
				log.Infof("stream end")
				// 聚合逻辑统一由上面的 defer 兜底执行，保证正常结束、客户端断开、
				// 读事件错误等所有退出路径都不会遗漏日志响应体。
				return nil
			}
			if r.err != nil {
				log.Warnf("failed to read event: %v", r.err)
				return fmt.Errorf("failed to read stream event: %w", r.err)
			}

			if r.event == nil || len(r.event.Data) == 0 {
				continue
			}
			// 这里只临时保存 pipeline 已经转换好的客户端格式事件，正常结束后聚合成最终响应体用于日志；不会把分片逐条落库。
			// 达到 maxStreamLogBytes / maxStreamLogEvents 后停止收集，避免日志聚合缓冲被超长/恶意流撑爆，透传给客户端不受影响。
			// 超限后仍单独保留 usage 事件，保证超长请求的用量/费用统计不因日志截断而漏记；
			// usage 事件按条数与字节双重限额（真实上游通常仅 1-2 条且很小），避免恶意巨型 usage 事件绕过预算。
			if responseLogBytes < maxStreamLogBytes && len(responseEvents) < maxStreamLogEvents {
				responseEvents = append(responseEvents, r.event)
				responseLogBytes += len(r.event.Data)
			} else if usageEvents < maxStreamUsageEvents && usageLogBytes < maxStreamUsageBytes && bytes.Contains(r.event.Data, []byte(`"usage"`)) {
				responseEvents = append(responseEvents, r.event)
				usageEvents++
				usageLogBytes += len(r.event.Data)
			}
			if firstToken {
				ra.metrics.FirstTokenTime = time.Now()
				firstToken = false
				if firstTokenTimer != nil {
					if !firstTokenTimer.Stop() {
						select {
						case <-firstTokenTimer.C:
						default:
						}
					}
					firstTokenTimer = nil
					firstTokenC = nil
				}
			}

			ra.c.SSEvent(r.event.Type, r.event.Data)
			ra.c.Writer.Flush()
		}
	}
}

// relayPipelineMiddleware 承接 octopus 自己的通道级副作用：
// 1. 在 pipeline 发出上游请求前应用渠道参数覆盖和自定义 header；
// 2. 在上游失败时保存 HTTP 状态码，供 key 冷却、熔断和后续选路使用；
// 3. 在非流式响应转成 llm.Response 后记录 usage。
// axonhub/llm 只提供了部分函数式 middleware 构造器，错误状态码和 llm 响应 usage 这两个回调没有公开构造器，
// 所以这里保留一个很薄的结构体实现完整接口，而不是在 relay 主流程里重复 pipeline 的执行逻辑。
type relayPipelineMiddleware struct {
	pipeline.DummyMiddleware
	attempt            *relayAttempt
	upstreamStatusCode int
}

func (m *relayPipelineMiddleware) Name() string {
	return "octopus_relay"
}

func (m *relayPipelineMiddleware) OnOutboundRawRequest(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
	if request.Headers == nil {
		request.Headers = make(http.Header)
	}
	m.attempt.applyChannelRequestOptions(request)
	return request, nil
}

func (m *relayPipelineMiddleware) OnOutboundRawError(ctx context.Context, err error) {
	var upstreamErr *httpclient.Error
	if errors.As(err, &upstreamErr) {
		// pipeline 会把上游错误转换成统一错误返回；这里在转换前记录原始 HTTP 状态码，用于渠道 key 的后续调度决策。
		m.upstreamStatusCode = upstreamErr.StatusCode
	}
}

func (m *relayPipelineMiddleware) OnOutboundLlmResponse(ctx context.Context, response *llm.Response) (*llm.Response, error) {
	if response != nil {
		// 非流式 usage 已由 outbound transformer 标准化到 llm.Response；流式 usage 在最终聚合时记录，避免重复计数。
		m.attempt.metrics.RecordUsage(response.Usage)
	}
	return response, nil
}

// parsedRequestInbound 让 pipeline 复用 relay 在选路前已经解析好的 llm.Request。
// 这样每次候选通道尝试只重新执行 outbound transform 和 HTTP 请求，不会重复读取或解析客户端 body。
type parsedRequestInbound struct {
	transformer.Inbound
	request *llm.Request
}

func (in *parsedRequestInbound) TransformRequest(ctx context.Context, request *httpclient.Request) (*llm.Request, error) {
	if in.request == nil {
		return nil, fmt.Errorf("missing parsed request")
	}
	// relay 已经为选路解析过请求；pipeline 入口复用该结果，避免每次通道尝试再次解析同一份 body。
	in.request.RawRequest = request
	return in.request, nil
}

// forwardMiMoCode 直接调用 api.xiaomimimo.com 的 OpenAI 兼容接口。
// 不经过 llm.Message 序列化，直接透传客户端原始 JSON body，只修改必要字段。
// HTTP 客户端与其它渠道一致（无固定总超时）；首 token 超时跟随分组 first_token_time_out。
func (ra *relayAttempt) forwardMiMoCode() (int, error) {
	ctx := ra.c.Request.Context()
	httpClient, err := helper.ChannelHttpClient(ra.channel)
	if err != nil {
		return 0, fmt.Errorf("mimocode http client: %w", err)
	}
	jwtMgr := newMiMoJWTManager(ra.channel.GetBaseUrl(), httpClient)
	sessionAffinity := fmt.Sprintf("ses_%x", time.Now().UnixMilli())

	// 读取客户端原始请求 body，避免 llm.Message 序列化引入非标准字段
	rawBody := ra.internalRequest.RawRequest.Body

	var bodyMap map[string]any
	if err := json.Unmarshal(rawBody, &bodyMap); err != nil {
		return 0, fmt.Errorf("unmarshal request body: %w", err)
	}

	// 强制流式 + usage
	bodyMap["stream"] = true
	bodyMap["stream_options"] = map[string]any{"include_usage": true}
	// 用 relay 选路后的实际模型名覆盖客户端传的名称
	bodyMap["model"] = ra.internalRequest.Model

	// 删除上游不理解的字段
	for _, key := range []string{"response_format"} {
		delete(bodyMap, key)
	}

	// 注入 magic prefix 到 system message
	messages, _ := bodyMap["messages"].([]any)
	bodyMap["messages"] = injectMiMoMagicPrefixRaw(messages)

	resp, err := miMoChatRaw(ctx, jwtMgr, ra.channel.GetBaseUrl(), bodyMap, sessionAffinity)
	if err != nil {
		return 0, fmt.Errorf("mimocode chat: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("mimocode upstream %d: %s", resp.StatusCode, body)
	}

	isStream := ra.internalRequest.Stream != nil && *ra.internalRequest.Stream

	if isStream {
		ra.c.Header("Content-Type", "text/event-stream")
		ra.c.Header("Cache-Control", "no-cache")
		ra.c.Header("Connection", "keep-alive")
		ra.c.Header("X-Accel-Buffering", "no")

		// 流式同时写客户端和缓冲原始 SSE，流结束后聚合用于日志
		var sseBuf bytes.Buffer
		tee := io.TeeReader(resp.Body, &sseBuf)
		if err := ra.miMoCopyUpstream(ctx, tee, true); err != nil {
			return http.StatusOK, err
		}
		chunks := miMoAggregateSSE(&sseBuf)
		if len(chunks) > 0 {
			aggregated := miMoAggregateChunks(chunks, ra.internalRequest.Model)
			ra.metrics.InternalResponse, _ = json.Marshal(aggregated)
		}
		return http.StatusOK, nil
	}

	// 非流式：上游仍是 SSE，先按首 token 超时读完再聚合
	var sseBuf bytes.Buffer
	if err := ra.miMoCopyUpstream(ctx, io.TeeReader(resp.Body, &sseBuf), false); err != nil {
		return 0, err
	}
	chunks := miMoAggregateSSE(&sseBuf)
	if len(chunks) == 0 {
		return http.StatusBadGateway, fmt.Errorf("mimocode: empty upstream response")
	}
	aggregated := miMoAggregateChunks(chunks, ra.internalRequest.Model)
	ra.c.Header("Content-Type", "application/json")
	ra.c.JSON(http.StatusOK, aggregated)
	ra.metrics.InternalResponse, _ = json.Marshal(aggregated)
	return http.StatusOK, nil
}

// miMoCopyUpstream 从上游读 SSE。toClient=true 时边读边写客户端；false 时只消费 body（非流式聚合用）。
// 首 token 超时与 writeStream 相同，取分组 first_token_time_out（秒，0 关闭）。
func (ra *relayAttempt) miMoCopyUpstream(ctx context.Context, src io.Reader, toClient bool) error {
	firstTokenTimeoutSec := ra.group.FirstTokenTimeOut
	var firstTokenTimer *time.Timer
	var firstTokenC <-chan time.Time
	if firstTokenTimeoutSec > 0 {
		firstTokenTimer = time.NewTimer(time.Duration(firstTokenTimeoutSec) * time.Second)
		firstTokenC = firstTokenTimer.C
		defer func() {
			if firstTokenTimer != nil {
				firstTokenTimer.Stop()
			}
		}()
	}

	type readResult struct {
		n   int
		err error
	}
	buf := make([]byte, 32*1024)
	firstToken := true

	for {
		ch := make(chan readResult, 1)
		go func() {
			n, err := src.Read(buf)
			ch <- readResult{n: n, err: err}
		}()

		var r readResult
		if firstToken && firstTokenC != nil {
			select {
			case <-ctx.Done():
				log.Infof("client disconnected, stopping mimocode stream")
				return nil
			case <-firstTokenC:
				log.Warnf("first token timeout (%ds), switching channel", firstTokenTimeoutSec)
				return fmt.Errorf("first token timeout (%ds)", firstTokenTimeoutSec)
			case r = <-ch:
			}
		} else {
			select {
			case <-ctx.Done():
				log.Infof("client disconnected, stopping mimocode stream")
				return nil
			case r = <-ch:
			}
		}

		if r.n > 0 {
			if firstToken {
				ra.metrics.FirstTokenTime = time.Now()
				firstToken = false
				if firstTokenTimer != nil {
					if !firstTokenTimer.Stop() {
						select {
						case <-firstTokenTimer.C:
						default:
						}
					}
					firstTokenTimer = nil
					firstTokenC = nil
				}
			}
			if toClient {
				if _, werr := ra.c.Writer.Write(buf[:r.n]); werr != nil {
					return werr
				}
				ra.c.Writer.Flush()
			}
		}
		if r.err != nil {
			if r.err == io.EOF {
				return nil
			}
			// 客户端断开时 Read 常伴随 cancel 错误，与 writeStream 一致不当作渠道失败
			if ctx.Err() != nil {
				return nil
			}
			return r.err
		}
	}
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
