package gateway

import (
	"testing"
	"time"
)

func testCred(id string, priority, weight int) *Credential {
	return &Credential{ID: id, Value: "v-" + id, Priority: priority, Weight: weight}
}

func TestGateway_CredentialCooldownBlocks(t *testing.T) {
	c := testCred("a", 0, 1)
	now := time.Now()
	if !c.available(now) {
		t.Fatal("fresh credential should be available")
	}
	c.cooldown(now, CooldownAuth, now.Add(10*time.Minute), "401")
	if c.available(now) {
		t.Fatal("auth cooldown should block")
	}
	if !c.available(now.Add(11 * time.Minute)) {
		t.Fatal("credential should recover after cooldown")
	}
}

func TestGateway_CredentialUnresolvedEnvBlocked(t *testing.T) {
	c := &Credential{ID: "a", Value: ""}
	if c.available(time.Now()) {
		t.Fatal("unresolved credential must not serve")
	}
	if _, ok := c.acquire(); ok {
		t.Fatal("unresolved credential must not acquire")
	}
}

func TestGateway_CredentialConcurrencyCap(t *testing.T) {
	c := &Credential{ID: "a", Value: "v", MaxConcurrent: 2}
	r1, ok := c.acquire()
	if !ok {
		t.Fatal("first acquire failed")
	}
	r2, ok := c.acquire()
	if !ok {
		t.Fatal("second acquire failed")
	}
	if _, ok := c.acquire(); ok {
		t.Fatal("third acquire must fail at cap 2")
	}
	r1()
	r2()
	if _, ok := c.acquire(); !ok {
		t.Fatal("acquire after release must succeed")
	}
}

func TestGateway_CredentialDoubleReleaseSafe(t *testing.T) {
	c := &Credential{ID: "a", Value: "v", MaxConcurrent: 1}
	r, _ := c.acquire()
	r()
	r()
	if _, ok := c.acquire(); !ok {
		t.Fatal("double release must not leak the slot")
	}
}

func TestGateway_SchedulerHighestPriorityTier(t *testing.T) {
	s := newScheduler([]*Credential{
		testCred("low", 0, 100),
		testCred("high1", 10, 1),
		testCred("high2", 10, 1),
	})
	for i := 0; i < 20; i++ {
		c := s.pick(time.Now(), nil)
		if c == nil {
			t.Fatal("pick returned nil")
		}
		if c.Priority != 10 {
			t.Fatalf("picked %q from lower tier while high tier available", c.ID)
		}
	}
}

func TestGateway_SchedulerDescendsWhenTopTierBlocked(t *testing.T) {
	s := newScheduler([]*Credential{
		testCred("high", 10, 1),
		testCred("low", 0, 1),
	})
	now := time.Now()
	s.members[0].cooldown(now, CooldownAuth, now.Add(time.Hour), "401")
	c := s.pick(now, nil)
	if c == nil || c.ID != "low" {
		t.Fatalf("expected low tier, got %+v", c)
	}
}

func TestGateway_SchedulerExcludesTried(t *testing.T) {
	s := newScheduler([]*Credential{testCred("a", 10, 1), testCred("b", 0, 1)})
	c := s.pick(time.Now(), map[string]struct{}{"a": {}, "b": {}})
	if c != nil {
		t.Fatal("all excluded must return nil")
	}
	c = s.pick(time.Now(), map[string]struct{}{"a": {}})
	if c == nil || c.ID != "b" {
		t.Fatalf("expected b after excluding a, got %+v", c)
	}
}

func TestGateway_SchedulerDisabledCredSkipped(t *testing.T) {
	high := testCred("high", 10, 1)
	high.Disabled = true
	s := newScheduler([]*Credential{high, testCred("low", 0, 1)})
	c := s.pick(time.Now(), nil)
	if c == nil || c.ID != "low" {
		t.Fatalf("disabled credential must be skipped, got %+v", c)
	}
}

func TestGateway_WeightedPickDistribution(t *testing.T) {
	a := testCred("a", 0, 3)
	b := testCred("b", 0, 1)
	counts := map[string]int{}
	for i := 0; i < 4000; i++ {
		c := weightedPick([]*Credential{a, b}, "", randnStub)
		counts[c.ID]++
	}
	// 3:1 ratio with generous tolerance
	if counts["a"] < 2600 || counts["a"] > 3400 {
		t.Fatalf("weight 3 expected ~3000 hits, got %d", counts["a"])
	}
}

