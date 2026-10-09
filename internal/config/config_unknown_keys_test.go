package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func unknownKeyWarning(path string, line int) string {
	return fmt.Sprintf("config: unknown key %q (line %d) is ignored", path, line)
}

func TestUnknownConfigKeys(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want []string
	}{
		{
			name: "legacy root typo",
			doc:  "port: 8317\ndebgu: true\n",
			want: []string{unknownKeyWarning("debgu", 2)},
		},
		{
			name: "v8 nested typo",
			doc:  "config-version: 8\naccess:\n  api-keyz:\n    - sk-client-1\n",
			want: []string{unknownKeyWarning("access.api-keyz", 3)},
		},
		{
			name: "routing typo",
			doc:  "routing:\n  strategy: fill-first\n  stratgy: round-robin\n  retry:\n    request-retri: 2\n",
			want: []string{unknownKeyWarning("routing.stratgy", 3), unknownKeyWarning("routing.retry.request-retri", 5)},
		},
		{
			name: "nested v8 struct typo",
			doc:  "config-version: 8\nserver:\n  port: 8317\n  tls:\n    enable: false\n    certt: a.pem\n",
			want: []string{unknownKeyWarning("server.tls.certt", 6)},
		},
		{
			name: "line numbers skip comments and blank lines",
			doc:  "# header\n\nrouting:\n  # comment\n  strategy: fill-first\n\n  sesion-affinity: true\n",
			want: []string{unknownKeyWarning("routing.sesion-affinity", 7)},
		},
		{
			name: "user maps are not inspected",
			doc: `config-version: 8
access:
  api-keys:
    - sk-client-0001
  api-key-limits:
    sk-client-0001: 1.5
  api-key-names:
    sk-client-0001: laptop
requests:
  passthrough-headers: true
  payload:
    override:
      - models:
          - name: gpt-*
            headers:
              X-Anything: value
        params:
          reasoning.effort: high
oauth:
  model-alias:
    codex:
      - name: gpt-5
        alias: g5
  excluded-models:
    any-channel: [a]
plugins:
  configs:
    my-plugin:
      enabled: true
      custom-option:
        nested: 1
`,
		},
		{
			name: "legacy provider list items",
			doc: `claude-api-key:
  - api-key: upstream-key-1
    base-url: https://api.anthropic.com
  - api-key: upstream-key-2
    base-urll: https://api.anthropic.com
    cloak:
      mod: always
    models:
      - name: claude-sonnet-4
        alias: sonnet
      - name: claude-opus-4
        aliass: opus
    request-scoped-errors:
      - status: 400
        actoin: stop
openai-compatibility:
  - name: compat
    base-url: https://example.com/v1
    api-key-entries:
      - api-key: upstream-key-3
        proxy-ur: socks5://127.0.0.1:1080
    models:
      - name: m1
        alias: a1
        thinking:
          levles: [low]
`,
			want: []string{
				unknownKeyWarning("claude-api-key[1].base-urll", 5),
				unknownKeyWarning("claude-api-key[1].cloak.mod", 7),
				unknownKeyWarning("claude-api-key[1].models[1].aliass", 12),
				unknownKeyWarning("claude-api-key[1].request-scoped-errors[0].actoin", 15),
				unknownKeyWarning("openai-compatibility[0].api-key-entries[0].proxy-ur", 21),
				unknownKeyWarning("openai-compatibility[0].models[0].thinking.levles", 26),
			},
		},
		{
			name: "v8 provider list items",
			doc: `config-version: 8
api-keys:
  gemini:
    - name: g1
      base-url: https://generativelanguage.googleapis.com
      headers:
        X-Custom: value
      models:
        - name: gemini-2.5-pro
          alais: pro
      keys:
        - api-key: upstream-key
          auth-index: abc
          not-a-field: 1
  claude:
    - name: c1
      keys:
        - api-key: upstream-key-2
          prefx: team
          rebuild-mid-system-message: true
  openai-compatibility:
    - name: compat
      base-urll: https://example.com/v1
      auth_index: def
      api-key-entries:
        - api-key: ignored-key
      keys:
        - api-key: upstream-key-3
          auth-index: ghi
          proxy-ur: socks5://127.0.0.1:1080
`,
			want: []string{
				unknownKeyWarning("api-keys.gemini[0].models[0].alais", 10),
				unknownKeyWarning("api-keys.gemini[0].keys[0].not-a-field", 14),
				unknownKeyWarning("api-keys.claude[0].keys[0].prefx", 19),
				unknownKeyWarning("api-keys.openai-compatibility[0].base-urll", 23),
				unknownKeyWarning("api-keys.openai-compatibility[0].api-key-entries", 25),
				unknownKeyWarning("api-keys.openai-compatibility[0].keys[0].proxy-ur", 30),
			},
		},
		{
			name: "nested typed lists outside providers",
			doc: `config-version: 8
requests:
  payload:
    override:
      - models:
          - name: gpt-*
            protocl: responses
        params:
          reasoning.effort: high
    filter:
      - models:
          - name: gpt-*
        paramz: [metadata]
payload:
  default:
    - modles:
        - name: gemini-*
codex:
  live-media-relay:
    ice-servers:
      - urls: [stun:stun.example.com]
        usernme: u
oauth:
  providers:
    codex:
      live-media-relay:
        ice-servers:
          - urlz: [stun:stun.example.com]
plugins:
  store-auth:
    - match: example.com
      tokn-env: TOKEN
`,
			want: []string{
				unknownKeyWarning("requests.payload.override[0].models[0].protocl", 7),
				unknownKeyWarning("requests.payload.filter[0].paramz", 13),
				unknownKeyWarning("payload.default[0].modles", 16),
				unknownKeyWarning("codex.live-media-relay.ice-servers[0].usernme", 22),
				unknownKeyWarning("oauth.providers.codex.live-media-relay.ice-servers[0].urlz", 28),
				unknownKeyWarning("plugins.store-auth[0].tokn-env", 32),
			},
		},
		{
			name: "free-form values in list items stay open",
			doc: `claude-api-key:
  - api-key: upstream-key
    headers:
      X-Anything: value
payload:
  override:
    - models:
        - name: gpt-*
          headers:
            X-Client: codex*
          match:
            - any.json.path: 1
      params:
        any.json.path: value
oauth-model-alias:
  codex:
    - name: gpt-5
      alias: g5
      any-field: 1
oauth-settings:
  claude:
    - anything: 1
oauth-request-scoped-errors:
  codex:
    - anything: 1
plugins:
  configs:
    my-plugin:
      list:
        - anything: 1
`,
		},
		{
			name: "mixed legacy and v8",
			doc: `config-version: 8
debug: true
server:
  port: 8317
tls:
  enable: false
  kye: x
oauth:
  providers:
    codex:
      disable-codex-cloaking: true
      live-media-relay:
        allow-private-remote-ips: true
codex:
  live-media-relay:
    allow-private-remote-ips: true
upstream:
  claude:
    header-defaults:
      user-agnet: x
`,
			want: []string{
				unknownKeyWarning("tls.kye", 7),
				unknownKeyWarning("oauth.providers.codex.live-media-relay.allow-private-remote-ips", 13),
				unknownKeyWarning("upstream.claude.header-defaults.user-agnet", 20),
			},
		},
		{
			name: "anchors aliases and merge keys",
			doc: `tls: &tls
  enable: false
  certt: a.pem
server:
  tls:
    <<: *tls
    key: b.pem
  bogus: *tls
routing:
  <<: {strategy: fill-first, stratgy: x}
`,
			want: []string{
				unknownKeyWarning("tls.certt", 3),
				unknownKeyWarning("server.tls.certt", 3),
				unknownKeyWarning("server.bogus", 8),
				unknownKeyWarning("routing.stratgy", 10),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Every document loads: the warning only reports what loading ignores.
			if _, err := ParseConfigBytes([]byte(tc.doc)); err != nil {
				t.Fatalf("document does not load: %v", err)
			}
			if got := unknownConfigKeys([]byte(tc.doc)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("unknownConfigKeys() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnknownConfigKeysMasksClientKeys(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		raw  []string
		want []string
	}{
		{
			name: "client key as an unknown mapping key",
			doc:  "api-keys:\n  - sk-live-0123456789\napi-key-names:\n  sk.dotted.client.key: phone\naccess:\n  sk-live-0123456789: true\nsk.dotted.client.key: 1\n",
			raw:  []string{"sk-live-0123456789", "sk.dotted.client.key"},
			want: []string{unknownKeyWarning("access.sk-l...6789", 6), unknownKeyWarning("sk.d....key", 7)},
		},
		{
			name: "client key as an unknown key in a list item",
			doc:  "api-keys:\n  - sk-live-0123456789\nclaude-api-key:\n  - api-key: upstream-key\n    sk-live-0123456789: true\n",
			raw:  []string{"sk-live-0123456789"},
			want: []string{unknownKeyWarning("claude-api-key[0].sk-l...6789", 5)},
		},
		{
			name: "client keys written as an api-keys mapping",
			doc:  "api-keys:\n  sk-unlisted-secret-42: laptop\n",
			raw:  []string{"sk-unlisted-secret-42"},
			want: []string{unknownKeyWarning("api-keys.sk-u...t-42", 2)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseConfigBytes([]byte(tc.doc)); err != nil {
				t.Fatalf("document does not load: %v", err)
			}
			got := unknownConfigKeys([]byte(tc.doc))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("unknownConfigKeys() = %q, want %q", got, tc.want)
			}
			for _, finding := range got {
				for _, raw := range tc.raw {
					if strings.Contains(finding, raw) {
						t.Fatalf("finding %q prints the client key", finding)
					}
				}
			}
		})
	}
}

func TestUnknownConfigKeysExampleConfig(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := unknownConfigKeys(example); len(got) != 0 {
		t.Fatalf("config.example.yaml has unknown keys: %q", got)
	}
	// The commented API-key examples, uncommented the way operators use them.
	text := strings.ReplaceAll(string(example), "\r\n", "\n")
	_, block, _ := strings.Cut(text, "# BEGIN API KEY EXAMPLES\n")
	block, _, found := strings.Cut(block, "# END API KEY EXAMPLES")
	if !found {
		t.Fatal("config.example.yaml is missing the API-key examples")
	}
	var uncommented strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(block, "\n"), "\n") {
		if line != "" {
			uncommented.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "#"), " ") + "\n")
		}
	}
	full := []byte(text + "\n" + uncommented.String())
	if got := unknownConfigKeys(full); len(got) != 0 {
		t.Fatalf("config.example.yaml API-key examples have unknown keys: %q", got)
	}
	// The same document written in the v8 layout, with its provider groups.
	migrated, _, err := NormalizeConfigLayout(full, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(migrated), "keys:") {
		t.Fatal("migrated example has no v8 provider groups")
	}
	if got := unknownConfigKeys(migrated); len(got) != 0 {
		t.Fatalf("config.example.yaml migrated to v8 has unknown keys: %q", got)
	}
}

func TestLoadConfigWarnsAboutUnknownKeys(t *testing.T) {
	previous := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	defer log.StandardLogger().ReplaceHooks(previous)
	hook := logtest.NewGlobal()

	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("port: 8317\napi-keys:\n  - sk-client-0001\napi-keyz:\n  - sk-client-0002\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8317 || !reflect.DeepEqual(cfg.APIKeys, []string{"sk-client-0001"}) {
		t.Fatalf("load changed: port=%d api-keys=%q", cfg.Port, cfg.APIKeys)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, raw) {
		t.Fatalf("config file was modified:\n%s", after)
	}
	var warnings []string
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, "unknown key") {
			if entry.Level != log.WarnLevel {
				t.Fatalf("level = %v, want warn", entry.Level)
			}
			warnings = append(warnings, entry.Message)
		}
	}
	if want := []string{unknownKeyWarning("api-keyz", 4)}; !reflect.DeepEqual(warnings, want) {
		t.Fatalf("warnings = %q, want %q", warnings, want)
	}
}
