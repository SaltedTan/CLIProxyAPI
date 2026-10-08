package cliproxy

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/keyusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

func (s *Service) applyConfigUpdate(newCfg *config.Config) {
	s.applyConfigUpdateWithAuthSynthesis(context.Background(), newCfg, true)
}

func (s *Service) applyWatcherConfigUpdate(newCfg *config.Config) {
	s.applyConfigUpdateWithAuthSynthesis(context.Background(), newCfg, false)
}

type configCommit struct {
	cfg      *config.Config
	sequence uint64
}

type routingRuntimeState struct {
	strategy                 string
	sessionAffinity          bool
	sessionAffinityTTL       time.Duration
	sessionAffinitySubagents bool
}

func normalizedRoutingRuntimeState(cfg *config.Config) routingRuntimeState {
	state := routingRuntimeState{
		strategy:                 "round-robin",
		sessionAffinityTTL:       time.Hour,
		sessionAffinitySubagents: true,
	}
	if cfg == nil {
		return state
	}

	switch strings.ToLower(strings.TrimSpace(cfg.Routing.Strategy)) {
	case "weighted-round-robin", "weightedroundrobin", "wrr":
		state.strategy = "weighted-round-robin"
	case "fill-first", "fillfirst", "ff":
		state.strategy = "fill-first"
	case "quota-aware", "quotaaware", "qa", "reset-priority":
		state.strategy = "quota-aware"
	}
	state.sessionAffinity = cfg.Routing.SessionAffinity
	if ttl := strings.TrimSpace(cfg.Routing.SessionAffinityTTL); ttl != "" {
		if parsed, errParse := time.ParseDuration(ttl); errParse == nil && parsed > 0 {
			if parsed < time.Second {
				parsed = time.Second
			}
			state.sessionAffinityTTL = parsed
		}
	}
	if state.sessionAffinity && cfg.Routing.SessionAffinitySubagents != nil {
		state.sessionAffinitySubagents = *cfg.Routing.SessionAffinitySubagents
	}
	return state
}

// quotaAwareActive reports whether the current configuration routes with quota-aware.
func (s *Service) quotaAwareActive() bool {
	return normalizedRoutingRuntimeState(s.currentConfig()).strategy == "quota-aware"
}

// quotaSources lists the readings quota-aware routing consults besides each
// credential's quota snapshot, in tie-break order. The client usage tracker keeps each
// Claude credential's last weekly reading across restarts, so credentials are ranked
// before their next response; the usage cache reads the Claude OAuth usage endpoint,
// which also counts usage made outside the proxy.
func quotaSources(usage *keyusage.UsageCache) []coreauth.QuotaSource {
	sources := []coreauth.QuotaSource{clientusage.Default().ClaudeQuota}
	if usage != nil {
		sources = append(sources, usage.QuotaReading)
	}
	return sources
}

func newRoutingSelector(state routingRuntimeState, usage *keyusage.UsageCache) coreauth.Selector {
	var selector coreauth.Selector
	switch state.strategy {
	case "weighted-round-robin":
		selector = &coreauth.WeightedRoundRobinSelector{}
	case "fill-first":
		selector = &coreauth.FillFirstSelector{}
	case "quota-aware":
		// Round-robin rotates equally urgent credentials and pools without quota data.
		quotaAware := coreauth.NewQuotaAwareSelector(&coreauth.RoundRobinSelector{})
		quotaAware.SetQuotaSources(quotaSources(usage)...)
		selector = quotaAware
	default:
		selector = &coreauth.RoundRobinSelector{}
	}
	if state.sessionAffinity {
		subagents := state.sessionAffinitySubagents
		selector = coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
			Fallback:         selector,
			TTL:              state.sessionAffinityTTL,
			SubagentAffinity: &subagents,
		})
	}
	return selector
}

func (s *Service) applyConfigUpdateWithAuthSynthesis(ctx context.Context, newCfg *config.Config, synthesizeConfigAuths bool) bool {
	commit := s.commitConfigUpdate(newCfg)
	if commit.cfg == nil {
		return false
	}
	return s.applyConfigRuntime(ctx, commit, synthesizeConfigAuths)
}