func TestGateway_WeightedPickZeroWeightsRotate(t *testing.T) {
	a := testCred("a", 0, 0)
	b := testCred("b", 0, 0)
	c := testCred("c", 0, 0)
	tier := []*Credential{a, b, c}
	// prev "" -> starts at a; prev a -> b; prev b -> c; prev c -> a
	seq := []string{}
	prev := ""
	for i := 0; i < 6; i++ {
		chosen := weightedPick(tier, prev, randnStub)
		seq = append(seq, chosen.ID)
		prev = chosen.ID
	}
	want := "abcabc"
	got := seq[0] + seq[1] + seq[2] + seq[3] + seq[4] + seq[5]
	if got != want {
		t.Fatalf("rotation sequence = %s, want %s", got, want)
	}
}

func TestGateway_WeightedPickSmallWeightsAmplified(t *testing.T) {
	// weights 1 and 2 (sum < 10): amplified by factor 100, both reachable
	a := testCred("a", 0, 1)
	b := testCred("b", 0, 2)
	counts := map[string]int{}
	for i := 0; i < 3000; i++ {
		c := weightedPick([]*Credential{a, b}, "", randnStub)
		counts[c.ID]++
	}
	if counts["a"] == 0 || counts["b"] == 0 {
		t.Fatalf("both weights must be reachable: %v", counts)
	}
	if counts["b"] <= counts["a"] {
		t.Fatalf("weight 2 should win more often: %v", counts)
	}
}

func randnStub(n int) int {
	// deterministic LCG for reproducible distributions
	seed = (seed*1103515245 + 12345) & 0x7fffffff
	return int(seed % int64(n))
}

var seed int64 = 42

func TestGateway_StickyTableBindGetSlide(t *testing.T) {
	table := newStickyTable(time.Hour)
	now := time.Now()
	table.bind("sess1", "prov", "prov\x00Chat\x00m1", "credA", now, false)
	entry, ok := table.get("sess1", now.Add(30*time.Minute))
	if !ok || entry.credentialID != "credA" {
		t.Fatalf("get after 30m: %+v %v", entry, ok)
	}
	// Each hit slides the expiry (last hit at 30m -> expires at 90m).
	if _, ok := table.get("sess1", now.Add(89*time.Minute)); !ok {
		t.Fatal("entry should still be valid before the slid expiry")
	}
	// Hit at 89m slides expiry to 149m; 91m is still inside.
	if _, ok := table.get("sess1", now.Add(91*time.Minute)); !ok {
		t.Fatal("hit at 89m should have slid the expiry past 91m")
	}
	// No more hits: last hit at 91m expires at 151m.
	if _, ok := table.get("sess1", now.Add(152*time.Minute)); ok {
		t.Fatal("entry should expire after TTL without further hits")
	}
}

func TestGateway_StickyTableBindsRouteAndCredential(t *testing.T) {
	table := newStickyTable(time.Hour)
	now := time.Now()
	table.bind("s", "prov", "prov\x00Chat\x00m1", "credA", now, false)
	entry, ok := table.get("s", now)
	if !ok {
		t.Fatal("binding missing")
	}
	if entry.provider != "prov" || entry.route != "prov\x00Chat\x00m1" || entry.credentialID != "credA" {
		t.Fatalf("binding = %+v", entry)
	}

	// A different route for the same session must not overwrite the binding:
	// that would move the conversation to another upstream cache.
	table.bind("s", "other", "other\x00Chat\x00m1", "credB", now.Add(time.Minute), false)
	entry, _ = table.get("s", now.Add(2*time.Minute))
	if entry.route != "prov\x00Chat\x00m1" {
		t.Fatalf("binding must not move to another route, got %+v", entry)
	}

	// A key rotation inside the same route refreshes the credential and keeps
	// the route: the provider-side cache is still the same account.
	table.bind("s", "prov", "prov\x00Chat\x00m1", "credC", now.Add(3*time.Minute), false)
	entry, _ = table.get("s", now.Add(4*time.Minute))
	if entry.route != "prov\x00Chat\x00m1" || entry.credentialID != "credC" {
		t.Fatalf("same-route rebind must refresh the credential, got %+v", entry)
	}
}

