package auth

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// SetRetryConfig updates additional credential retry rounds, the per-round credential limit, and the cooldown wait interval.
func (m *Manager) SetRetryConfig(retry int, maxRetryInterval time.Duration, maxRetryCredentials int) {
	if m == nil {
		return
	}
	if retry < 0 {
		retry = 0
	}
	if maxRetryCredentials < 0 {
		maxRetryCredentials = 0
	}
	if maxRetryInterval < 0 {
		maxRetryInterval = 0
	}
	m.requestRetry.Store(int32(retry))
	m.maxRetryCredentials.Store(int32(maxRetryCredentials))
	m.maxRetryInterval.Store(maxRetryInterval.Nanoseconds())
}

// RegisterExecutor registers a provider executor with the manager.
func (m *Manager) RegisterExecutor(executor ProviderExecutor) {
	if executor == nil {
		return
	}
	provider := strings.TrimSpace(executor.Identifier())
	if provider == "" {
		return
	}

	var replaced ProviderExecutor
	var toReschedule []string
	m.mu.Lock()
	replaced = m.executors[provider]
	m.executors[provider] = executor
	for id, auth := range m.auths {
		if auth != nil && strings.EqualFold(executorKeyFromAuth(auth), provider) {
			toReschedule = append(toReschedule, id)
		}
	}
	m.mu.Unlock()

	for _, id := range toReschedule {
		m.queueRefreshReschedule(id)
	}

	if replaced == nil || replaced == executor {
		return
	}
	if closer, ok := replaced.(ExecutionSessionCloser); ok && closer != nil {
		closer.CloseExecutionSession(CloseAllExecutionSessionsID)
	}
}

// UnregisterExecutor removes the executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	m.mu.Lock()
	delete(m.executors, provider)
	m.mu.Unlock()
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	NormalizeCredentialMetadata(auth.Metadata)
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return nil, fmt.Errorf("register auth: %w", errWeight)
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	now := time.Now()
	if auth.Generation == 0 {
		auth.Generation = 1
	}
	if auth.CreatedAt.IsZero() {
		auth.CreatedAt = now
	}
	auth.UpdatedAt = now
	cooldownStateChanged := normalizeModelStates(auth)
	if m.cooldownDisabledForAuth(auth) || auth.Disabled || auth.Status == StatusDisabled {
		cooldownStateChanged = clearCooldownStateForAuth(auth, now) || cooldownStateChanged
	}
	auth.EnsureIndex()
	m.mu.Lock()
	if m.authEpochs == nil {
		m.authEpochs = make(map[string]uint64)
	}
	existing := m.auths[auth.ID]
	if existing != nil && existing.RegistrationEpoch > m.authEpochs[auth.ID] {
		m.authEpochs[auth.ID] = existing.RegistrationEpoch
	}
	auth.quotaLineage = replacedQuotaLineage(existing, auth)
	if auth.RegistrationEpoch > m.authEpochs[auth.ID] {
		m.authEpochs[auth.ID] = auth.RegistrationEpoch
	}
	m.authEpochs[auth.ID]++
	auth.RegistrationEpoch = m.authEpochs[auth.ID]
	auth.Generation = 1
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	m.notifyAuthChangeLocked(auth.ID)
	// Snapshot before unlocking: MarkResult mutates the published auth in place.
	var schedulerSnapshot *Auth
	if m.scheduler != nil {
		schedulerSnapshot = authClone.Clone()
	}
	m.mu.Unlock()
	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(schedulerSnapshot)
	}
	m.structuralEpoch.Add(1)
	m.queueRefreshReschedule(auth.ID)
	// Persist failures stay non-fatal, but must not be silent: a restart would
	// lose the credential that only exists in memory.
	if errPersist := m.persist(ctx, auth); errPersist != nil {
		log.WithFields(log.Fields{"auth_id": auth.ID, "credential": auth.ID, "provider": auth.Provider}).Warnf("failed to persist registered auth %s (%s): %v", auth.Provider, auth.ID, errPersist)
	}
	m.hook.OnAuthRegistered(ctx, auth.Clone())
	if cooldownStateChanged {
		m.persistCooldownStates(context.Background())
	}
	return auth.Clone(), nil
}

type updateAuthMode int

const (
	updateModeReplace updateAuthMode = iota
	updateModeRefresh
	updateModePrepare
)

