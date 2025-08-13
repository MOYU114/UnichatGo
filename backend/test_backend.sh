#!/usr/bin/env bash
set -euo pipefail

BASE_URL="http://localhost:8080/api"
USERNAME="tester_$(date +%s)"
PASSWORD="pass123"
PROVIDER="openai"
MODEL1="gpt-5-nano"
MODEL2="gpt-5-mini"
TOKEN="YOUR_TOKEN_HERE"

req() {
  local method="$1"; shift
  local path="$1"; shift
  local data="$1"; shift
  local args=(-sS -X "$method" "$BASE_URL$path" -H "Content-Type: application/json")
  if [[ $# -gt 0 ]]; then
    args+=("$@")
  fi
  if [[ -n "$data" ]]; then
    args+=(-d "$data")
  fi
  curl "${args[@]}"
}

echo "Registering user $USERNAME"
resp=$(req POST "/users/register" "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}")
echo "Register response: $resp"
user_id=$(echo "$resp" | jq '.id')

resp=$(req POST "/users/login" "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}")
echo "Login response: $resp"
auth_token=$(echo "$resp" | jq -r '.auth_token')

AUTH_HEADER=("-H" "Authorization: Bearer $auth_token")
resp=$(req POST "/users/$user_id/token" "{\"provider\":\"$PROVIDER\",\"token\":\"$TOKEN\"}" "${AUTH_HEADER[@]}")
echo "Set token status: $resp"

resp=$(req POST "/users/$user_id/conversation/start" "{\"provider\":\"$PROVIDER\",\"session_id\":0,\"model_type\":\"$MODEL1\"}" "${AUTH_HEADER[@]}")
echo "Start conversation: $resp"
session_id=$(echo "$resp" | jq '.sessionId')

send_msg() {
	local text="$1"
	echo "Sending: $text"
	req POST "/users/$user_id/conversation/msg" "{\"session_id\":$session_id,\"content\":\"$text\",\"provider\":\"$PROVIDER\",\"model_type\":\"$MODEL1\"}" "${AUTH_HEADER[@]}"
}

sentences=("Hello, We will do a memory test! Please remember what I said." "Please remember my name is Bob." "What is my name?")
for msg in "${sentences[@]}"; do
	send_msg "$msg"
done

echo "Conversation history for session $session_id:"
sqlite3 app.db "SELECT role,content FROM messages WHERE session_id=$session_id ORDER BY id;"

resp=$(req POST "/users/$user_id/logout" "" "${AUTH_HEADER[@]}")
echo "Logout status: $resp"

resp=$(req POST "/users/login" "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}")
echo "Re-login: $resp"
auth_token=$(echo "$resp" | jq -r '.auth_token')
AUTH_HEADER=("-H" "Authorization: Bearer $auth_token")

echo "Reopen conversation $session_id after re-login"
req POST "/users/$user_id/conversation/start" "{\"provider\":\"$PROVIDER\",\"session_id\":$session_id,\"model_type\":\"$MODEL2\"}" "${AUTH_HEADER[@]}"

send_msg "What was our last conversation?"

echo "Conversation history after re-login:"
sqlite3 app.db "SELECT role,content FROM messages WHERE session_id=$session_id ORDER BY id;"

req DELETE "/users/$user_id" "" "${AUTH_HEADER[@]}"

echo "Flow completed"
