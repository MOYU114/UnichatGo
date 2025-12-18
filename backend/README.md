# UnichatGo Backend

Go-based API server that powers user authentication, session management, and streaming AI conversations for Unichat. The backend stores conversations in SQLite, issues auth tokens, and streams assistant output over Server-Sent Events so the UI can render incremental responses while the LLM finishes thinking.

## Features
- User registration, login, logout, and account deletion with token revocation.
- API key management per provider (e.g., OpenAI) with validation before invoking AI.
- Conversation lifecycle: list sessions, start or resume, delete, and auto-generate titles after the first message.
- Streaming `/conversation/msg` endpoint that emits `ack`, `stream`, `done`, and optional `error` events.
- SQLite schema and migrations baked into the binary; no external database required by default.
- Optional Redis cache used for bearer-token/worker state storage (defaults to disabled; enable via `redis` config block and Docker Compose).

## Directory Structure
```
backend/
├── main.go                  # Entrypoint wiring config, DB, and HTTP router
├── internal/
│   ├── api                  # Gin handlers / HTTP surface
│   ├── auth                 # Token issuance, middleware, helpers
│   ├── config               # JSON config loader (UNICHATGO_CONFIG)
│   ├── models               # User / Session / Message structs
│   ├── redis                # Redis configure and basic caller
│   ├── service
│   │   ├── ai               # Streaming AI model adapter
│   │   └── assistant        # Domain logic for users, sessions, titles
│   ├── storage              # SQLite open + migrate helpers
│   └── worker               # Per-user worker managing AI sessions & caching
├── config.json              # Default configuration (server address, DB path, providers)
├── app.db                   # SQLite database (auto-created if missing)
└── test_backend.sh          # Convenience test script
```

## Prerequisites
- Go 1.21+
- SQLite (the Go binary links against `github.com/mattn/go-sqlite3`)
- Optional: custom config file referenced via `UNICHATGO_CONFIG`. When unset, `config.json` at repo root is used.

Sample minimal config (`config.json`):
```json
{
  "basic_config": {
    "server_address": ":8090",
    "min_workers": 3,
    "max_workers": 10,
    "queue_size" : 100,
    "worker_idle_timeout_minutes": 30,
    "file_base_dir": "./data/uploads",
    "temp_file_ttl_minutes": 1440,
    "temp_file_clean_interval_minutes": 60
  },
  "providers": {
    "openai": {
      "model": "gpt-5-nano",
      "api_key": "",
      "base_url": "https://api.openai.com/v1"
    },
    "gemini": {
      "model": "gemini-2.5-flash",
      "api_key": "",
      "base_url": ""
    },
    "claude": {
      "model": "claude-haiku-4-5",
      "api_key": "",
      "base_url": "https://api.anthropic.com"
    }
  },
  "databases": {
    "sqlite3": {
      "dsn": "./data/app.db"
    },
    "mysql": {
      "host": "mysql_host_ip",
      "port": 3306,
      "username": "unichatgo",
      "password": "password",
      "db_name": "unichatgo",
      "params": "charset=utf8mb4&parseTime=true&loc=Local"
    }
  },
  "redis": {
    "host": "redis",
    "port": 6379,
    "password": "password",
    "db_name": 0
  }
}
```

Set environment overrides when needed:
```bash
export UNICHATGO_CONFIG=/full/path/to/config.json
export UNICHATGO_APIKEY_KEY=$(openssl rand -base64 32)  # 32-byte key used to encrypt provider API tokens
export UNICHATGO_DB=sqlite3                              # or mysql
export REDIS_HOST=redis                                # Optional: override redis host (defaults to config.json)
export REDIS_PASSWORD=password                         # Optional: override redis password
export GOOGLE_API_KEY=...                              # Optional: enables Google Search tool
export GOOGLE_SEARCH_ENGINE_ID=...                     # Optional: enables Google Search tool
export UNICHATGO_WORKER_DEBUG=1                        # Optional: verbose worker scheduling logs
```
### Token Management APIs
- `POST /api/users/:id/token`: upsert encrypted provider token.
- `GET /api/users/:id/token`: list configured providers (without exposing token values).
- `DELETE /api/users/:id/token`: remove a provider token; returns `404` if not found.
## Running Locally
```bash
go run ./backend
```
The API listens on `basic.server_address` (defaults to `:8090`).

## Docker Compose Notes
The top-level `docker-compose.yml` starts `mysql`, `redis`, `backend`, and `frontend` services. When running in Compose:
- Configure `databases.mysql.host` as `mysql` and `redis.host` as `redis` in `config.json` (already defaulted in the sample).
- The Redis container enforces the password from `${REDIS_PASSWD}`; ensure `config.json`/environment variables match.
- Conversation data (and temporary files) are persisted under `backend/data/...`, so mount that directory when deploying elsewhere.

## Testing
```bash
go test ./...
```
If module downloads are blocked in your environment, populate `GOMODCACHE`/`GOCACHE` locally first or run tests on a networked machine.

## API Quickstart
Below is an end-to-end example using `curl` (replace placeholders in ALL_CAPS):

```bash
BASE=http://localhost:8090
USER_ID=1

# 1. Register
curl -s -X POST "$BASE/api/users/register" \
  -H 'Content-Type: application/json' \
  -d '{"username":"demo","password":"secret"}'

# 2. Login and capture the auth token
LOGIN=$(curl -s -X POST "$BASE/api/users/login" \
  -H 'Content-Type: application/json' \
  -d '{"username":"demo","password":"secret"}')
AUTH_TOKEN=$(echo "$LOGIN" | jq -r '.auth_token')
USER_ID=$(echo "$LOGIN" | jq -r '.id')

# 3. Store your provider API key (required before invoking AI)
curl -s -X POST "$BASE/api/users/$USER_ID/token" \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -d '{"provider":"openai","token":"sk-..."}'

# 4. Start a conversation (session_id=0 creates a new session)
curl -s -X POST "$BASE/api/users/$USER_ID/conversation/start" \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -d '{"provider":"openai","model_type":"gpt-5-nano","session_id":0}'

# 5. Send a message and stream the reply
curl -N -X POST "$BASE/api/users/$USER_ID/conversation/msg" \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -d '{"session_id":SESSION_ID,"provider":"openai","model_type":"gpt-5-nano","content":"Hello"}'
```

### Streaming Events
The `/conversation/msg` endpoint responds with Server-Sent Events:
- `ack`: echoes the stored user message (DB ID, timestamps).
- `stream`: incremental assistant text chunks (multiple events).
- `done`: final payload with both user + assistant messages, and `title` if this was the first message in the session.
- `error`: emitted if the worker fails mid-stream.

Clients should keep the HTTP connection open until `done` or `error` arrives; UI layers can update the session title immediately when it appears in the `done` payload.

## Session Titles
On the first user message of a session, the worker:
1. Loads conversation history (initially empty).
2. Calls the configured assistant model to generate a concise title.
3. Persists the title via `UpdateSessionTitle` and includes it in the `done` event payload.

Existing sessions keep their stored titles; deleting a session or user automatically cascades related messages and tokens.

## Useful Commands
Provide a useful `test_backend.sh` to test all the scenario, feel free to use or change it.
```bash
# Format code
go fmt ./...

# Clean module metadata after dependency changes
go mod tidy

# Run the service
go run main.go

# Use another bash to run the script
chmod +x test_backend.sh
./test_backend.sh
```