// UpdatePreparedAuth atomically merges request preparation results into the latest runtime auth
// under the manager lock, preserving concurrent modifications without modifying refresh lifecycle fields.
func (m *Manager) UpdatePreparedAuth(ctx context.Context, base, updated *Auth) (*Auth, error) {
	return m.updateInternal(ctx, base, updated, updateModePrepare)
}

// UpdateRefreshedAuth atomically merges refresh results into the latest runtime auth
// under the manager lock, preserving concurrent modifications (proxy_url, notes, weights, etc.).
func (m *Manager) UpdateRefreshedAuth(ctx context.Context, base, updated *Auth) (*Auth, error) {
	return m.updateInternal(ctx, base, updated, updateModeRefresh)
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	return m.updateInternal(ctx, nil, auth, updateModeReplace)
}

func (m *Manager) updateInternal(ctx context.Context, base, auth *Auth, mode updateAuthMode) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	NormalizeCredentialMetadata(auth.Metadata)
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return nil, fmt.Errorf("update auth: %w", errWeight)
	}
	m.mu.Lock()
	existing, ok := m.auths[auth.ID]
	if !ok || existing == nil {
		m.mu.Unlock()
		return nil, nil
	}
	if m.authEpochs == nil {
		m.authEpochs = make(map[string]uint64)
	}
	if existing.RegistrationEpoch > m.authEpochs[auth.ID] {
		m.authEpochs[auth.ID] = existing.RegistrationEpoch
	}
	if (mode == updateModeRefresh || mode == updateModePrepare) && base != nil && existing.RegistrationEpoch != base.RegistrationEpoch {
		m.mu.Unlock()
		return nil, fmt.Errorf("update auth %s: stale registration epoch %d != %d", auth.ID, base.RegistrationEpoch, existing.RegistrationEpoch)
	}
	if mode == updateModeRefresh {
		merged := MergeRefreshedAuth(base, existing, auth)
		if merged != nil {
			auth = merged
			NormalizeCredentialMetadata(auth.Metadata)
		}
	} else if mode == updateModePrepare {
		merged := MergePreparedAuth(base, existing, auth)
		if merged != nil {
			auth = merged
			NormalizeCredentialMetadata(auth.Metadata)
		}
	}
	if auth.RegistrationEpoch != 0 && auth.RegistrationEpoch < m.authEpochs[auth.ID] {
		m.mu.Unlock()
		return nil, fmt.Errorf("update auth %s: stale registration epoch %d < %d", auth.ID, auth.RegistrationEpoch, m.authEpochs[auth.ID])
	}
	if auth.RegistrationEpoch >= m.authEpochs[auth.ID] {
		m.authEpochs[auth.ID] = auth.RegistrationEpoch
	} else if auth.RegistrationEpoch == 0 {
		auth.RegistrationEpoch = m.authEpochs[auth.ID]
	}
	if !auth.indexAssigned && auth.Index == "" {
		auth.Index = existing.Index
		auth.indexAssigned = existing.indexAssigned
	}
	auth.Success = existing.Success
	auth.Failed = existing.Failed
	auth.recentRequests = existing.recentRequests
	if auth.Generation <= existing.Generation {
		auth.Generation = existing.Generation + 1
	} else {
		auth.Generation++
	}
	if mode == updateModeRefresh || mode == updateModePrepare {
		// The provider rotated the credentials of the same grant, or the proxy added
		// metadata: quota data taken for existing still describes the account. A replace
		// that changed the lineage while base was refreshed or prepared mixes two accounts'
		// data, so the merge starts a new lineage.
		if base == nil || base.quotaLineage == existing.quotaLineage {
			auth.quotaLineage = keptQuotaLineage(existing)
		} else {
			auth.quotaLineage = nextQuotaLineage()
		}
	} else if sameQuotaAccountByIdentity(existing, auth) {
		// A replace usually rebuilds the auth from its file, which carries no quota snapshot,
		// and every token refresh rewrites that file. Keep the newer snapshot and the quota
		// lineage while the account stays the same. The decision ignores auth's own lineage:
		// auth may be a clone of existing whose account was edited. The credential_quota
		// carry-over below copies only cooldown fields.
		auth.Quota = mergeQuotaObservation(auth.Quota, existing.Quota)
		auth.quotaLineage = keptQuotaLineage(existing)
	} else {
		// auth may be a clone of existing, with its snapshot, whose account was edited (a
		// management field patch): that snapshot describes existing's account. Drop it, or
		// the next reload of the edited file, whose credentials match, would keep it.
		auth.Quota = withoutQuotaObservation(auth.Quota)
		auth.quotaLineage = nextQuotaLineage()
	}
	cooldownStateChanged := false
	if !existing.Disabled && existing.Status != StatusDisabled && !auth.Disabled && auth.Status != StatusDisabled {
		if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
			auth.ModelStates = existing.ModelStates
		}
		credChanged := CredentialsChanged(existing, auth)
		if credChanged {
			if hasUnauthorizedAuthFailure(existing) || (auth.LastError != nil && isUnauthorizedError(auth.LastError)) {
				auth.Unavailable = false
				auth.LastError = nil
				auth.StatusMessage = ""
				auth.Status = StatusActive
			}
			resumed := clearUnauthorizedModelStates(auth, time.Now())
			if len(resumed) > 0 {
				cooldownStateChanged = true
			}
		}
		if existing.Quota.Exceeded && existing.Quota.Reason == "credential_quota" && existing.Quota.NextRecoverAt.After(time.Now()) {
			auth.Unavailable = existing.Unavailable
			auth.NextRetryAfter = existing.NextRetryAfter
			applyCooldownFields(&auth.Quota, existing.Quota)
			if auth.Status == StatusActive {
				auth.Status = existing.Status
			}
		}
	}
	now := time.Now()
	auth.UpdatedAt = now
	cooldownStateChanged = normalizeModelStates(auth) || cooldownStateChanged
	if m.cooldownDisabledForAuth(auth) || auth.Disabled || auth.Status == StatusDisabled {
		cooldownStateChanged = clearCooldownStateForAuth(auth, now) || cooldownStateChanged
	}
	auth.EnsureIndex()
	// A minted Meta key must reach the configured store before requests can use it.
	// Keep the epoch check, save and installation together so a concurrent reload
	// or removal cannot let an obsolete mint overwrite the credential on disk.
	persistMetaMint := (mode == updateModePrepare || mode == updateModeRefresh) && strings.EqualFold(strings.TrimSpace(auth.Provider), "meta")
	if persistMetaMint {
		if errPersist := m.persist(ctx, auth); errPersist != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("persist meta auth: %w", errPersist)
		}
	}
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	m.notifyAuthChangeLocked(auth.ID)
	// Snapshot before unlocking: MarkResult mutates the published auth in place.
	var schedulerSnapshot *Auth
	if m.scheduler != nil {
		schedulerSnapshot = authClone.Clone()
	}
	m.mu.Unlock()
	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(schedulerSnapshot)
	}
	m.structuralEpoch.Add(1)
	m.queueRefreshReschedule(auth.ID)
	if !persistMetaMint {
		// Persist failures stay non-fatal, but must not be silent: after a token
		// refresh the rotated credentials only exist in memory until persisted.
		if errPersist := m.persist(ctx, auth); errPersist != nil {
			log.WithFields(log.Fields{"auth_id": auth.ID, "credential": auth.ID, "provider": auth.Provider}).Warnf("failed to persist updated auth %s (%s): %v", auth.Provider, auth.ID, errPersist)
		}
	}
	m.hook.OnAuthUpdated(ctx, auth.Clone())
	if cooldownStateChanged {
		m.persistCooldownStates(context.Background())
	}
	return auth.Clone(), nil
}

