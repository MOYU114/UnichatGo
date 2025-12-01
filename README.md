# UnichatGo

UnichatGo is a conversational application powered by a Go backend and a Vue 3 frontend. The backend exposes authenticated REST + SSE endpoints, while the frontend renders streaming responses, manages per-provider tokens, and supports model switching.

## Quick Start
- Backend (local):
  ```bash
  cd backend
  export UNICHATGO_APIKEY_KEY=$(openssl rand -base64 32)
  go run ./
  ```
- Frontend (local):
  ```bash
  cd frontend
  npm install
  npm run dev
  ```
- Docker Compose (with persisted DB):
  ```bash
  mkdir -p backend/data
  export UNICHATGO_APIKEY_KEY=$(openssl rand -base64 32)
  docker compose up --build
  ```
- Tear down containers: `docker compose down`

## Repository Layout
```
.
├── backend/          # Go server (users, sessions, SSE)
├── frontend/         # Vue 3 + Vite client
├── deploy/           # Reserved for infra scripts
└── README.md
```

## Backend Highlights
- Go 1.21+, SQLite storage.
- Auth: login issues HttpOnly cookies + CSRF tokens; supports logout and account deletion.
- Conversations:
  - `POST /users/:id/conversation/start`: create or resume a session (auto-titles first message).
  - `POST /users/:id/conversation/msg`: SSE stream returning `ack` → `stream` → `done`/`error`.
  - `GET /users/:id/conversation/sessions/:session_id/messages`: fetch historical messages.
- Provider tokens are encrypted using AES-GCM; set `UNICHATGO_APIKEY_KEY` (32-byte key) before running.
- Run locally:
  ```bash
  cd backend
  export UNICHATGO_APIKEY_KEY=$(openssl rand -base64 32)
  go run ./
  ```
- Tests: `cd backend && go test ./...` (requires network to fetch modules on first run).
- SQLite path: `./data/app.db` by default (see `backend/config.json`). Mount `backend/data` when running in Docker.

See `backend/README.md` for detailed API walkthroughs and curl examples.

## Frontend Highlights
- Built with Vue 3, Pinia, Element Plus, Vite.
- Axios client uses Cookies + CSRF header automatically; SSE via `fetch` + ReadableStream.
- Features:
  - Authentication-aware layout (login/register/dashboard).
  - Session sidebar, chat panel with Markdown rendering, provider/model dropdowns.
  - Token dialog accessible from the user menu; tokens stored per provider.
  - Provider/model metadata lives in `src/store/session.js` (`PROVIDERS` mapping); extend as needed.
- Development:
  ```bash
  cd frontend
  npm install
  npm run dev   # Vite dev server with /api proxy -> http://localhost:8080
  ```
- Build: `npm run build`.

## Common Commands
| Task | Command |
|------|---------|
| Start backend | `cd backend && export UNICHATGO_APIKEY_KEY=... && go run ./` |
| Backend tests | `cd backend && go test ./...` |
| Frontend dev  | `cd frontend && npm install && npm run dev` |
| Frontend build | `npm run build` |
| Docker compose (first time) | `export UNICHATGO_APIKEY_KEY=... && docker compose up --build` |
| Docker compose (after init) | `docker compose up` |
| Clean up | `docker compose down` |

## Environment Variables
- `UNICHATGO_CONFIG`: Optional path to backend config JSON (defaults to `backend/config.json`).
- `UNICHATGO_APIKEY_KEY`: **Required** 32-byte key for encrypting provider tokens.
- `VITE_API_BASE_URL`: Frontend override for API endpoint (defaults to `/api` and proxied to `http://localhost:8080` during development).

## References
- [cloudwego/eino](https://github.com/cloudwego/eino)
- [wangle201210/gochat](https://github.com/wangle201210/gochat/tree/main)

Will add `/deploy` later.

***Feel free to add new providers/models, or customize the UI. Contributions are welcome!***
