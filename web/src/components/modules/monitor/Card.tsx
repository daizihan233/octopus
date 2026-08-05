import { useMemo } from 'react';
import { Clock, Zap, Cpu, ArrowDownToLine, ArrowUpFromLine, DollarSign, Timer, Rocket, TrendingUp, Loader2 } from 'lucide-react';
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
 * 单个 (channel, model) 可用性监控行（横向长条，与日志卡片同风格）
 * - 左上：模型 logo + 模型名 / 渠道名 + 冷静标注
 * - 右上：可用性竖条区（等宽固定 4px，固定高度 32px，最多显示最近 30 次）
 * - 下方：一行紧凑指标（最近调用/平均首字/平均总耗时/总输入/总输出/总费用/来源）
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
            <div className={`rounded-3xl border ${borderClass} bg-card w-full px-4 py-3 flex flex-col gap-2 transition-colors`}>
                {/* 顶行：左上模型信息 + 右上竖条 */}
                <div className="flex items-center gap-3 min-w-0">
                    <ModelAvatar size={28} />
                    <div className="min-w-0 flex-1">
                        <div className="flex items-center gap-1.5 min-w-0">
                            <span className="font-semibold text-sm text-card-foreground truncate" style={{ color: brandColor }} title={row.model_name}>
                                {row.model_name}
                            </span>
                            {isCooling && (
                                <span className="shrink-0 flex items-center gap-1 text-[10px] font-medium px-1.5 py-0.5 rounded-md bg-amber-500/15 text-amber-600 dark:text-amber-400" title={yellowLeft > 0 ? t('cooldownYellow') : t('cooldownRed')}>
                                    {yellowLeft > 0 ? t('cooldownYellow') : t('cooldownRed')} {Math.max(yellowLeft, redLeft)}s
                                </span>
                            )}
                        </div>
                        <div className="text-xs text-muted-foreground truncate" title={row.channel_name}>{row.channel_name}</div>
                    </div>

                    {/* 右上：可用性竖条区 */}
                    <div className="flex items-end h-8 gap-[3px] shrink-0 w-[210px] justify-end overflow-hidden">
                        {calls.length === 0 && (
                            <div className="flex items-center text-xs text-muted-foreground gap-1.5">
                                <Loader2 className="size-3 animate-spin" />
                                <span>{t('waiting')}</span>
                            </div>
                        )}
                        {calls.slice(-30).map((c) => {
                            const meta = STATUS_META[c.status];
                            const isOk = c.status === 'ok';
                            let heightPct = 100;
                            if (isOk) {
                                const ms = c.use_time > 0 ? c.use_time : c.ftut;
                                heightPct = maxSuccessMs > 0 ? Math.max(10, Math.round((ms / maxSuccessMs) * 100)) : 10;
                            }
                            return (
                                <Tooltip key={c.seq}>
                                    <TooltipTrigger asChild>
                                        {isOk ? (
                                            // 绿色竖条：底部真实色条 + 100% 高半灰命中区。
                                            // 竖条很矮时也能方便 hover；hover 时灰底变黑、绿条变深绿。
                                            <div
                                                className="group relative w-1 rounded-sm shrink-0 cursor-pointer bg-emerald-500/15 transition-colors hover:bg-black"
                                                style={{ height: '100%' }}
                                            >
                                                <div
                                                    className="absolute bottom-0 left-0 right-0 w-full rounded-sm bg-emerald-500 transition-colors group-hover:bg-emerald-800"
                                                    style={{ height: `${heightPct}%` }}
                                                />
                                            </div>
                                        ) : (
                                            <div
                                                className="w-1 rounded-sm shrink-0 cursor-pointer transition-[filter] hover:brightness-75"
                                                style={{ height: '100%', backgroundColor: meta.color }}
                                            />
                                        )}
                                    </TooltipTrigger>
                                    <TooltipContent className="border bg-card p-2 rounded-xl text-xs flex flex-col gap-0.5">
                                        <span>{formatTime(c.time)}</span>
                                        <span>{t(meta.labelKey)}</span>
                                        <span>{t('use_time')} {formatDuration(c.use_time)}</span>
                                        {isOk && <span>{t('ftut')} {formatDuration(c.ftut)}</span>}
                                    </TooltipContent>
                                </Tooltip>
                            );
                        })}
                    </div>
                </div>

                {/* 下方：分散指标栏 */}
                <div className="grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 xl:grid-cols-5 gap-x-6 gap-y-2 text-xs text-muted-foreground pt-2 border-t border-border/60">
                    <Metric icon={<Clock className="size-3.5 shrink-0" style={{ color: brandColor }} />} label={t('lastTime')} value={formatTime(row.time)} />
                    <Metric icon={<Rocket className="size-3.5 shrink-0 text-emerald-500" />} label={t('fastestFtut')} value={row.success_ftut_min > 0 ? formatDuration(row.success_ftut_min) : '-'} />
                    <Metric icon={<TrendingUp className="size-3.5 shrink-0 text-orange-500" />} label={t('slowestFtut')} value={row.success_ftut_max > 0 ? formatDuration(row.success_ftut_max) : '-'} />
                    <Metric icon={<Zap className="size-3.5 shrink-0 text-amber-500" />} label={t('avgFtut')} value={row.success_count ? formatDuration(Math.round(avgFtut)) : '-'} />
                    <Metric icon={<Cpu className="size-3.5 shrink-0 text-blue-500" />} label={t('avgUseTime')} value={row.success_count ? formatDuration(Math.round(avgUseTime)) : '-'} />
                    <Metric icon={<ArrowDownToLine className="size-3.5 shrink-0 text-green-500" />} label={t('totalInput')} value={formatToken(row.input_total)} />
                    <Metric icon={<ArrowUpFromLine className="size-3.5 shrink-0 text-purple-500" />} label={t('totalOutput')} value={formatToken(row.output_total)} />
                    <Metric icon={<DollarSign className="size-3.5 shrink-0 text-emerald-500" />} label={t('totalCost')} value={Number(row.cost_total ?? 0).toFixed(4)} />
                    <Metric icon={<Timer className="size-3.5 shrink-0 text-orange-500" />} label={t('source')} value={row.last_source_name || t('apiSource')} />
                </div>
            </div>
        </TooltipProvider>
    );
}

function Metric({ icon, label, value }: { icon: React.ReactNode; label: string; value: string }) {
    return (
        <div className="flex items-center gap-1.5 min-w-0" title={`${label}: ${value}`}>
            {icon}
            <span className="text-muted-foreground/80 shrink-0">{label}</span>
            <span className="truncate tabular-nums ml-auto">{value}</span>
        </div>
    );
}