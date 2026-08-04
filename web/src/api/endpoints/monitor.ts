import { useCallback, useEffect, useRef, useState } from 'react';
import { apiClient, API_BASE_URL } from '../client';
import { logger } from '@/lib/logger';
import type { RelayLog } from './log';

/**
 * 单次渠道尝试的可用性竖条数据
 */
export interface MonitorCall {
    time: number;           // 调用时间(秒)
    status: 'ok' | '429' | 'error' | 'cancel';
    ftut: number;           // 首字时间(ms)，仅成功
    use_time: number;       // 本次尝试耗时(ms)
    input: number;
    output: number;
    cost: number;
}

/**
 * 一个 (channel, model) 可用性监控行
 */
export interface MonitorRow {
    channel_id: number;
    channel_name: string;
    model_name: string;

    time: number;           // 最后一次调用时间(秒)
    captured_at: number;    // 最后一次调用 capture 时间(秒)，用于频率排序
    count: number;          // 统计周期内所有尝试次数
    last_source_name: string; // 统计周期内最后一次调用的来源（API Key 名称）

    input_total: number;
    output_total: number;
    cost_total: number;
    success_count: number;
    success_use_time_sum: number;
    success_ftut_sum: number;
    success_input: number;
    success_output: number;
    success_cost: number;

    calls: MonitorCall[];

    yellow_cooldown: number; // key 429 冷却剩余(秒)
    red_cooldown: number;    // 熔断冷静剩余(秒)
}

// 监控页面完整实时载荷（SSE 每次推送一整份）
export interface MonitorPayload {
    rows: MonitorRow[];
    latest_log: RelayLog | null;
}

/**
 * 模型可用性监控 Hook
 * - 初始加载当前快照
 * - SSE 实时推送快照变更（含冷静剩余周期周期性刷新）
 *
 * @example
 * const { rows, isConnected, error } = useMonitor();
 */
export function useMonitor() {
    const [rows, setRows] = useState<MonitorRow[]>([]);
    const [latestLog, setLatestLog] = useState<RelayLog | null>(null);
    const [isConnected, setIsConnected] = useState(false);
    const [error, setError] = useState<Error | null>(null);
    const eventSourceRef = useRef<EventSource | null>(null);
    const retryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
    const connectRef = useRef<() => void>(() => {});

    const connect = useCallback(() => {
        const cancelled = false;

        const open = async () => {
            try {
                const { token } = await apiClient.get<{ token: string }>('/api/v1/monitor/stream-token');
                if (cancelled) return;

                const eventSource = new EventSource(`${API_BASE_URL}/api/v1/monitor/stream?token=${token}`);
                eventSourceRef.current = eventSource;

                eventSource.onopen = () => {
                    setIsConnected(true);
                    setError(null);
                };

                eventSource.onmessage = (event) => {
                    try {
                        const payload = JSON.parse(event.data) as MonitorPayload;
                        setRows(payload.rows);
                        setLatestLog(payload.latest_log);
                    } catch (e) {
                        logger.error('解析监控数据失败:', e);
                    }
                };

                eventSource.onerror = () => {
                    setIsConnected(false);
                    setError(new Error('SSE 连接断开'));
                    eventSource.close();
                    eventSourceRef.current = null;
                    // 自动重连
                    retryTimerRef.current = setTimeout(() => {
                        connectRef.current();
                    }, 3000);
                };
            } catch (e) {
                if (cancelled) return;
                setError(e instanceof Error ? e : new Error('获取 stream token 失败'));
                logger.error('获取 stream token 失败:', e);
                retryTimerRef.current = setTimeout(() => {
                    connectRef.current();
                }, 3000);
            }
        };

        void open();
    }, []);

    connectRef.current = connect;

    useEffect(() => {
        connect();
        return () => {
            eventSourceRef.current?.close();
            eventSourceRef.current = null;
            if (retryTimerRef.current) clearTimeout(retryTimerRef.current);
            setIsConnected(false);
        };
    }, [connect]);

    return { rows, latestLog, isConnected, error };
}