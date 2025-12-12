#!/usr/bin/env bash
set -euo pipefail

BASE_URL="http://localhost:8090/api"
USERNAME="tester_$(date +%s)"
PASSWORD="pass123"
PROVIDER="openai"
MODEL1="gpt-4o-mini"
MODEL2="gpt-5-mini"
TOKEN="YOUR_TOKEN_HERE"
COOKIE_JAR=$(mktemp)
trap 'rm -f "$COOKIE_JAR"' EXIT
req() {
  local method="$1"; shift
  local path="$1"; shift
  local data="$1"; shift
  local args=(-sS -X "$method" "$BASE_URL$path" -H "Content-Type: application/json" -b "$COOKIE_JAR" -c "$COOKIE_JAR")
  if [[ $# -gt 0 ]]; then
    args+=("$@")
  fi
  if [[ -n "$data" ]]; then
    args+=(-d "$data")
  fi
  curl "${args[@]}"
}

update_csrf_header() {
  local token
  token=$(awk '$6 == "csrf_token" {print $7}' "$COOKIE_JAR" | tail -n1)
  if [[ -z "$token" ]]; then
    echo "Failed to obtain CSRF token from cookie jar"
    exit 1
  fi
  CSRF_HEADER=("-H" "X-CSRF-Token: $token")
}

echo "Registering user $USERNAME"
resp=$(req POST "/users/register" "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}")
echo "Register response: $resp"
user_id=$(echo "$resp" | jq '.id')

resp=$(req POST "/users/login" "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}")
echo "Login response: $resp"
update_csrf_header

resp=$(req POST "/users/$user_id/token" "{\"provider\":\"$PROVIDER\",\"token\":\"$TOKEN\"}" "${CSRF_HEADER[@]}")
echo "Set token status: $resp"

resp=$(req POST "/users/$user_id/conversation/start" "{\"provider\":\"$PROVIDER\",\"session_id\":0,\"model_type\":\"$MODEL1\"}" "${CSRF_HEADER[@]}")
echo "Start conversation: $resp"
session_id=$(echo "$resp" | jq '.sessionId')

send_msg() {
	local text="$1"
	echo "Sending: $text"
	req POST "/users/$user_id/conversation/msg" "{\"session_id\":$session_id,\"content\":\"$text\",\"provider\":\"$PROVIDER\",\"model_type\":\"$MODEL1\"}" "${CSRF_HEADER[@]}"
}

sentences=("Hello, We will do a memory test! Please remember what I said." "Please remember my name is Bob." "What is my name?" "You need to search 'How's the weather in Singapore' in the Internet and tell me about it.")
for msg in "${sentences[@]}"; do
	start=$(date +%s)
	send_msg "$msg"
	end=$(date +%s)
	echo "Message '$msg' took $((end - start)) seconds"
done

start=$(date +%s)
echo "Conversation history for session $session_id:"
sqlite3 data/app.db "SELECT role,content FROM messages WHERE session_id=$session_id ORDER BY id;"

resp=$(req POST "/users/$user_id/logout" "" "${CSRF_HEADER[@]}")
echo "Logout status: $resp"

resp=$(req POST "/users/login" "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}")
echo "Re-login: $resp"
update_csrf_header

echo "Reopen conversation $session_id after re-login"
resume_resp=$(req POST "/users/$user_id/conversation/start" "{\"provider\":\"$PROVIDER\",\"session_id\":$session_id,\"model_type\":\"$MODEL2\"}" "${CSRF_HEADER[@]}")
echo "Resume response: $resume_resp"
end=$(date +%s)
echo "Restart conversation took $((end - start)) seconds"

followup="What was our last conversation?"
start=$(date +%s)
send_msg "$followup"
end=$(date +%s)
echo "Message '$followup' took $((end - start)) seconds"

echo "Conversation history after re-login:"
sqlite3 data/app.db "SELECT role,content FROM messages WHERE session_id=$session_id ORDER BY id;"

req DELETE "/users/$user_id" "" "${CSRF_HEADER[@]}"

echo "Flow completed"
