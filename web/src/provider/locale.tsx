import { useEffect, useState, type ReactNode } from 'react';
import { IntlProvider } from 'use-intl';
import { useSettingStore, type Locale } from '@/stores/setting';

import zh_hansMessages from '@/locales/zh_hans.json';
import zh_hantMessages from '@/locales/zh_hant.json';
import enMessages from '@/locales/en.json';

const messages: Record<Locale, typeof zh_hansMessages> = { // 各语言对应的客户端消息集合。
    zh_hans: zh_hansMessages,
    zh_hant: zh_hantMessages,
    en: enMessages,
};

export function LocaleProvider({ children }: { children: ReactNode }) {
    const { locale } = useSettingStore();
    const [currentLocale, setCurrentLocale] = useState<Locale>('zh_hans');

    useEffect(() => {
        setCurrentLocale(locale);
    }, [locale]);

    return (
        <IntlProvider
            locale={currentLocale}
            messages={messages[currentLocale]}
            timeZone="Asia/Shanghai"
        >
            {children}
        </IntlProvider>
    );
}