// A binding whose route can no longer serve the request is the one case where
// another route takes over: the session moved off it because it was unusable,
// and it must not be pulled back when that route recovers — it has since built
// its prompt cache on the provider that answered.
func TestGateway_StickyTableReplacesOnlyUnavailableBinding(t *testing.T) {
	table := newStickyTable(time.Hour)
	now := time.Now()
	table.bind("s", "prov", "prov\x00Chat\x00m1", "credA", now, false)

	// The bound route is down: another provider answers and takes the session.
	table.bind("s", "other", "other\x00Chat\x00m1", "credB", now.Add(time.Minute), true)
	entry, _ := table.get("s", now.Add(2*time.Minute))
	if entry.route != "other\x00Chat\x00m1" {
		t.Fatalf("an unusable binding must be replaced, got %+v", entry)
	}

	// The original route is healthy again. It must not win the session back:
	// the cache the session has since built would be discarded.
	table.bind("s", "prov", "prov\x00Chat\x00m1", "credA", now.Add(3*time.Minute), false)
	entry, _ = table.get("s", now.Add(4*time.Minute))
	if entry.route != "other\x00Chat\x00m1" {
		t.Fatalf("a recovered provider must not take the session back, got %+v", entry)
	}

	// When the new route also becomes unusable, the session may move again.
	table.bind("s", "prov", "prov\x00Chat\x00m1", "credA", now.Add(5*time.Minute), true)
	entry, _ = table.get("s", now.Add(6*time.Minute))
	if entry.route != "prov\x00Chat\x00m1" {
		t.Fatalf("a second failure must let the session move again, got %+v", entry)
	}
}

func TestGateway_StickyTableClearCredential(t *testing.T) {
	table := newStickyTable(time.Hour)
	now := time.Now()
	table.bind("s1", "p", "p\x00Chat\x00m1", "a", now, false)
	table.bind("s2", "p", "p\x00Chat\x00m1", "a", now, false)
	table.bind("s3", "p", "p\x00Chat\x00m1", "b", now, false)
	table.clearCredential("a")
	if _, ok := table.get("s1", now); ok {
		t.Fatal("s1 should be unbound")
	}
	if _, ok := table.get("s2", now); ok {
		t.Fatal("s2 should be unbound")
	}
	if entry, ok := table.get("s3", now); !ok || entry.credentialID != "b" {
		t.Fatal("s3 must keep its binding")
	}
}

func TestGateway_SessionHashExplicitHeaderIsScopedToCaller(t *testing.T) {
	explicit := SessionHash("sess-123", "1.2.3.4", "key1", "body")
	fallback := SessionHash("", "1.2.3.4", "key1", "body")
	if explicit == fallback {
		t.Fatal("explicit header value must produce a different hash than the content fallback")
	}
	// An explicit session header still identifies one conversation, but the
	// sticky binding must not be shared across callers: otherwise two tenants
	// presenting the same header pin to one upstream credential.
	if SessionHash("sess-123", "9.9.9.9", "key2", "zzz") == explicit {
		t.Fatal("different callers presenting the same header must not share a sticky binding")
	}
	if SessionHash("sess-123", "1.2.3.4", "key1", "other-body") != explicit {
		t.Fatal("same caller and header must be stable regardless of the body")
	}
	// content fallback mixes identity
	a := SessionHash("", "1.2.3.4", "key1", "body")
	b := SessionHash("", "1.2.3.4", "key2", "body")
	if a == b {
		t.Fatal("different caller keys must not collide in the fallback")
	}
}

func TestGateway_SchedulerPickIsConcurrencySafe(t *testing.T) {
	s := newScheduler([]*Credential{testCred("a", 1, 1), testCred("b", 1, 1), testCred("c", 1, 1)})
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				c := s.pick(time.Now(), nil)
				if c == nil {
					t.Error("nil pick under concurrency")
					return
				}
				if release, ok := c.acquire(); ok {
					release()
				}
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

func TestGateway_StatusSnapshot(t *testing.T) {
	c := testCred("a", 0, 1)
	c.MaxConcurrent = 3
	now := time.Now()
	c.cooldown(now, CooldownRateLimit, now.Add(time.Minute), "429 no retry-after")
	st := c.Status()
	if !st.RateLimitUntil.After(now) {
		t.Fatalf("status should carry rate-limit cooldown: %+v", st)
	}
	if st.LastError != "429 no retry-after" {
		t.Fatalf("last error = %q", st.LastError)
	}
	c.ClearCooldowns()
	if c.available(now.Add(time.Second)) == false {
		t.Fatal("cleared credential must be available")
	}
}
