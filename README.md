# UnichatGo

UnichatGo is a full-stack conversational assistant platform (backend ready, frontend pending) that lets users register, manage API credentials, and chat with AI providers using streaming responses. The repository is organized with a Go backend (`backend/`) and a placeholder for a Vue-based frontend (`frontend/`).

## Repository Layout
```
.
├── backend/              # Go API server (production ready)
│   ├── README.md         # Backend-specific documentation
│   ├── main.go           # Entrypoint wiring config, DB, router
│   └── internal/...      # Handlers, services, storage, workers
├── frontend/             # Planned Vue client (not implemented yet)
├── deploy/               # Future ops/infra artifacts
├── .env.example          # Sample environment variables (if present)
└── README.md           # (This file) repository overview
```

## Backend Overview
- **Language:** Go 1.21+
- **Database:** SQLite via `github.com/mattn/go-sqlite3`
- **Key features:**
    - User registration/login/logout, account deletion, API key storage per provider
    - Conversation sessions with auto-generated titles (first user message)
    - SSE streaming endpoint (`/conversation/msg`) returning `ack`, `stream`, `done`, `error`
    - Token-based auth middleware and per-user worker orchestration for AI calls
- **Config:** JSON file loaded via `UNICHATGO_CONFIG` env var (defaults to `backend/config.json`).
- **Run locally:**
  ```bash
  cd backend
  go run ./
  ```
- **Tests:** `go test ./...` (requires network access to download modules the first time).

Refer to `backend/README.md` for detailed API workflows, curl examples, and streaming event descriptions.

## Frontend Status
- The `frontend/` folder is reserved for a Vue 3 application that will consume the backend APIs and render streaming responses.
- No implementation is committed yet; recommended stack: Vite + Vue 3 + TypeScript, with SSE handling for `/conversation/msg`.
- When starting the frontend, ensure CORS or reverse-proxy settings align with the backend server (`:8080` by default).

## Development Workflow
1. **Start backend:** `go run ./backend` and verify SQLite DB (default `backend/app.db`).
2. **Prepare frontend (future):** scaffold a Vue app under `frontend/`, configure `.env` to point to backend API.
3. **Testing:**
    - Backend: `cd backend && go test ./...`
    - Frontend: run lint/unit tests once implemented (e.g., `npm run test`).
4. **Environment variables:**
    - `UNICHATGO_CONFIG` – path to backend config file.
    - Additional `.env` keys for the frontend will be documented once implemented.

## Planned Improvements
- Build the Vue frontend with authenticated session management, conversation list view, and streaming chat UI.
- Add deployment manifests/scripts under `deploy/` (Docker, Kubernetes, etc.).
- Extend backend tests for additional routes and add integration tests once the frontend is available.

## Reference
- [eino](https://github.com/cloudwego/eino)
- [Gochat](https://github.com/wangle201210/gochat/tree/main)

---
For backend details, see `backend/README.md`. When the frontend comes online, add its own README inside `frontend/` and update this overview accordingly.
