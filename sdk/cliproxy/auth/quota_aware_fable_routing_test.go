package auth

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const testSonnetUpstream = "claude-sonnet-5-5"

// fableRoutingCase describes two Claude credentials picked through the manager for one
// route model. Their quota readings order them differently by window: "overall" has the
// most overall weekly quota at risk and "fable" the most Fable, so the pick shows which
// window ranked the request.
type fableRoutingCase struct {
	// credential builds a credential with the given ID; nil builds a Claude OAuth one.
	credential func(id string) *Auth
	// models are registered for both credentials.
	models []*registry.ModelInfo
	// aliases is the manager's OAuth model alias table.
	aliases map[string][]internalconfig.OAuthModelAlias
	// config builds the manager's runtime config for the credential IDs, for API key
	// model aliases; nil sets none.
	config func(ids ...string) *internalconfig.Config
	route  string
	// upstream is the model the executor must receive.
	upstream string
	// fable reports whether the request must be ranked by the Fable window.
	fable bool
}

// assertManagerFableRouting picks for the route model through the manager's local pick
// paths: SelectAuth (single provider) and Execute (mixed providers), with the quota-aware
// selector on its own and nested under session affinity.
func assertManagerFableRouting(t *testing.T, tc fableRoutingCase) {
	t.Helper()
	selectors := map[string]func(*QuotaAwareSelector) Selector{
		"quota-aware":                  func(quota *QuotaAwareSelector) Selector { return quota },
		"session-affinity/quota-aware": func(quota *QuotaAwareSelector) Selector { return NewSessionAffinitySelector(quota) },
	}
	for selectorName, newSelector := range selectors {
		for _, path := range []string{"select", "execute"} {
			t.Run(selectorName+"/"+path, func(t *testing.T) {
				ctx := context.Background()
				now := quotaAwareTestBase()
				overallID, fableID := "a-overall-"+t.Name(), "b-fable-"+t.Name()
				quota := newTestQuotaAwareSelector(now, nil)
				quota.SetQuotaSources(mapQuotaSource(map[string]QuotaReading{
					// 80% of the week left but only 10% of Fable.
					overallID: fableQuotaReading(now, 0.2, &QuotaWindowReading{Used: 0.9, ResetAt: now.Add(72 * time.Hour)}),
					// 40% of the week left and 90% of Fable.
					fableID: fableQuotaReading(now, 0.6, &QuotaWindowReading{Used: 0.1, ResetAt: now.Add(72 * time.Hour)}),
				}))
				selector := newSelector(quota)
				if affinity, ok := selector.(*SessionAffinitySelector); ok {
					t.Cleanup(affinity.Stop)
				}
				manager := NewManager(nil, selector, nil)
				manager.SetRetryConfig(0, 0, 0)
				if tc.config != nil {
					manager.SetConfig(tc.config(overallID, fableID))
				}
				manager.SetOAuthModelAlias(tc.aliases)
				for _, id := range []string{overallID, fableID} {
					credential := &Auth{ID: id, Provider: "claude", Status: StatusActive}
					if tc.credential != nil {
						credential = tc.credential(id)
					}
					registry.GetGlobalRegistry().RegisterClient(id, "claude", tc.models)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					if _, errRegister := manager.Register(ctx, credential); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				var sent []string
				manager.RegisterExecutor(&mockCustomErrorExecutor{
					identifier: "claude",
					executeFn: func(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
						sent = append(sent, req.Model)
						return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
					},
				})

				want, wantWindow := overallID, "the overall window"
				if tc.fable {
					want, wantWindow = fableID, "the Fable window"
				}
				for _, session := range []string{"session-1", "session-2"} {
					var got string
					switch path {
					case "select":
						selected, errSelect := manager.SelectAuth(ctx, "claude", tc.route, sessionOpts(session))
						if errSelect != nil {
							t.Fatalf("SelectAuth(%s) error = %v", tc.route, errSelect)
						}
						got = selected.ID
					default:
						response, errExecute := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: tc.route}, sessionOpts(session))
						if errExecute != nil {
							t.Fatalf("Execute(%s) error = %v", tc.route, errExecute)
						}
						got = string(response.Payload)
					}
					if got != want {
						t.Fatalf("%s: picked %s for %s, want %s (ranked by %s)", session, got, tc.route, want, wantWindow)
					}
				}
				for _, model := range sent {
					if model != tc.upstream {
						t.Fatalf("executor received %v, want %s", sent, tc.upstream)
					}
				}
			})
		}
	}
}

