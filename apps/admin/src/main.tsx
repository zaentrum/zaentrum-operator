import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import './styles/global.css';
import { App } from './App';

// The admin UI is served under /manage (nginx location + Vite base). The
// router basename keeps client-side paths in sync with that mount point.
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <BrowserRouter basename="/manage">
      <App />
    </BrowserRouter>
  </StrictMode>,
);
