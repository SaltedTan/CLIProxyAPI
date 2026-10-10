# Claude Code status line for client key usage

Shows, in Claude Code's status line, what is left of your client API key's Claude
allowance and when its 7-day window resets, and two figures shared by all keys: the
Claude 5-hour limit left across the proxy's Claude subscriptions, and the Fable
allowance left across its Claude accounts that serve Fable:

```
Claude 63% left · resets in 4d22h │ 5h 1830% of 2600% left · +500% in 2h13m │ Fable 69% left · +3% in 19h59m
```

The percentages are green above 50%, yellow down to 20% and red below. Other
states read `Claude: limit reached · back in 1d2h`, `Claude 100% left · 7d window
starts on next use` (no window running yet), `5h: n/a` (no 5-hour figure) and
`Fable: n/a` (no Fable figure). `(partial)` after the 5-hour or Fable figure means
some accounts could not be read, so it may be off.

`5h 1830% of 2600% left` is the 5-hour (session) limit left across every enabled
Claude OAuth subscription of the proxy, whatever models it serves. 100% is one
Claude Pro plan's 5-hour limit, and each subscription adds its plan's: Pro 100%,
Team 125%, Max 5x 500% and Max 20x 2000% (a plan the proxy cannot tell adds its
credential weight, at least 100%). Here a Pro, a Max 5x and a Max 20x subscription
make 2600%, and 1830% of it is left; its color follows the share left, 70%.
`+500% in 2h13m` is the next time the figure grows, and by how much: a
subscription's 5-hour window resetting, or a subscription that has used up its
weekly allowance (and so counts as having nothing left) getting it back.

`+3% in 19h59m` is the next time the Fable pool grows, and by how much: an account's
Fable window resetting, or an account that has used up its overall weekly allowance
(and so counts as having no Fable left) getting it back. When no Fable is left, it
is when Fable returns.

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
without `format`, JSON for scripts, with `claude`, `five_hour` (in Claude Pro units:
1 is one Pro plan's 5-hour limit) and `fable` objects:

```sh
curl -H "x-api-key: $ANTHROPIC_AUTH_TOKEN" "$ANTHROPIC_BASE_URL/v1/key/usage?format=text"
```
