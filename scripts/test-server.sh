#!/usr/bin/env bash
# End-to-end check of a DEPLOYED platform: connects your GitHub token, creates a project,
# approves it, follows the deploy and fetches the live URL.
#
#   ./scripts/test-server.sh
# reads PLATFORM_URL, API_KEY and GITHUB_TOKEN from scripts/test-server.env (git-ignored),
# or from the environment. PLATFORM_URL defaults to the demo Cloud Run service.
#
# The user is created on the server first (integrations and projects belong to a user row);
# the same email is reused on later runs.
# Optional: GITHUB_OWNER (org to create repos in), USER_EMAIL (default demo@example.com),
#           PROJECT_NAME, PROMPT, TIMEOUT (seconds per phase, default 900),
#           SKIP_CONNECT=1 (the token is already stored for that user).
# Needs curl and jq. Secrets are never printed or put on a command line.
set -euo pipefail

# local, git-ignored settings (PLATFORM_URL, API_KEY, GITHUB_TOKEN); real env vars win
ENV_FILE="$(dirname "$0")/test-server.env"
if [ -f "$ENV_FILE" ]; then
  _url="${PLATFORM_URL:-}"; _key="${API_KEY:-}"; _tok="${GITHUB_TOKEN:-}"
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  PLATFORM_URL="${_url:-${PLATFORM_URL:-}}"; API_KEY="${_key:-${API_KEY:-}}"; GITHUB_TOKEN="${_tok:-${GITHUB_TOKEN:-}}"
fi
PLATFORM_URL="${PLATFORM_URL:-https://anz-training-service-386127327935.asia-southeast1.run.app}"

: "${PLATFORM_URL:?set PLATFORM_URL}"
: "${API_KEY:?set API_KEY}"
[ "${SKIP_CONNECT:-}" = 1 ] || : "${GITHUB_TOKEN:?set GITHUB_TOKEN (or SKIP_CONNECT=1)}"
BASE="${PLATFORM_URL%/}"
USER_EMAIL="${USER_EMAIL:-demo@example.com}"
PROJECT_NAME="${PROJECT_NAME:-server-test-$(date +%H%M%S)}"
PROMPT="${PROMPT:-A hello world web app}"
TIMEOUT="${TIMEOUT:-900}"
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

HDR="$(mktemp)"; trap 'rm -f "$HDR"' EXIT; chmod 600 "$HDR"
set_user() { printf 'x-api-key: %s\nX-User-ID: %s\nContent-Type: application/json\n' "$API_KEY" "$1" > "$HDR"; }
set_user "unset"

step() { printf '\n== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# call METHOD PATH [json]  -> prints the body; fails on a non-2xx status.
# With a third argument "json" the request body is read from stdin.
call() {
  local method="$1" path="$2" out code
  out="$(mktemp)"
  if [ "${3:-}" = json ]; then
    code="$(curl -sS -o "$out" -w '%{http_code}' -X "$method" -H @"$HDR" --data-binary @- "$BASE$path")"
  else
    code="$(curl -sS -o "$out" -w '%{http_code}' -X "$method" -H @"$HDR" "$BASE$path")"
  fi
  if [ "${code:0:1}" != 2 ]; then
    echo "$method $path -> HTTP $code: $(head -c 300 "$out")" >&2; rm -f "$out"; return 1
  fi
  cat "$out"; rm -f "$out"
}

step "1. Server reachable, API key accepted"
call GET /api/integrations >/dev/null || fail "check PLATFORM_URL and API_KEY (and that the service allows unauthenticated invocations)"
echo "ok"

step "1b. Make sure the user exists: $USER_EMAIL"
USER_JSON="$(jq -n --arg e "$USER_EMAIL" '{email:$e,name:"Demo User"}' | call POST /users json 2>/dev/null)" \
  || USER_JSON="$(call GET "/users?email=$USER_EMAIL")" || fail "could not create or find the user"
USER_ID="$(jq -r '.ID // .id // empty' <<<"$USER_JSON")"
[ -n "$USER_ID" ] || fail "the server returned no user id: $USER_JSON"
set_user "$USER_ID"
echo "user id: $USER_ID"

if [ "${SKIP_CONNECT:-}" != 1 ]; then
  step "2. Connect GitHub token (stored encrypted on the server)"
  jq -n --arg t "$GITHUB_TOKEN" --arg o "${GITHUB_OWNER:-}" '{token:$t} + (if $o != "" then {owner:$o} else {} end)' \
    | call POST /api/integrations/github json >/dev/null \
    || fail "could not store the GitHub token (HTTP 422 above = GitHub rejected it: needs the repo and workflow scopes; 500 = server/database problem, see the revision log)"
  echo "ok"
fi

step "3. Create project: $PROJECT_NAME"
CREATED="$(jq -n --arg n "$PROJECT_NAME" --arg p "$PROMPT" '{name:$n,prompt:$p}' | call POST /api/projects json)" || fail "create failed"
ID="$(jq -r .id <<<"$CREATED")"
echo "project id: $ID"

# poll until the status is one of the wanted ones; prints each status/step change
wait_for() {
  local wanted="$1" deadline=$((SECONDS + TIMEOUT)) last="" body st sp
  while [ "$SECONDS" -lt "$deadline" ]; do
    body="$(call GET "/api/projects/$ID")" || fail "could not read the project"
    st="$(jq -r .status <<<"$body")"; sp="$(jq -r .current_step <<<"$body")"
    [ "$st/$sp" = "$last" ] || { printf '  %s  %-22s %s\n' "$(date +%T)" "$st" "$sp"; last="$st/$sp"; }
    if [ "$st" = failed ]; then fail "project failed: $(jq -r '.error_message // "no message"' <<<"$body")"; fi
    case " $wanted " in *" $st "*) PROJECT="$body"; return 0;; esac
    sleep 5
  done
  fail "timed out after ${TIMEOUT}s waiting for: $wanted"
}

step "4. Repo, code and pull request"
wait_for waiting_for_approval
echo "repo: $(jq -r .repo_url <<<"$PROJECT")"
echo "PR:   $(jq -r .pr_url <<<"$PROJECT")"

step "5. Approve (merges the PR, which triggers the GitHub Actions deploy)"
call POST "/api/projects/$ID/approve" >/dev/null || fail "approve failed"
echo "approved"

step "6. Build, test and deploy (GitHub Actions)"
wait_for success
LIVE="$(jq -r '.live_url // empty' <<<"$PROJECT")"
[ -n "$LIVE" ] || fail "project succeeded but has no live_url"
echo "live url: $LIVE"

step "7. Fetch the live URL"
for i in 1 2 3 4 5 6; do
  code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 20 "$LIVE" || true)"
  [ "$code" = 200 ] && { echo "GET $LIVE -> 200"; printf '\nPASS\n'; exit 0; }
  echo "  attempt $i: HTTP ${code:-none}"; sleep 10
done
fail "live URL did not return 200"
