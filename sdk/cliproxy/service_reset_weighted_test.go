package cliproxy

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestResetWeightedRoutingConfiguration(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Strategy = "reset-weighted"
	state := normalizedRoutingRuntimeState(cfg)
	if _, ok := newRoutingSelector(state).(*coreauth.ResetWeightedSelector); !ok {
		t.Fatal("reset-weighted strategy was not selected")
	}
	cfg.Routing.SessionAffinity = true
	selector := newRoutingSelector(normalizedRoutingRuntimeState(cfg))
	if affinity, ok := selector.(*coreauth.SessionAffinitySelector); ok {
		defer affinity.Stop()
	} else {
		t.Fatal("session affinity was not applied")
	}
}
