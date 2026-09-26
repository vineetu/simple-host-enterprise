package handler

import (
	"testing"

	"github.com/vsriram/simple-host/internal/ratelimit"
)

func TestSharedPoliciesAreTheSecurityLimits(t *testing.T) {
	want := []ratelimit.Policy{authClientPolicy, apiKeyMintPolicy, oauthRegisterPolicy, oauthTokenPolicy}
	if len(sharedPolicies) != len(want) {
		t.Fatalf("sharedPolicies has %d entries, want %d", len(sharedPolicies), len(want))
	}
	for _, p := range want {
		if !sharedPolicies[p.Name] {
			t.Errorf("%s is not shared across replicas", p.Name)
		}
		if _, window := ratelimit.FixedWindow(p); window <= 0 || window > ratelimit.MaxSharedWindow {
			t.Errorf("%s maps to window %v, outside (0, %v]", p.Name, window, ratelimit.MaxSharedWindow)
		}
	}
	if apiKeyMintPolicy.Burst != 30 || apiKeyMintPolicy.RefillPerSecond != 0.1 {
		t.Error("api-key-mint drifted from the management-user budget it replaced")
	}
}

// Two AbuseLimits on one database stand in for two pods: the sign-in
// budget is shared, the management budget stays per pod.
func TestAbuseLimitsSharedAcrossPods(t *testing.T) {
	database := connectorTestDB(t)
	podA := NewAbuseLimits().WithSharedStore(database)
	podB := NewAbuseLimits().WithSharedStore(database)

	allowed := 0
	for i := 0; i < authClientPolicy.Burst*2; i++ {
		pod := podA
		if i%2 == 1 {
			pod = podB
		}
		if pod.allow(authClientPolicy, "203.0.113.5").Allowed {
			allowed++
		}
	}
	// A window boundary inside the loop may admit up to one more budget.
	if allowed < authClientPolicy.Burst || allowed >= authClientPolicy.Burst*2 {
		t.Fatalf("two pods admitted %d sign-in requests, want the shared %d", allowed, authClientPolicy.Burst)
	}

	for _, pod := range []*AbuseLimits{podA, podB} {
		for i := 0; i < managementUserPolicy.Burst; i++ {
			if !pod.allow(managementUserPolicy, "user-1").Allowed {
				t.Fatalf("management request %d denied: that limit must stay per pod", i+1)
			}
		}
	}
}
