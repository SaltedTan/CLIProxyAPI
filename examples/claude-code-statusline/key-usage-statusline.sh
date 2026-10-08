#!/bin/sh
# Claude Code status line showing what is left of this client key's Claude
# allowance and of the Fable allowance shared by all keys, from CLIProxyAPI's
# GET /v1/key/usage.
#
# Reads the proxy from CPA_BASE_URL, else ANTHROPIC_BASE_URL, and the client key
# from CPA_API_KEY, else ANTHROPIC_AUTH_TOKEN, else ANTHROPIC_API_KEY. The line is
# cached for CPA_USAGE_CACHE_SECONDS (default 30) per proxy and key; when the proxy
# cannot be reached, the last line is shown marked as stale.

# Claude Code sends the session as JSON on stdin; this line does not need it.
cat >/dev/null

base=${CPA_BASE_URL:-$ANTHROPIC_BASE_URL}
key=${CPA_API_KEY:-${ANTHROPIC_AUTH_TOKEN:-$ANTHROPIC_API_KEY}}
[ -n "$base" ] && [ -n "$key" ] || exit 0
ttl=${CPA_USAGE_CACHE_SECONDS:-30}

cache_dir=${XDG_CACHE_HOME:-$HOME/.cache}/cpa-key-usage
mkdir -p "$cache_dir" 2>/dev/null
cache="$cache_dir/$(printf '%s %s' "$base" "$key" | cksum | cut -d' ' -f1)"

now=$(date +%s)
if [ -f "$cache" ]; then
  { read -r saved_at; read -r saved_line; } <"$cache"
  if [ $((now - ${saved_at:-0})) -lt "$ttl" ]; then
    printf '%s\n' "$saved_line"
    exit 0
  fi
fi

# The key is passed on stdin so it never appears in the process list.
if line=$(printf 'x-api-key: %s\n' "$key" |
  curl -fsS --max-time 3 -H @- "${base%/}/v1/key/usage?format=line&color=1" 2>/dev/null); then
  printf '%s\n%s\n' "$now" "$line" >"$cache.tmp" && mv "$cache.tmp" "$cache"
  printf '%s\n' "$line"
elif [ -n "$saved_line" ]; then
  printf '%s (stale)\n' "$saved_line"
fi
