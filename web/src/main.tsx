import { createRoot } from 'react-dom/client';
import { AppContainer } from '@/components/app';
import { ServiceWorkerRegister } from '@/components/sw-register';
import { TooltipProvider } from '@/components/animate-ui/components/animate/tooltip';
import { Toaster } from '@/components/ui/sonner';
import { LocaleProvider } from '@/provider/locale';
import QueryProvider from '@/provider/query';
import { ThemeProvider } from '@/provider/theme';
import './globals.css';

createRoot(document.getElementById('root')!).render(
  <>
    <ServiceWorkerRegister />
    <ThemeProvider>
      <QueryProvider>
        <LocaleProvider>
          <TooltipProvider>
            <AppContainer />
            <Toaster />
          </TooltipProvider>
        </LocaleProvider>
      </QueryProvider>
    </ThemeProvider>
  </>,
);