// Remove deletes an auth from runtime state without persisting.
// Disk and token-store deletion must be handled by the caller.
func (m *Manager) Remove(ctx context.Context, id string) {
	if m == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	_ = ctx

	m.mu.Lock()
	existing := m.auths[id]
	if existing == nil {
		m.mu.Unlock()
		return
	}
	provider := strings.TrimSpace(existing.Provider)
	delete(m.auths, id)
	m.notifyAuthChangeLocked(id)
	if m.modelPoolOffsets != nil {
		delete(m.modelPoolOffsets, id)
	}
	for sessionID, sessionAuths := range m.homeRuntimeAuths {
		if sessionAuths == nil {
			continue
		}
		delete(sessionAuths, id)
		if len(sessionAuths) == 0 {
			delete(m.homeRuntimeAuths, sessionID)
		}
	}
	if m.authEpochs == nil {
		m.authEpochs = make(map[string]uint64)
	}
	if existing.RegistrationEpoch > m.authEpochs[id] {
		m.authEpochs[id] = existing.RegistrationEpoch
	}
	m.authEpochs[id]++
	tombstoneEpoch := m.authEpochs[id]
	m.mu.Unlock()

	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.RecordRemovalTombstone(id, tombstoneEpoch)
	}
	m.structuralEpoch.Add(1)
	m.queueRefreshUnschedule(id)
	m.invalidateSessionAffinity(id)

	if provider != "" {
		if exec, ok := m.Executor(provider); ok && exec != nil {
			if closer, okCloser := exec.(ExecutionSessionCloser); okCloser {
				closer.CloseExecutionSession(CloseAllExecutionSessionsID)
			}
		}
	}
	m.persistCooldownStates(context.Background())
}

