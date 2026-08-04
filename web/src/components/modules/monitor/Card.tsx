import { useMemo, type ReactNode } from 'react';
import { Clock, Zap, Cpu, ArrowDownToLine, ArrowUpFromLine, DollarSign, Timer, Loader2 } from 'lucide-react';
import { getModelIcon } from '@/lib/model-icons';
import { type MonitorRow, type MonitorCall } from '@/api/endpoints/monitor';
import { Tooltip, TooltipContent, TooltipTrigger, TooltipProvider } from '@/components/animate-ui/components/animate/tooltip';

const STATUS_META: Record<MonitorCall['status'], { color: string; labelKey: string }> = {
    ok: { color: '#22c55e', labelKey: 'status.ok' },
    '429': { color: '#eab308', labelKey: 'status.rateLimited' },
    error: { color: '#ef4444', labelKey: 'status.error' },
    cancel: { color: '#9ca3af', labelKey: 'status.canceled' },
};

function formatDuration(ms: number): string {
    if (ms < 1000) return `${ms}ms`;
    return `${(ms / 1000).toFixed(2)}s`;
}

function formatTime(ts: number): string {
    if (!ts) return '-';
    return new Date(ts * 1000).toLocaleString('zh-CN', {
        month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit',
    });
}

function formatToken(n: number): string {
    return (n ?? 0).toLocaleString();
}

/**
 * 单个 (channel, model) 可用性监控卡片
 * - 右侧竖条区：最近若干次尝试，绿色=成功，黄色=429，红色=错误，灰色=cancel
 *   （黄/红/灰恒为 100% 高；绿色高度按统计周期内最慢一次成功查询缩放，最低可见一个"点"）
 * - 底部一排指标：最近调用时间、来源、平均首字、平均响应、总输入、总输出、总费用
 */
export function MonitorCard({ row, t }: { row: MonitorRow; t: (key: string) => string }) {
    const { Avatar: ModelAvatar, color: brandColor } = useMemo(
        () => getModelIcon(row.model_name),
        [row.model_name]
    );

    const calls = useMemo(() => row.calls ?? [], [row.calls]);

    // 统计周期内最慢一次成功的总耗时或首字时间，用于绿色竖条高度基准
    const maxSuccessMs = useMemo(() => {
        let max = 0;
        for (const c of calls) {
            if (c.status !== 'ok') continue;
            const v = c.use_time > 0 ? c.use_time : c.ftut;
            if (v > max) max = v;
        }
        return max;
    }, [calls]);

    const avgUseTime = row.success_count > 0 ? row.success_use_time_sum / row.success_count : 0;
    const avgFtut = row.success_count > 0 ? row.success_ftut_sum / row.success_count : 0;

    const yellowLeft = Math.max(0, row.yellow_cooldown);
    const redLeft = Math.max(0, row.red_cooldown);

    const isCooling = yellowLeft > 0 || redLeft > 0;
    const borderClass = yellowLeft > 0
        ? 'border-yellow-500/70'
        : redLeft > 0
            ? 'border-red-500/80'
            : 'border-border';

    return (
        <TooltipProvider>
            <div className={`relative rounded-3xl border ${borderClass} bg-card p-4 flex flex-col gap-3 transition-colors`}>
                {/* 顶部：模型 logo + 渠道 + 模型名 */}
                <div className="flex items-center gap-3 min-w-0">
                    <ModelAvatar size={34} />
                    <div className="min-w-0 flex-1">
                        <div className="flex items-center gap-2 min-w-0">
                            <span className="font-semibold text-card-foreground truncate" style={{ color: brandColor }}>
                                {row.model_name}
                            </span>
                            {isCooling && (
                                <span className="shrink-0 flex items-center gap-1 text-[10px] font-medium px-1.5 py-0.5 rounded-md bg-amber-500/15 text-amber-600 dark:text-amber-400">
                                    {yellowLeft > 0 ? t('cooldownYellow') : t('cooldownRed')} {Math.max(yellowLeft, redLeft)}s
                                </span>
                            )}
                        </div>
                        <div className="text-xs text-muted-foreground truncate">{row.channel_name}</div>
                    </div>
                </div>

                {/* 竖条区 */}
                <div className="flex items-end h-24 gap-[2px] overflow-hidden">
                    {calls.length === 0 && (
                        <div className="flex items-center text-xs text-muted-foreground gap-1.5">
                            <Loader2 className="size-3 animate-spin" />
                            <span>{t('waiting')}</span>
                        </div>
                    )}
                    {calls.map((c, idx) => {
                        const meta = STATUS_META[c.status];
                        let heightPct = 100;
                        if (c.status === 'ok') {
                            const ms = c.use_time > 0 ? c.use_time : c.ftut;
                            heightPct = maxSuccessMs > 0 ? Math.max(6, Math.round((ms / maxSuccessMs) * 100)) : 6;
                        }
                        return (
                            <Tooltip key={idx}>
                                <TooltipTrigger asChild>
                                    <div
                                        className="flex-1 min-w-[3px] rounded-sm"
                                        style={{ height: `${heightPct}%`, backgroundColor: meta.color }}
                                    />
                                </TooltipTrigger>
                                <TooltipContent className="border bg-card p-2 rounded-xl text-xs flex flex-col gap-0.5">
                                    <span>{formatTime(c.time)}</span>
                                    <span>{t(meta.labelKey)}</span>
                                    <span>{t('use_time')} {formatDuration(c.use_time)}</span>
                                    {c.status === 'ok' && <span>{t('ftut')} {formatDuration(c.ftut)}</span>}
                                </TooltipContent>
                            </Tooltip>
                        );
                    })}
                </div>

                {/* 底部一排指标 */}
                <div className="grid grid-cols-2 md:grid-cols-7 gap-x-2 gap-y-1.5 text-[10px] text-muted-foreground pt-1 border-t border-border/60">
                    <Metric icon={<Clock className="size-3" style={{ color: brandColor }} />} label={t('lastTime')} value={formatTime(row.time)} />
                    <Metric icon={<Zap className="size-3 text-amber-500" />} label={t('avgFtut')} value={row.success_count ? formatDuration(Math.round(avgFtut)) : '-'} />
                    <Metric icon={<Cpu className="size-3 text-blue-500" />} label={t('avgUseTime')} value={row.success_count ? formatDuration(Math.round(avgUseTime)) : '-'} />
                    <Metric icon={<ArrowDownToLine className="size-3 text-green-500" />} label={t('totalInput')} value={formatToken(row.input_total)} />
                    <Metric icon={<ArrowUpFromLine className="size-3 text-purple-500" />} label={t('totalOutput')} value={formatToken(row.output_total)} />
                    <Metric icon={<DollarSign className="size-3 text-emerald-500" />} label={t('totalCost')} value={Number(row.cost_total ?? 0).toFixed(6)} />
                    <Metric icon={<Timer className="size-3 text-orange-500" />} label={t('source')} value={row.last_source_name || t('apiSource')} />
                </div>
            </div>
        </TooltipProvider>
    );
}

function Metric({ icon, label, value }: { icon: ReactNode; label: string; value: string }) {
    return (
        <div className="flex items-center gap-1 min-w-0" title={label}>
            {icon}
            <span className="text-muted-foreground/80 shrink-0">{label}</span>
            <span className="truncate ml-auto tabular-nums">{value}</span>
        </div>
    );
}