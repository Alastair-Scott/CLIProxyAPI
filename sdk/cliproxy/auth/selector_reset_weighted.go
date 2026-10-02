package auth

import (
	"context"
	"maps"
	"math"
	"strconv"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ResetWeightedSelector shares new sessions within the highest available priority
// tier, favouring Codex accounts whose next observed quota reset is sooner.
// SessionAffinitySelector remains responsible for keeping established sessions sticky.
type ResetWeightedSelector struct {
	weighted WeightedRoundRobinSelector
}

func (s *ResetWeightedSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	return s.pickAt(ctx, provider, model, opts, auths, time.Now())
}

func (s *ResetWeightedSelector) pickAt(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth, now time.Time) (*Auth, error) {
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	resets := make(map[string]time.Duration, len(available))
	var soonest time.Duration
	for _, auth := range available {
		if remaining := codexResetRemaining(auth, model, now); remaining > 0 {
			resets[auth.ID] = remaining
			if soonest == 0 || remaining < soonest {
				soonest = remaining
			}
		}
	}
	weighted := make([]*Auth, 0, len(available))
	for _, auth := range available {
		weight := 1
		if remaining := resets[auth.ID]; remaining > 0 {
			weight = max(1, int(math.Round(8*float64(soonest)/float64(remaining))))
		}
		// Only the scheduler's temporary view receives dynamic weights. Never
		// persist them or mutate shared credential metadata.
		candidate := *auth
		candidate.Attributes = maps.Clone(auth.Attributes)
		if candidate.Attributes == nil {
			candidate.Attributes = make(map[string]string)
		}
		candidate.Attributes[AttributeWeight] = strconv.Itoa(weight)
		weighted = append(weighted, &candidate)
	}
	picked, err := s.weighted.Pick(ctx, provider, model, opts, weighted)
	if err != nil {
		return nil, err
	}
	for _, auth := range available {
		if auth.ID == picked.ID {
			return auth, nil
		}
	}
	return nil, &Error{Code: "auth_unavailable", Message: "selected credential is no longer available"}
}

// Ignore stale/expired snapshots and unrelated providers. Missing data gets the
// baseline share so idle accounts can acquire fresh observations from real traffic.
func codexResetRemaining(auth *Auth, model string, now time.Time) time.Duration {
	if auth == nil || auth.Provider != "codex" {
		return 0
	}
	quota := auth.Quota
	if state := auth.ModelStates[canonicalModelKey(model)]; state != nil && state.Quota.ObservedAt.After(quota.ObservedAt) {
		quota = state.Quota
	}
	if quota.ObservedAt.IsZero() || quota.ObservedAt.After(now) || now.Sub(quota.ObservedAt) > 15*time.Minute {
		return 0
	}
	var soonest time.Duration
	for _, window := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + window + "-"
		used, err := strconv.ParseFloat(quota.Signals[prefix+"Used-Percent"], 64)
		if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used >= 100 {
			continue
		}
		var reset time.Time
		if seconds, err := strconv.ParseInt(quota.Signals[prefix+"Reset-At"], 10, 64); err == nil && seconds > 0 {
			reset = time.Unix(seconds, 0)
		} else if seconds, err := strconv.ParseInt(quota.Signals[prefix+"Reset-After-Seconds"], 10, 64); err == nil && seconds > 0 && seconds <= 30*24*60*60 {
			reset = quota.ObservedAt.Add(time.Duration(seconds) * time.Second)
		}
		if reset.After(now) && reset.Before(now.Add(30*24*time.Hour)) {
			remaining := max(time.Minute, reset.Sub(now))
			if soonest == 0 || remaining < soonest {
				soonest = remaining
			}
		}
	}
	return soonest
}
