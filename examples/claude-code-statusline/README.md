# Claude Code status line for client key usage

Shows, in Claude Code's status line, what is left of your client API key's Claude
allowance and when its 7-day window resets, and how much Fable allowance is left
across the proxy's Claude accounts (shared by all keys):

```
Claude 63% left · resets in 4d22h │ Fable 69% left · +3% in 19h59m
```

The percentages are green above 50%, yellow down to 20% and red below. Other
states read `Claude: limit reached · back in 1d2h`, `Claude 100% left · 7d window
starts on next use` (no window running yet) and `Fable: n/a` (no Fable figure). `(partial)` after the Fable figure means some
accounts could not be read, so it may be off.
`+3% in 19h59m` is the next time the pool grows, and by how much: an account's Fable
window resetting, or an account that has used up its overall weekly allowance (and
so counts as having no Fable left) getting it back. When no Fable is left, it is
when Fable returns.

## Setup

1. Copy `key-usage-statusline.sh` to `~/.claude/` and make it executable.
2. Add to `~/.claude/settings.json`:

   ```json
   {
     "statusLine": {
       "type": "command",
       "command": "~/.claude/key-usage-statusline.sh",
       "refreshInterval": 60
     }
   }
   ```

The script uses the proxy and key Claude Code already uses (`ANTHROPIC_BASE_URL` and
`ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY`). Set `CPA_BASE_URL` and `CPA_API_KEY`
to use others. It needs `curl`. It caches the line for 30 seconds
(`CPA_USAGE_CACHE_SECONDS`) in a private file per proxy and key, when `sha256sum` or
`shasum` is available.

Without the script, a one-line command works too, with no cache:

```json
"command": "curl -fsS --max-time 3 -H \"x-api-key: $ANTHROPIC_AUTH_TOKEN\" \"$ANTHROPIC_BASE_URL/v1/key/usage?format=line&color=1\""
```

## Other formats

The same endpoint answers `?format=text` with a longer summary for a terminal and,
without `format`, JSON for scripts:

```sh
curl -H "x-api-key: $ANTHROPIC_AUTH_TOKEN" "$ANTHROPIC_BASE_URL/v1/key/usage?format=text"
```
