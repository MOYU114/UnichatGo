# UnichatGo Frontend

Vue 3 + Vite client for UnichatGo. Handles authentication, session list, streaming chat, provider/model selection, and token management.

## Tech Stack
- Vue 3 + Pinia + Vue Router
- Element Plus UI
- Axios for REST (Cookies + CSRF)
- `fetch` + ReadableStream for SSE
- Markdown rendering via `marked` + `dompurify`

## Scripts
```bash
npm install         # install dependencies
npm run dev         # start Vite dev server (proxy /api -> http://localhost:8090)
npm run build       # production build
npm run preview     # preview build
```

## Environment
- `VITE_API_BASE_URL` (optional): override API base URL (default `/api`).
  In Docker, Nginx proxies `/api` to `backend:8090` by default (configurable via `frontend/nginx.conf`).

## Notes
- Provider/model presets defined in `src/store/session.js` (`PROVIDERS`). Update as needed.
- Token dialog accessed via user menu in the dashboard header.
- For dev proxy to work, backend must run at `http://localhost:8090`.
