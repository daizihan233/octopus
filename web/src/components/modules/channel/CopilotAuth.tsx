'use client';

import { useState, useEffect, useCallback, useRef } from 'react';
import { Button } from '@/components/ui/button';
import { ExternalLink, Loader2, CheckCircle, XCircle, RefreshCw } from 'lucide-react';
import { toast } from '@/components/common/Toast';
import { useTranslations } from 'use-intl';
import { useAuthStore } from '@/api/endpoints/user';

interface CopilotAuthProps {
    existingKey: string;
    onKeyObtained: (key: string) => void;
}

type AuthState = 'idle' | 'starting' | 'pending' | 'done' | 'expired' | 'denied' | 'error';

export function CopilotAuth({ existingKey, onKeyObtained }: CopilotAuthProps) {
    const t = useTranslations('channel.form');
    const [state, setState] = useState<AuthState>(existingKey ? 'done' : 'idle');
    const [userCode, setUserCode] = useState('');
    const [verificationUri, setVerificationUri] = useState('');
    const [deviceCode, setDeviceCode] = useState('');
    const [countdown, setCountdown] = useState(0);
    const pollTimer = useRef<NodeJS.Timeout | null>(null);
    const countdownTimer = useRef<NodeJS.Timeout | null>(null);

    // 清理定时器
    useEffect(() => {
        return () => {
            if (pollTimer.current) clearInterval(pollTimer.current);
            if (countdownTimer.current) clearInterval(countdownTimer.current);
        };
    }, []);

    const startAuth = useCallback(async () => {
        setState('starting');
        try {
            const resp = await fetch('/api/v1/channel/copilot/start', {
                method: 'POST',
                headers: {
                    'Content-Type': 'application/json',
                    'Authorization': `Bearer ${useAuthStore.getState().token}`,
                },
            });
            const data = await resp.json();
            if (data.code !== 200) {
                throw new Error(data.message);
            }
            const flow = data.data;
            setUserCode(flow.user_code);
            setVerificationUri(flow.verification_uri);
            setDeviceCode(flow.device_code);
            setState('pending');
            setCountdown(flow.expires_in);

            // 启动轮询
            const interval = Math.max((flow.interval || 5) * 1000, 5000);
            pollTimer.current = setInterval(() => poll(flow.device_code), interval);

            // 倒计时
            countdownTimer.current = setInterval(() => {
                setCountdown(prev => {
                    if (prev <= 1) {
                        setState('expired');
                        return 0;
                    }
                    return prev - 1;
                });
            }, 1000);
        } catch (err) {
            setState('error');
            toast.error(t('copilotAuthFailed'), { description: String(err) });
        }
    }, []);

    const poll = useCallback(async (code: string) => {
        try {
            const resp = await fetch('/api/v1/channel/copilot/poll', {
                method: 'POST',
                headers: {
                    'Content-Type': 'application/json',
                    'Authorization': `Bearer ${useAuthStore.getState().token}`,
                },
                body: JSON.stringify({ device_code: code }),
            });
            const data = await resp.json();
            if (data.code !== 200) {
                throw new Error(data.message);
            }
            const result = data.data;

            switch (result.status) {
                case 'pending':
                    break; // 继续轮询
                case 'slow_down':
                    // 加大轮询间隔
                    if (pollTimer.current) {
                        clearInterval(pollTimer.current);
                        pollTimer.current = setInterval(() => poll(code), 10000);
                    }
                    break;
                case 'done':
                    cleanup();
                    setState('done');
                    onKeyObtained(result.key);
                    toast.success(t('copilotAuthSuccess'));
                    break;
                case 'expired':
                    cleanup();
                    setState('expired');
                    break;
                case 'denied':
                    cleanup();
                    setState('denied');
                    break;
            }
        } catch (err) {
            cleanup();
            setState('error');
            toast.error(t('copilotAuthFailed'), { description: String(err) });
        }
    }, []);

    const cleanup = useCallback(() => {
        if (pollTimer.current) {
            clearInterval(pollTimer.current);
            pollTimer.current = null;
        }
        if (countdownTimer.current) {
            clearInterval(countdownTimer.current);
            countdownTimer.current = null;
        }
    }, []);

    const reset = useCallback(() => {
        cleanup();
        setState('idle');
        setUserCode('');
        setVerificationUri('');
        setDeviceCode('');
        setCountdown(0);
    }, []);

    // 已有凭证：显示已授权状态
    if (state === 'done' && existingKey) {
        return (
            <div className="flex items-center gap-2 p-3 rounded-xl bg-green-500/10 border border-green-500/20">
                <CheckCircle className="h-4 w-4 text-green-500" />
                <span className="text-sm text-green-500">{t('copilotAuthorized')}</span>
                <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    onClick={reset}
                    className="ml-auto h-6 px-2 text-xs text-muted-foreground hover:bg-transparent"
                >
                    <RefreshCw className="h-3 w-3 mr-1" />
                    {t('copilotReauth')}
                </Button>
            </div>
        );
    }

    // 授权进行中
    if (state === 'pending' || state === 'starting') {
        return (
            <div className="space-y-3 p-4 rounded-xl border border-border bg-muted/20">
                <div className="flex items-center gap-2 text-sm font-medium">
                    <Loader2 className="h-4 w-4 animate-spin" />
                    {t('copilotAuthProgress')}
                </div>
                {userCode && (
                    <>
                        <div className="space-y-1">
                            <p className="text-xs text-muted-foreground">{t('copilotAuthInstruction')}</p>
                            <div className="flex items-center gap-3">
                                <code className="text-lg font-mono font-bold px-3 py-1.5 rounded-lg bg-background border">
                                    {userCode}
                                </code>
                                <a
                                    href={verificationUri}
                                    target="_blank"
                                    rel="noopener noreferrer"
                                    className="inline-flex items-center gap-1 text-sm text-primary hover:underline"
                                >
                                    {verificationUri}
                                    <ExternalLink className="h-3 w-3" />
                                </a>
                            </div>
                        </div>
                        <p className="text-xs text-muted-foreground">
                            {t('copilotAuthCountdown', { seconds: countdown })}
                        </p>
                    </>
                )}
            </div>
        );
    }

    // 过期/拒绝/错误
    if (state === 'expired' || state === 'denied' || state === 'error') {
        return (
            <div className="space-y-2">
                <div className="flex items-center gap-2 p-3 rounded-xl bg-destructive/10 border border-destructive/20">
                    <XCircle className="h-4 w-4 text-destructive" />
                    <span className="text-sm text-destructive">
                        {state === 'expired' && t('copilotAuthExpired')}
                        {state === 'denied' && t('copilotAuthDenied')}
                        {state === 'error' && t('copilotAuthFailed')}
                    </span>
                </div>
                <Button type="button" variant="outline" size="sm" onClick={reset}>
                    {t('copilotRetry')}
                </Button>
            </div>
        );
    }

    // idle：显示授权按钮
    return (
        <div className="space-y-2">
            <p className="text-xs text-muted-foreground">{t('copilotAuthDesc')}</p>
            <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={startAuth}
                className="rounded-xl"
            >
                {t('copilotAuthStart')}
            </Button>
        </div>
    );
}
