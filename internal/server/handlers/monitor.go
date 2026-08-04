package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/monitor").
		Use(middleware.Auth()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(monitorList),
		).
		AddRoute(
			router.NewRoute("/stream-token", http.MethodGet).
				Handle(monitorStreamToken),
		)

	router.NewGroupRouter("/api/v1/monitor").
		AddRoute(
			router.NewRoute("/stream", http.MethodGet).
				Handle(monitorStream),
		)
}

// monitorRowResp 返回给前端的一行可用性监控数据，
// 在 op.MonitorRow 基础上补充"是否处于冷静"的标注。
type monitorRowResp struct {
	op.MonitorRow
	YellowCooldown int64 `json:"yellow_cooldown"` // 剩余冷静周期(秒)：key 被 429 冷却
	RedCooldown    int64 `json:"red_cooldown"`    // 剩余冷静周期(秒)：熔断 Open
}

// monitorPayload 监控页面的完整实时载荷：可用性行 + 最近一条日志。
type monitorPayload struct {
	Rows      []monitorRowResp `json:"rows"`
	LatestLog *model.RelayLog  `json:"latest_log"`
}

func monitorList(c *gin.Context) {
	resp.Success(c, buildMonitorPayload(c.Request.Context()))
}

func monitorStreamToken(c *gin.Context) {
	token, err := op.MonitorStreamTokenCreate()
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, gin.H{"token": token})
}

func monitorStream(c *gin.Context) {
	token := c.Query("token")
	if token == "" || !op.MonitorStreamTokenVerify(token) {
		resp.Error(c, http.StatusUnauthorized, "invalid stream token")
		return
	}
	op.MonitorStreamTokenRevoke(token)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flush := func() {
		data, err := json.Marshal(buildMonitorPayload(c.Request.Context()))
		if err != nil {
			return
		}
		c.Writer.Write([]byte(fmt.Sprintf("data: %s\n\n", data)))
		c.Writer.Flush()
	}
	flush()

	ch := op.MonitorSubscribe()
	defer op.MonitorUnsubscribe(ch)

	ctx := c.Request.Context()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			flush()
		case <-ticker.C:
			// 冷却时间随时间流逝，周期性刷新剩余周期
			flush()
		}
	}
}

// buildMonitorPayload 组装当前可见监控数据。
func buildMonitorPayload(ctx context.Context) monitorPayload {
	channels, _ := op.ChannelList(ctx)
	channelMap := make(map[int]*model.Channel, len(channels))
	for i := range channels {
		channelMap[channels[i].ID] = &channels[i]
	}
	groups, _ := op.GroupList(ctx)
	channelInUse := make(map[int]struct{})
	for _, g := range groups {
		for _, item := range g.Items {
			channelInUse[item.ChannelID] = struct{}{}
		}
	}

	rows := op.MonitorSnapshot()
	now := time.Now().Unix()
	out := make([]monitorRowResp, 0, len(rows))
	for _, row := range rows {
		ch, ok := channelMap[row.ChannelID]
		if !ok || !ch.Enabled {
			continue
		}
		if _, used := channelInUse[row.ChannelID]; !used {
			continue
		}
		row.ChannelName = ch.Name
		r := monitorRowResp{MonitorRow: row}

		var yellow int64
		for _, key := range ch.Keys {
			if key.StatusCode == http.StatusTooManyRequests && key.LastUseTimeStamp > 0 {
				remain := int64(5*time.Minute/time.Second) - (now - key.LastUseTimeStamp)
				if remain > yellow {
					yellow = remain
				}
			}
		}
		r.YellowCooldown = yellow

		var red int64
		for _, key := range ch.Keys {
			tripped, remaining := balancer.IsTripped(ch.ID, key.ID, row.ModelName)
			if tripped && remaining > 0 {
				sec := int64(remaining.Seconds())
				if sec > red {
					red = sec
				}
			}
		}
		r.RedCooldown = red
		out = append(out, r)
	}

	// 按使用频率由高到低排序：最后一次调用时间倒序为主，其次按调用次数
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CapturedAt != out[j].CapturedAt {
			return out[i].CapturedAt > out[j].CapturedAt
		}
		return out[i].Count > out[j].Count
	})

	// 最近一条日志（供顶部卡片展示）——裁剪掉完整请求/响应内容，避免在 API 响应中
	// 泄露用户 PII / 敏感业务数据（前端只消费元信息字段）。
	var latest *model.RelayLog
	if logs, err := op.RelayLogList(ctx, nil, nil, 1, 1); err == nil && len(logs) > 0 {
		l := logs[0]
		l.RequestContent = ""
		l.ResponseContent = ""
		l.Attempts = nil
		l.Error = ""
		latest = &l
	}

	return monitorPayload{Rows: out, LatestLog: latest}
}