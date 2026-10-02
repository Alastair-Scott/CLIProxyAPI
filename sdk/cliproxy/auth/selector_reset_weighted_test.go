package auth

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestManagerResetWeightedDistribution(t *testing.T) {
	for _, sticky := range []bool{false, true} {
		t.Run(fmt.Sprint("affinity=", sticky), func(t *testing.T) {
			now := time.Now()
			var selector Selector = &ResetWeightedSelector{}
			if sticky {
				affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector, TTL: time.Hour})
				defer affinity.Stop()
				selector = affinity
			}
			manager := NewManager(nil, selector, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
			for _, auth := range []*Auth{
				resetTestAuth("reset-manager-soon", 10, now.Add(time.Hour), now),
				resetTestAuth("reset-manager-later", 10, now.Add(2*time.Hour), now),
				resetTestAuth("reset-manager-backup", 0, now.Add(time.Minute), now),
			} {
				if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "reset-model"}})
				defer registry.GetGlobalRegistry().UnregisterClient(auth.ID)
			}
			counts := map[string]int{}
			for i := range 120 {
				opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: fmt.Sprint("session-", i)}}
				picked, _, err := manager.pickNext(context.Background(), "codex", "reset-model", opts, nil)
				if err != nil {
					t.Fatal(err)
				}
				counts[picked.ID]++
			}
			if counts["reset-manager-soon"] != 80 || counts["reset-manager-later"] != 40 || counts["reset-manager-backup"] != 0 {
				t.Fatalf("manager split: %v", counts)
			}
		})
	}
}

func resetTestAuth(id string, priority int, reset time.Time, observed time.Time) *Auth {
	return &Auth{ID: id, Provider: "codex", Status: StatusActive,
		Attributes: map[string]string{"priority": strconv.Itoa(priority)},
		Quota: QuotaState{ObservedAt: observed, Signals: map[string]string{
			"X-Codex-Primary-Used-Percent": "30",
			"X-Codex-Primary-Reset-At":     strconv.FormatInt(reset.Unix(), 10),
		}}}
}

func TestResetWeightedDistributionAndPriority(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	soon := resetTestAuth("soon", 10, now.Add(time.Hour), now)
	later := resetTestAuth("later", 10, now.Add(2*time.Hour), now)
	backup := resetTestAuth("backup", 0, now.Add(time.Minute), now)
	selector := &ResetWeightedSelector{}
	counts := map[string]int{}
	for range 120 {
		picked, err := selector.pickAt(context.Background(), "codex", "model", cliproxyexecutor.Options{}, []*Auth{soon, later, backup}, now)
		if err != nil {
			t.Fatal(err)
		}
		counts[picked.ID]++
		if picked != soon && picked != later {
			t.Fatal("returned a temporary weighted copy or lower-priority auth")
		}
	}
	if counts["soon"] != 80 || counts["later"] != 40 || counts["backup"] != 0 {
		t.Fatalf("unexpected split: %v", counts)
	}
	if _, exists := soon.Attributes[AttributeWeight]; exists {
		t.Fatal("mutated original credential")
	}
	soon.Disabled, later.Disabled = true, true
	picked, err := selector.pickAt(context.Background(), "codex", "model", cliproxyexecutor.Options{}, []*Auth{soon, later, backup}, now)
	if err != nil || picked != backup {
		t.Fatalf("fallback = %v, %v", picked, err)
	}
}

func TestResetWeightedUnknownDataGetsFairShare(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	known := resetTestAuth("known", 0, now.Add(time.Hour), now)
	unknown := &Auth{ID: "unknown", Provider: "codex", Status: StatusActive}
	selector := &ResetWeightedSelector{}
	counts := map[string]int{}
	for range 90 {
		picked, err := selector.pickAt(context.Background(), "codex", "model", cliproxyexecutor.Options{}, []*Auth{known, unknown}, now)
		if err != nil {
			t.Fatal(err)
		}
		counts[picked.ID]++
	}
	if counts["known"] != 80 || counts["unknown"] != 10 {
		t.Fatalf("unexpected split: %v", counts)
	}
}

func TestCodexResetSnapshotValidation(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name   string
		change func(*Auth)
		want   time.Duration
	}{
		{"valid", func(a *Auth) {}, time.Hour},
		{"stale", func(a *Auth) { a.Quota.ObservedAt = now.Add(-16 * time.Minute) }, 0},
		{"future observation", func(a *Auth) { a.Quota.ObservedAt = now.Add(time.Second) }, 0},
		{"expired", func(a *Auth) {
			a.Quota.Signals["X-Codex-Primary-Reset-At"] = strconv.FormatInt(now.Add(-time.Second).Unix(), 10)
		}, 0},
		{"malformed", func(a *Auth) { a.Quota.Signals["X-Codex-Primary-Reset-At"] = "bad" }, 0},
		{"invalid percent", func(a *Auth) { a.Quota.Signals["X-Codex-Primary-Used-Percent"] = "NaN" }, 0},
		{"exhausted window", func(a *Auth) { a.Quota.Signals["X-Codex-Primary-Used-Percent"] = "100" }, 0},
		{"other provider", func(a *Auth) { a.Provider = "claude" }, 0},
		{"relative reset", func(a *Auth) {
			delete(a.Quota.Signals, "X-Codex-Primary-Reset-At")
			a.Quota.ObservedAt = now.Add(-time.Minute)
			a.Quota.Signals["X-Codex-Primary-Reset-After-Seconds"] = "3660"
		}, time.Hour},
		{"secondary sooner", func(a *Auth) {
			a.Quota.Signals["X-Codex-Secondary-Used-Percent"] = "10"
			a.Quota.Signals["X-Codex-Secondary-Reset-At"] = strconv.FormatInt(now.Add(30*time.Minute).Unix(), 10)
		}, 30 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := resetTestAuth("a", 0, now.Add(time.Hour), now)
			tc.change(auth)
			if got := codexResetRemaining(auth, "model", now); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResetWeightedAffinitySurvivesPriorityRecovery(t *testing.T) {
	now := time.Now()
	first := resetTestAuth("first", 10, now.Add(time.Hour), now)
	second := resetTestAuth("second", 10, now.Add(2*time.Hour), now)
	backup := resetTestAuth("backup", 0, now.Add(time.Minute), now)
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &ResetWeightedSelector{}, TTL: time.Hour})
	defer selector.Stop()
	all := []*Auth{first, second, backup}
	pick := func(session string) *Auth {
		t.Helper()
		picked, err := selector.Pick(context.Background(), "codex", "model", cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: session}}, all)
		if err != nil {
			t.Fatal(err)
		}
		return picked
	}
	bound := pick("stable")
	for range 10 {
		if pick("stable") != bound {
			t.Fatal("session changed account")
		}
	}
	// Real quota failures mark the credential unavailable until its reset.
	for _, auth := range []*Auth{first, second} {
		auth.Unavailable = true
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "credential_quota"
		auth.Quota.NextRecoverAt = now.Add(time.Hour)
	}
	if pick("stable") != backup {
		t.Fatal("did not fail over")
	}
	for _, auth := range []*Auth{first, second} {
		auth.Unavailable = false
		auth.Quota.Exceeded = false
	}
	if pick("stable") != backup {
		t.Fatal("recovery broke existing session")
	}
	for i := range 5 {
		if pick(fmt.Sprint("new-", i)) == backup {
			t.Fatal("new session ignored priority")
		}
	}
}
