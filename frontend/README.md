## Async conversation recovery (2026-10-12)

The UI negotiates `/api/capabilities`. Async mode submits a frozen client ID and
payload, then observes authenticated SQL history rather than keeping the model
call attached to SSE. Unknown acceptance retains the original request; closing
the page does not cancel accepted execution. Uncertain outcomes block the
conversation until explicit audited abandonment.

Async submission requires HTTPS/localhost and Web Locks. The retry journal uses
localStorage scoped to user/session and can contain message text and attachment
IDs; it does not store Provider credentials. Do not clear an unknown-acceptance
journal to force a new request ID. Run `npm test` and `npm run build` for automated
checks. Real-browser E2E and full backend feature parity are still release gates;
see `../docs/roadmap/implementation-checkpoint-2026-10-12.md`.

---

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