func (m *Manager) invalidateSessionAffinity(authID string) {
	if m == nil || authID == "" {
		return
	}
	sel := m.Selector()
	if invalidator, ok := sel.(interface{ InvalidateAuth(string) }); ok && invalidator != nil {
		invalidator.InvalidateAuth(authID)
	}
}

// Load resets manager state from the backing store.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	if m.store == nil {
		m.mu.Unlock()
		return nil
	}
	items, err := m.store.List(ctx)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	previousAuths := m.auths
	m.auths = make(map[string]*Auth, len(items))
	if m.authEpochs == nil {
		m.authEpochs = make(map[string]uint64, len(items))
	}
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		NormalizeCredentialMetadata(auth.Metadata)
		if errWeight := ValidateAuthWeight(auth); errWeight != nil {
			continue
		}
		auth.EnsureIndex()
		m.authEpochs[auth.ID] = max(m.authEpochs[auth.ID], auth.RegistrationEpoch) + 1
		auth.RegistrationEpoch = m.authEpochs[auth.ID]
		auth.Generation = 1
		auth.quotaLineage = replacedQuotaLineage(previousAuths[auth.ID], auth)
		m.auths[auth.ID] = auth.Clone()
	}

	type removalTombstone struct {
		id    string
		epoch uint64
	}
	var removedTombstones []removalTombstone
	for prevID := range previousAuths {
		if _, exists := m.auths[prevID]; !exists {
			m.authEpochs[prevID]++
			removedTombstones = append(removedTombstones, removalTombstone{
				id:    prevID,
				epoch: m.authEpochs[prevID],
			})
		}
	}

	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	for id := range m.authChangeWatchers {
		m.notifyAuthChangeLocked(id)
	}
	m.mu.Unlock()

	if m.scheduler != nil {
		for _, rt := range removedTombstones {
			m.scheduler.RecordRemovalTombstone(rt.id, rt.epoch)
		}
	}
	m.structuralEpoch.Add(1)
	m.syncScheduler()
	return nil
}

type authPersistLock struct {
	mu             sync.Mutex
	lastEpoch      uint64
	lastGeneration uint64
}

func (m *Manager) persist(ctx context.Context, auth *Auth) error {
	if m.store == nil || auth == nil {
		return nil
	}
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return fmt.Errorf("persist auth: %w", errWeight)
	}
	if IsConfigAPIKeyAuth(auth) {
		return nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	if IsPluginVirtualAuth(auth) {
		return nil
	}
	// Skip persistence when metadata is absent (e.g., runtime-only auths).
	if auth.Metadata == nil {
		return nil
	}

	lockVal, _ := m.persistLocks.LoadOrStore(auth.ID, &authPersistLock{})
	pLock, _ := lockVal.(*authPersistLock)
	if pLock != nil {
		pLock.mu.Lock()
		defer pLock.mu.Unlock()
		if auth.RegistrationEpoch < pLock.lastEpoch || (auth.RegistrationEpoch == pLock.lastEpoch && auth.Generation < pLock.lastGeneration) {
			return nil
		}
		pLock.lastEpoch = auth.RegistrationEpoch
		pLock.lastGeneration = auth.Generation
		if shouldSkipPersist(ctx) {
			return nil
		}
		_, err := m.store.Save(ctx, auth)
		return err
	}

	if shouldSkipPersist(ctx) {
		return nil
	}
	_, err := m.store.Save(ctx, auth)
	return err
}
