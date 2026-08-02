import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';

export type Theme = 'light' | 'dark' | 'system';
type ResolvedTheme = Exclude<Theme, 'system'>;

interface ThemeContextValue {
    theme: Theme; // 用户选择的主题模式。
    resolvedTheme: ResolvedTheme; // 结合系统偏好后实际生效的主题。
    setTheme: (theme: string) => void; // 设置并持久化主题模式。
}

const ThemeContext = createContext<ThemeContextValue | null>(null); // 全局主题状态上下文。

export function ThemeProvider({ children }: { children: ReactNode }) {
    const [theme, setThemeState] = useState<Theme>(() => {
        const storedTheme = localStorage.getItem('theme');
        return storedTheme === 'light' || storedTheme === 'dark' || storedTheme === 'system'
            ? storedTheme
            : 'system';
    });
    const [systemTheme, setSystemTheme] = useState<ResolvedTheme>(() =>
        window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
    );
    const resolvedTheme = theme === 'system' ? systemTheme : theme;

    useEffect(() => {
        const mediaQuery = window.matchMedia('(prefers-color-scheme: dark)');

        const handleChange = (event: MediaQueryListEvent) => {
            setSystemTheme(event.matches ? 'dark' : 'light');
        };

        mediaQuery.addEventListener('change', handleChange);
        return () => mediaQuery.removeEventListener('change', handleChange);
    }, []);

    useEffect(() => {
        document.documentElement.classList.toggle('dark', resolvedTheme === 'dark');
        document.querySelector('meta[name="theme-color"]')?.setAttribute(
            'content',
            resolvedTheme === 'dark' ? '#413a2c' : '#eae9e3'
        );
        localStorage.setItem('theme', theme);
    }, [resolvedTheme, theme]);

    const setTheme = (value: string) => {
        if (value === 'light' || value === 'dark' || value === 'system') {
            setThemeState(value);
        }
    };

    return (
        <ThemeContext value={{ theme, resolvedTheme, setTheme }}>
            {children}
        </ThemeContext>
    );
}

export function useTheme() {
    const context = useContext(ThemeContext);
    if (!context) {
        throw new Error('useTheme must be used within ThemeProvider');
    }
    return context;
}