// commitConfigUpdate applies only in-memory configuration state. Runtime work that
// may block on plugins, models, storage, or networking is deliberately deferred.
func (s *Service) commitConfigUpdate(newCfg *config.Config) configCommit {
	if s == nil {
		return configCommit{}
	}

	s.configUpdateMu.Lock()
	defer s.configUpdateMu.Unlock()

	if newCfg == nil {
		s.cfgMu.RLock()
		newCfg = s.cfg
		s.cfgMu.RUnlock()
	}
	if newCfg == nil {
		return configCommit{}
	}
	if errValidate := newCfg.ValidateCredentialWeights(); errValidate != nil {
		log.WithError(errValidate).Warn("rejected config update with invalid credential weights")
		return configCommit{}
	}

	s.cfgMu.Lock()
	s.cfg = newCfg
	s.cfgMu.Unlock()
	s.cancelStaleAntigravityProbes("")
	s.configSequence++
	return configCommit{cfg: newCfg, sequence: s.configSequence}
}

// currentConfig returns the configuration in effect.
func (s *Service) currentConfig() *config.Config {
	if s == nil {
		return nil
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Service) configCommitCurrent(commit configCommit) bool {
	if s == nil || commit.sequence == 0 {
		return false
	}
	s.configUpdateMu.Lock()
	current := s.configSequence == commit.sequence
	s.configUpdateMu.Unlock()
	return current
}

func (s *Service) applyConfigRuntime(ctx context.Context, commit configCommit, synthesizeConfigAuths bool) bool {
	cfg := commit.cfg
	if s == nil || cfg == nil {
		return false
	}
	s.configRuntimeMu.Lock()
	defer s.configRuntimeMu.Unlock()
	if !s.configCommitCurrent(commit) {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}

	if !s.applyManagerConfig(ctx, commit) {
		return false
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	if !s.applyPprofConfigContext(ctx, cfg) {
		return false
	}
	s.applyDiscoveryConfigContext(ctx, cfg)
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	if !s.updateServerClientsContext(ctx, cfg) {
		return false
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}

	registrationCtx := coreauth.WithSkipPersist(ctx)
	s.syncPluginRuntimeConfigForConfig(registrationCtx, cfg)
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	var auths []*coreauth.Auth
	if s.coreManager != nil {
		auths = s.coreManager.List()
	}
	s.registerAvailableExecutors(registrationCtx, executorRegistrationOptions{
		includeBaseline:   cfg.Home.Enabled,
		forceReplaceAuths: true,
		auths:             auths,
	})
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	if synthesizeConfigAuths {
		s.registerConfigAPIKeyAuths(registrationCtx, cfg)
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	if s.coreManager != nil && !cfg.Home.Enabled && cfg.SaveCooldownStatus {
		if errRestoreCooldown := s.coreManager.RestoreCooldownStates(registrationCtx); errRestoreCooldown != nil && ctx.Err() == nil {
			log.Warnf("failed to restore cooldown state after config update: %v", errRestoreCooldown)
		}
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	s.syncPluginModelRuntime(registrationCtx)
	return ctx.Err() == nil
}

func (s *Service) applyManagerConfig(ctx context.Context, commit configCommit) bool {
	if s == nil || s.coreManager == nil || commit.cfg == nil {
		return s != nil && commit.cfg != nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	routingState := normalizedRoutingRuntimeState(commit.cfg)
	if s.appliedRoutingState == nil || *s.appliedRoutingState != routingState {
		s.replaceRoutingSelector(newRoutingSelector(routingState, s.claudeUsage), time.Now())
		s.appliedRoutingState = &routingState
	}
	s.applyRetryConfig(commit.cfg)
	store := s.resolveCooldownStateStore(commit.cfg)
	if !s.coreManager.ApplyConfigWithCooldownStateStore(ctx, commit.cfg, store) {
		return false
	}
	s.coreManager.SetOAuthModelAlias(commit.cfg.OAuthModelAlias)
	return true
}

// replaceRoutingSelector installs next for a routing settings change. When both the
// current and the next selector are session affinity selectors, the session bindings
// are carried over, so live sessions keep their credential (and its prompt cache).
// Turning session affinity off drops them.
func (s *Service) replaceRoutingSelector(next coreauth.Selector, now time.Time) {
	current, _ := s.coreManager.Selector().(*coreauth.SessionAffinitySelector)
	nextAffinity, _ := next.(*coreauth.SessionAffinitySelector)
	var bindings []coreauth.SessionBinding
	if current != nil && nextAffinity != nil {
		// Snapshot before the swap, which stops the current selector. A pick that lands
		// on it between the snapshot and the swap is not carried over; that session is
		// re-picked on its next request.
		bindings = current.SessionBindings(now)
	}
	s.coreManager.SetSelector(next)
	if len(bindings) == 0 {
		return
	}
	// Restore after the swap: bindings made on the new selector in between are live and
	// win, and expiries are capped at the new TTL.
	kept := nextAffinity.RestoreSessionBindings(bindings, now, usableBindingAuth(s.coreManager))
	log.Infof("session affinity: kept %d of %d bindings across a routing change", kept, len(bindings))
}

// ensureRoutingSelector installs the configured routing selector when none has been
// applied yet (a manager supplied through WithCoreAuthManager), as the first config
// commit would, so state restored into the selector is not dropped by that commit.
func (s *Service) ensureRoutingSelector() {
	if s == nil || s.coreManager == nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil {
		return
	}
	s.configRuntimeMu.Lock()
	defer s.configRuntimeMu.Unlock()
	if s.appliedRoutingState != nil {
		return
	}
	routingState := normalizedRoutingRuntimeState(cfg)
	s.coreManager.SetSelector(newRoutingSelector(routingState, s.claudeUsage))
	s.appliedRoutingState = &routingState
}

func (s *Service) updateServerClientsContext(ctx context.Context, cfg *config.Config) bool {
	if s == nil || cfg == nil || (ctx != nil && ctx.Err() != nil) {
		return false
	}
	if s.updateServerClientsContextFn != nil {
		return s.updateServerClientsContextFn(ctx, cfg)
	}
	if s.server == nil {
		return true
	}
	return s.server.UpdateClientsContext(ctx, cfg)
}

func (s *Service) reloadConfigFromWatcher() bool {
	if s == nil || s.watcher == nil {
		return false
	}
	return s.watcher.ReloadConfigIfChanged()
}

func (s *Service) registerConfigAPIKeyAuths(ctx context.Context, cfg *config.Config) {
	if s == nil || s.coreManager == nil || cfg == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	configSynth := synthesizer.NewConfigSynthesizer()
	auths, errSynthesize := configSynth.Synthesize(&synthesizer.SynthesisContext{
		Config:      cfg,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		log.Warnf("failed to synthesize config API key auths: %v", errSynthesize)
		return
	}

	registrationCtx := coreauth.WithDeferredAPIKeyModelAliasRebuild(ctx)
	tasks := make([]modelRegistrationTask, 0, len(auths))
	needsAliasRebuild := false
	for _, auth := range auths {
		if !coreauth.IsConfigAPIKeyAuth(auth) {
			continue
		}
		prepared := s.prepareCoreAuthForModelRegistration(registrationCtx, auth)
		if prepared == nil {
			continue
		}
		needsAliasRebuild = true
		authForRegistration := prepared
		tasks = append(tasks, modelRegistrationTask{
			phase:    modelRegistrationPhaseConfigAPIKey,
			category: modelRegistrationCategory(authForRegistration),
			run: func(compatCache *openAICompatibilityRegistrationCache) {
				s.completeModelRegistrationForAuthWithCache(registrationCtx, authForRegistration, compatCache)
			},
		})
	}
	if needsAliasRebuild {
		s.coreManager.RefreshAPIKeyModelAlias()
	}
	s.runModelRegistrationTasks(registrationCtx, tasks)
}

func forceHomeRuntimeConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	cfg.APIKeys = nil
	cfg.UsageStatisticsEnabled = true
	cfg.DisableCooling = true
	cfg.SaveCooldownStatus = false
	cfg.WebsocketAuth = false
	cfg.RemoteManagement.AllowRemote = false
	cfg.RemoteManagement.DisableControlPanel = true
	cfg.Plugins.StoreAuth = nil
}
