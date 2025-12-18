# UnichatGo

UnichatGo is a conversational application powered by a Go backend and a Vue 3 frontend. The backend exposes authenticated REST + SSE endpoints with built-in OpenAI/Gemini/Claude provider support, while the frontend renders streaming responses, manages per-provider tokens, and supports model switching.

## Quick Start
- Backend (local):
  ```bash
  cd backend
  #  add needed env into current shell, example: 
  export UNICHATGO_APIKEY_KEY=$(openssl rand -base64 32)
  ...
  go run main.go
  ```
- Frontend (local):
  ```bash
  cd frontend
  npm install
  npm run dev
  ```
- Docker Compose (with persisted MySQL + Redis):
  ```bash
  #  add needed env into current shell, example: 
  export UNICHATGO_APIKEY_KEY=$(openssl rand -base64 32)
  ...
  docker compose up --build
  ```
- Tear down containers: `docker compose down`

## Repository Layout
```
.
├── backend/            # Go server (users, sessions, SSE)
├── frontend/           # Vue 3 + Vite client
├── docker-compose.yml  # Docker Compose File
└── README.md
```

## Backend Highlights
- Go 1.24+, SQLite/MySQL storage, Redis listener and cache.
- Auth: login issues HttpOnly cookies + CSRF tokens; supports logout and account deletion.
- Conversations:
  - `POST /users/:id/conversation/start`: create or resume a session (auto-titles first message).
  - `POST /users/:id/conversation/msg`: SSE stream returning `ack` → `stream` → `done`/`error`.
  - `GET /users/:id/conversation/sessions/:session_id/messages`: fetch historical messages.
- `POST /users/:id/uploads`: upload temporary files (text or image). Each attachment is assigned a `file_id`, MIME type, size, and TTL; include the desired `file_ids` in subsequent messages to reference them. Text files are chunked through the `temp_file_reader` tool, while images are embedded inline (Base64) for vision-capable models (GPT-4o, Claude 3.5, Gemini 1.5, etc.). Attachment size/TTL limits are configurable in `backend/config.json`.
- Provider tokens are encrypted using AES-GCM; set `UNICHATGO_APIKEY_KEY` (32-byte key) before running. Users can list/remove their provider tokens via `/api/users/:id/token` (GET/DELETE).
- File & image support: the backend can handle small text documents and screenshots simultaneously. Text attachments consume your model’s token quota (chunk size defaults to 2k characters per tool call), while inline images depend on the chosen model’s multimodal capability. If no `file_ids` are supplied in a request, the backend still exposes the session’s cached text files for optional lookup but does not force a tool invocation.
- Per-user Job Queue + LRU Scheduling: Each user maintains an independent task queue, ensuring response order and resource isolation even under high concurrency.
- Run locally:
  ```bash
  cd backend
  #  add needed env into current shell, example: 
  export UNICHATGO_APIKEY_KEY=$(openssl rand -base64 32)
  ...
  go run main.go
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
	- Provide openai/gemini/claude API interface.
- Development:
  ```bash
  cd frontend
  npm install
  npm run dev   # Vite dev server with /api proxy -> http://localhost:8090
  ```
- Build: `npm run build`.

## Common Commands
| Task | Command |
|------|---------|
| Start backend | `cd backend && export UNICHATGO_APIKEY_KEY=... [GOOGLE_API_KEY=... GOOGLE_SEARCH_ENGINE_ID=...] && go run ./` |
| Backend tests | `cd backend && go test ./...` |
| Frontend dev  | `cd frontend && npm install && npm run dev` |
| Frontend build | `npm run build` |
| Docker compose (first time) | `export UNICHATGO_APIKEY_KEY=... && docker compose up --build` |
| Docker compose (after init) | `docker compose up` |
| Clean up | `docker compose down` |

## Environment Variables
- `UNICHATGO_CONFIG`: Optional path to backend config JSON (defaults to `backend/config.json`).
- `UNICHATGO_APIKEY_KEY`: **Required** 32-byte key for encrypting provider tokens.
- `UNICHATGO_DB`: Choose database driver (`sqlite3` by default, set to `mysql` when using MySQL).
  - If you need to use MySQL, you MUST config following variables:
    - `MYSQL_HOST`: your MySQL IP address;
    - `MYSQL_ROOT_PASSWD`: you root password for MySQL;
    - `MYSQL_UNICHATGO_PASSWD`: you unichatgo (default user name) database password for MySQL.
- `REDIS_HOST` / `REDIS_PASSWORD` / `REDIS_DB`: Optional overrides when pointing the backend at a non-default Redis instance (defaults provided in `backend/config.json`; Compose uses service name `redis`).
- `GOOGLE_API_KEY`: Optional; when set with `GOOGLE_SEARCH_ENGINE_ID`, enables the Google Search tool (and unified `web_search` fallback).
- `GOOGLE_SEARCH_ENGINE_ID`: Optional; Programmable Search Engine ID paired with `GOOGLE_API_KEY`.
- `VITE_API_BASE_URL`: Frontend override for API endpoint (defaults to `/api` and proxied to `http://localhost:8090` during development).

## References
- [cloudwego/eino](https://github.com/cloudwego/eino)
- [wangle201210/gochat](https://github.com/wangle201210/gochat/tree/main)


***Feel free to add new providers/models, or customize the UI. Contributions are welcome!***