// Only the upstream models are registered, so the registry cannot say what an alias or a
// prefixed route model stands for; the manager's own model resolution must decide.
var fableRoutingUpstreamModels = []*registry.ModelInfo{{ID: testFableModel}, {ID: testSonnetUpstream}}

// An OAuth model alias whose name does not mention Fable but whose target is a Fable model
// is a Fable request.
func TestManagerQuotaAware_FableAliasWithoutFableInNameRanksByFableWindow(t *testing.T) {
	assertManagerFableRouting(t, fableRoutingCase{
		models:   fableRoutingUpstreamModels,
		aliases:  map[string][]internalconfig.OAuthModelAlias{"claude": {{Name: testFableModel, Alias: "big"}}},
		route:    "big",
		upstream: testFableModel,
		fable:    true,
	})
}

// An OAuth model alias whose name mentions Fable but whose target is not a Fable model is
// not a Fable request.
func TestManagerQuotaAware_NonFableAliasWithFableInNameRanksByOverallWindow(t *testing.T) {
	assertManagerFableRouting(t, fableRoutingCase{
		models:   fableRoutingUpstreamModels,
		aliases:  map[string][]internalconfig.OAuthModelAlias{"claude": {{Name: testSonnetUpstream, Alias: "fable-fast"}}},
		route:    "fable-fast",
		upstream: testSonnetUpstream,
	})
}

// A credential reached through its prefix counts by the model it is sent once the prefix
// is stripped and its alias applied, whatever the prefix is called.
func TestManagerQuotaAware_PrefixedCredentialRanksByUpstreamModel(t *testing.T) {
	aliases := map[string][]internalconfig.OAuthModelAlias{"claude": {{Name: testFableModel, Alias: "big"}}}
	prefixed := func(prefix string) func(id string) *Auth {
		return func(id string) *Auth {
			return &Auth{ID: id, Provider: "claude", Prefix: prefix, Status: StatusActive}
		}
	}
	t.Run("alias behind a prefix", func(t *testing.T) {
		assertManagerFableRouting(t, fableRoutingCase{
			credential: prefixed("team"),
			models:     fableRoutingUpstreamModels,
			aliases:    aliases,
			route:      "team/big",
			upstream:   testFableModel,
			fable:      true,
		})
	})
	t.Run("non-Fable model behind a prefix naming Fable", func(t *testing.T) {
		assertManagerFableRouting(t, fableRoutingCase{
			credential: prefixed("fable-team"),
			models:     fableRoutingUpstreamModels,
			aliases:    aliases,
			route:      "fable-team/" + testSonnetUpstream,
			upstream:   testSonnetUpstream,
		})
	})
}

// A Claude API key credential counts by the upstream model its configured alias sends.
func TestManagerQuotaAware_APIKeyAliasRanksByConfiguredUpstream(t *testing.T) {
	const baseURL = "https://claude.example.com"
	keyFor := func(id string) string { return "key-" + id }
	assertManagerFableRouting(t, fableRoutingCase{
		credential: func(id string) *Auth {
			return &Auth{ID: id, Provider: "claude", Status: StatusActive, Attributes: map[string]string{
				AttributeAPIKey: keyFor(id),
				"base_url":      baseURL,
			}}
		},
		// The registered alias carries no upstream model, so only the API key alias tells.
		models: []*registry.ModelInfo{{ID: "big"}},
		config: func(ids ...string) *internalconfig.Config {
			cfg := &internalconfig.Config{}
			for _, id := range ids {
				cfg.ClaudeKey = append(cfg.ClaudeKey, internalconfig.ClaudeKey{
					APIKey:  keyFor(id),
					BaseURL: baseURL,
					Models:  []internalconfig.ClaudeModel{{Name: testFableModel, Alias: "big"}},
				})
			}
			return cfg
		},
		route:    "big",
		upstream: testFableModel,
		fable:    true,
	})
}
