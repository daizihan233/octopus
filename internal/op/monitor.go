package op

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// MonitorCall 统计周期内该 (channel, model) 的一次渠道尝试。
// Status: ok=成功响应, 429=上游429无可用key, error=其他转发错误(超时等), cancel=上下文取消。
type MonitorCall struct {
	Seq     int64   `json:"seq"`    // 全局自增序号，用于前端稳定 key（滚动窗口不错位）
	Time    int64   `json:"time"`
	Status  string  `json:"status"`
	Ftut    int64   `json:"ftut"`     // 首字时间(ms)，仅成功时有效
	UseTime int64   `json:"use_time"` // 本次尝试耗时(ms)
	Input   int64   `json:"input"`
	Output  int64   `json:"output"`
	Cost    float64 `json:"cost"`
}

// MonitorRow 一个 (channel, model) 可用性监控行。Calls 保存最近 windowSize 条尝试，用于对齐
// 底部竖条；聚合指标统计本统计周期内所有记录（含被截断的更早调用）。
type MonitorRow struct {
	ChannelID   int    `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	ModelName   string `json:"model_name"`

	Time       int64 `json:"time"`        // 最后一次调用时间(秒)
	CapturedAt int64 `json:"captured_at"` // 最后一次调用的 capture 时间(秒)，用于频率排序
	Count      int64 `json:"count"`       // 统计周期内所有尝试次数

	// 统计周期内的来源（最后一次调用的 API Key 名称）
	LastSourceName string `json:"last_source_name"`

	// 累加指标（"总"= 所有尝试；"平均"= 仅成功响应）
	InputTotal        int64   `json:"input_total"`
	OutputTotal       int64   `json:"output_total"`
	CostTotal         float64 `json:"cost_total"`
	SuccessCount      int64   `json:"success_count"`
	SuccessUseTimeSum int64   `json:"success_use_time_sum"`
	SuccessFtutSum    int64   `json:"success_ftut_sum"`
	SuccessInput      int64   `json:"success_input"`
	SuccessOutput     int64   `json:"success_output"`
	SuccessCost       float64 `json:"success_cost"`

	Calls []MonitorCall `json:"calls"`
}

const monitorWindowSize = 30 // 每个 (channel, model) 保留的最近尝试条数

var monitorCacheLock sync.Mutex
var monitorCache = make(map[string]*MonitorRow)

// monitorCallSeq 全局自增序号，保证每条 MonitorCall 有唯一 seq 供前端做稳定 key。
var monitorCallSeq atomic.Int64

var monitorSubscribers = make(map[chan struct{}]struct{})
var monitorSubscribersLock sync.RWMutex

func monitorKey(channelID int, modelName string) string {
	return fmt.Sprintf("%d\x00%s", channelID, modelName)
}

func MonitorSubscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	monitorSubscribersLock.Lock()
	monitorSubscribers[ch] = struct{}{}
	monitorSubscribersLock.Unlock()
	return ch
}

func MonitorUnsubscribe(ch chan struct{}) {
	monitorSubscribersLock.Lock()
	delete(monitorSubscribers, ch)
	monitorSubscribersLock.Unlock()
	close(ch)
}

func monitorNotify() {
	monitorSubscribersLock.RLock()
	defer monitorSubscribersLock.RUnlock()
	for ch := range monitorSubscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// MonitorCallAdd 记录一次渠道尝试到监控缓存，并同步更新渠道名（渠道改名后旧名回填）。
// sourceName 为本次调用的来源（API Key 名称）。
func MonitorCallAdd(channelID int, channelName, modelName string, call MonitorCall, sourceName string) {
	if channelID <= 0 || modelName == "" {
		return
	}
	monitorCacheLock.Lock()
	key := monitorKey(channelID, modelName)
	row, ok := monitorCache[key]
	if !ok {
		row = &MonitorRow{
			ChannelID:   channelID,
			ChannelName: channelName,
			ModelName:   modelName,
		}
		monitorCache[key] = row
	}
	row.ChannelName = channelName // 渠道改名时回填最新名

	row.Count++
	call.Seq = monitorCallSeq.Add(1) // 分配全局唯一序号，供前端稳定 key
	row.InputTotal += call.Input
	row.OutputTotal += call.Output
	row.CostTotal += call.Cost

	if call.Status == "ok" {
		row.SuccessCount++
		row.SuccessUseTimeSum += call.UseTime
		row.SuccessFtutSum += call.Ftut
		row.SuccessInput += call.Input
		row.SuccessOutput += call.Output
		row.SuccessCost += call.Cost
	}

	if call.Time > row.Time {
		row.Time = call.Time
	}
	row.CapturedAt = call.Time
	row.LastSourceName = sourceName

	row.Calls = append(row.Calls, call)
	if len(row.Calls) > monitorWindowSize {
		row.Calls = row.Calls[len(row.Calls)-monitorWindowSize:]
	}
	monitorCacheLock.Unlock()

	monitorNotify()
}


// MonitorSnapshot 返回当前监控行的只读快照。Calls slice 深拷贝，避免读路径与
// MonitorCallAdd 的 append 写路径共享底层数组导致并发竞争。
func MonitorSnapshot() []MonitorRow {
	monitorCacheLock.Lock()
	defer monitorCacheLock.Unlock()
	out := make([]MonitorRow, 0, len(monitorCache))
	for _, row := range monitorCache {
		cp := *row
		if len(row.Calls) > 0 {
			cp.Calls = make([]MonitorCall, len(row.Calls))
			copy(cp.Calls, row.Calls)
		}
		out = append(out, cp)
	}
	return out
}

// MonitorClear 清空监控缓存（可选的重置入口）。
func MonitorClear() {
	monitorCacheLock.Lock()
	monitorCache = make(map[string]*MonitorRow)
	monitorCacheLock.Unlock()
	monitorNotify()
}
