import { useMemo } from 'react';
import { useTranslations } from 'use-intl';
import { Loader2, Wifi, WifiOff, Clock, ArrowRight, CheckCircle2, XCircle } from 'lucide-react';
import { useMonitor } from '@/api/endpoints/monitor';
import { MonitorCard } from './Card';
import { VirtualizedGrid } from '@/components/common/VirtualizedGrid';
import { getModelIcon } from '@/lib/model-icons';
import { Badge } from '@/components/ui/badge';
import type { RelayLog } from '@/api/endpoints/log';

function formatTime(ts: number): string {
    if (!ts) return '-';
    return new Date(ts * 1000).toLocaleString('zh-CN', {
        month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit',
    });
}

function formatDuration(ms: number): string {
    if (ms < 1000) return `${ms}ms`;
    return `${(ms / 1000).toFixed(2)}s`;
}

/**
 * 顶部"最近一条日志"摘要卡片
 */
function LatestLogCard({ log }: { log: RelayLog }) {
    const t = useTranslations('monitor');
    const { Avatar: ModelAvatar, color: brandColor } = useMemo(() => getModelIcon(log.actual_model_name), [log.actual_model_name]);
    const hasError = !!log.error;

    return (
        <div className={`rounded-3xl border bg-card px-4 py-3 flex items-center gap-3 shrink-0 ${hasError ? 'border-destructive/40' : 'border-border'}`}>
            <ModelAvatar size={30} />
            <div className="min-w-0 flex-1 flex items-center gap-2 text-xs">
                <span className="font-semibold text-card-foreground truncate">{log.request_model_name}</span>
                <ArrowRight className="size-3 shrink-0 text-muted-foreground/50" />
                <Badge variant="secondary" className="shrink-0 text-[10px] px-1.5 py-0" style={{ backgroundColor: `${brandColor}15`, color: brandColor }}>
                    {log.channel_name}
                </Badge>
                <span className="text-muted-foreground truncate">{log.actual_model_name}</span>
            </div>
            <div className="hidden md:flex items-center gap-3 text-xs text-muted-foreground shrink-0">
                <span className="flex items-center gap-1">
                    <Clock className="size-3" style={{ color: brandColor }} />
                    {formatTime(log.time)}
                </span>
                <span>{t('ftut')} {formatDuration(log.ftut)}</span>
                <span>{t('use_time_short')} {formatDuration(log.use_time)}</span>
            </div>
            <span className={`flex items-center gap-1 text-xs shrink-0 ${hasError ? 'text-destructive' : 'text-emerald-600 dark:text-emerald-400'}`}>
                {hasError ? <XCircle className="size-3.5" /> : <CheckCircle2 className="size-3.5" />}
                {hasError ? t('status.error') : t('status.ok')}
            </span>
        </div>
    );
}

/**
 * 模型可用性监控页面
 * - SSE 实时刷新（op 层在每次渠道尝试后推送新快照，且每秒刷新冷却剩余周期）
 * - 顶部卡片：最近一条日志摘要
 * - 列表：每个 (channel, model) 一张卡片，含可用性竖条与底部指标
 */
export function Monitor() {
    const t = useTranslations('monitor');
    const { rows, latestLog, isConnected, error } = useMonitor();

    const content = useMemo(() => {
        if (error) {
            return (
                <div className="flex items-center justify-center h-40 gap-2 text-muted-foreground">
                    <WifiOff className="size-4" />
                    <span>{t('disconnected')}</span>
                </div>
            );
        }
        if (rows.length === 0) {
            return (
                <div className="flex items-center justify-center h-40 gap-2 text-muted-foreground">
                    <Loader2 className="size-4 animate-spin" />
                    <span>{t('noData')}</span>
                </div>
            );
        }
        return (
            <VirtualizedGrid
                items={rows}
                layout="list"
                columns={{ default: 1 }}
                estimateItemHeight={64}
                overscan={8}
                getItemKey={(row) => `monitor-${row.channel_id}-${row.model_name}`}
                renderItem={(row) => <MonitorCard row={row} t={t} />}
            />
        );
    }, [rows, error, t]);

    return (
        <div className="flex flex-col h-full min-h-0 gap-3 p-1">
            <div className="flex items-center justify-between px-2">
                <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
                    {isConnected ? (
                        <Wifi className="size-3.5 text-green-500" />
                    ) : (
                        <WifiOff className="size-3.5 text-muted-foreground" />
                    )}
                    <span>{isConnected ? t('connected') : t('connecting')}</span>
                </span>
                <span className="text-xs text-muted-foreground">{t('windowSize', { n: 30 })}</span>
            </div>
            {latestLog && <LatestLogCard log={latestLog} />}
            <div className="flex-1 min-h-0">{content}</div>
        </div>
    );
}