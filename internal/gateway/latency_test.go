package gateway

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// 避免与包内其它测试文件里的同名助手冲突。
func skipSlowBoolPtr(b bool) *bool { return &b }

func testPolicy(factor float64, maxMs int, minSamples int) skipSlowPolicy {
	return resolveSkipSlow(config.GatewaySkipSlowConfig{
		Enabled:         skipSlowBoolPtr(true),
		MaxFirstTokenMs: maxMs,
		RelativeFactor:  factor,
		MinSamples:      minSamples,
	})
}

// 相对倍数规则：明显更慢的供应商被跳过，正常范围内的留下。
func TestSkipSlow_RelativeFactor(t *testing.T) {
	table := newLatencyTable()
	for i := 0; i < 5; i++ {
		table.observe("fast", 500*time.Millisecond)
		table.observe("slow", 5000*time.Millisecond)
	}
	policy := testPolicy(3, 0, 3)

	fast, ok, _ := table.latency("fast", policy.minSamples)
	if !ok || fast < 400 || fast > 600 {
		t.Fatalf("fast 的平滑值 = %v（ok=%v）", fast, ok)
	}
	slow, ok, _ := table.latency("slow", policy.minSamples)
	if !ok {
		t.Fatal("slow 样本够多却没被信任")
	}
	if !tooSlow(slow, fast, policy) {
		t.Fatalf("slow=%.0f fast=%.0f 应被判为过慢", slow, fast)
	}
	// 同一档内的差异不该触发跳过。
	table.observe("near", 900*time.Millisecond)
	table.observe("near", 900*time.Millisecond)
	table.observe("near", 900*time.Millisecond)
	near, _, _ := table.latency("near", policy.minSamples)
	if tooSlow(near, fast, policy) {
		t.Fatalf("near=%.0f fast=%.0f 不该被判为过慢", near, fast)
	}
}

// 绝对上限规则独立生效，且可以单独使用。
func TestSkipSlow_AbsoluteCeiling(t *testing.T) {
	table := newLatencyTable()
	for i := 0; i < 4; i++ {
		table.observe("slow", 30*time.Second)
	}
	policy := testPolicy(0, 10000, 3)
	ms, trusted, _ := table.latency("slow", policy.minSamples)
	if !trusted {
		t.Fatal("样本足够却没被信任")
	}
	if !tooSlow(ms, 1, policy) {
		t.Fatalf("超过绝对上限 %.0fms 应被判为过慢", ms)
	}
}

// 样本不足必须放行：冷启动、重载后的新网关、刚加的供应商都不能因为
// "还没测过"而被跳过，否则一次重载就会把流量锁死在某个供应商上。
func TestSkipSlow_UntrustedSamplesFailOpen(t *testing.T) {
	table := newLatencyTable()
	table.observe("new", 50*time.Second) // 一次很慢的观测
	policy := testPolicy(1.5, 0, 3)
	if _, trusted, _ := table.latency("new", policy.minSamples); trusted {
		t.Fatal("只有一条观测不应被信任")
	}
	// 一次观测不足以形成路由结论。
	if trustRequired := table.sampleCount("new"); trustRequired != 1 {
		t.Fatalf("样本数 = %d，期望 1", trustRequired)
	}
}

// EWMA 让单次异常不至于立刻改变路由，但持续变慢会被跟上。
func TestSkipSlow_EWMASmoothsOutliers(t *testing.T) {
	table := newLatencyTable()
	for i := 0; i < 5; i++ {
		table.observe("p", 500*time.Millisecond)
	}
	steady, _, _ := table.latency("p", 1)
	table.observe("p", 5*time.Second) // 一次抖动
	afterOutlier, _, _ := table.latency("p", 1)
	if afterOutlier >= 2000 {
		t.Fatalf("单次抖动把平滑值拉到 %.0fms，过冲过大", afterOutlier)
	}
	if afterOutlier <= steady {
		t.Fatalf("平滑值没有反映这次变慢：%0.f -> %.0f", steady, afterOutlier)
	}
	// 持续变慢会被跟上。
	for i := 0; i < 10; i++ {
		table.observe("p", 5*time.Second)
	}
	degraded, _, _ := table.latency("p", 1)
	if degraded < 3000 {
		t.Fatalf("持续变慢后平滑值仍只有 %.0fms", degraded)
	}
}

// 禁用时不产生任何筛选。
func TestSkipSlow_Disabled(t *testing.T) {
	policy := resolveSkipSlow(config.GatewaySkipSlowConfig{Enabled: skipSlowBoolPtr(false)})
	if policy.enabled {
		t.Fatal("disabled: false 未关闭策略")
	}
	// 关掉策略后，无论观测多慢都不再筛选：这是 operator 表达
	// "别替我决定" 的方式，也是回滚手段。
	table := newLatencyTable()
	for i := 0; i < 5; i++ {
		table.observe("slow", 60*time.Second)
	}
	if policy.enabled {
		t.Fatal("策略应为关闭")
	}
	// 策略关闭时 fastRoutes 原样返回候选（这里直接验证判定函数在
	// enabled=false 下不会产生跳过语义）。
	if tooSlow(60_000, 100, skipSlowPolicy{}) {
		t.Fatal("零值策略下不应判为过慢")
	}
}

// 无可信测量时的默认策略：开着但不能拿没测过的供应商做判断。
func TestSkipSlow_DefaultPolicyIsRelativeOnly(t *testing.T) {
	policy := resolveSkipSlow(config.GatewaySkipSlowConfig{})
	if !policy.enabled {
		t.Fatal("默认应启用")
	}
	if policy.maxFirstTokenMs != 0 {
		t.Fatalf("默认不应有绝对上限，实际 %.0f", policy.maxFirstTokenMs)
	}
	if policy.relativeFactor != config.GatewaySkipSlowDefaultFactor {
		t.Fatalf("相对倍数 = %v，期望默认 %v", policy.relativeFactor, config.GatewaySkipSlowDefaultFactor)
	}
	if policy.minSamples != config.GatewaySkipSlowDefaultMinSamples {
		t.Fatalf("最小样本数 = %d，期望 %d", policy.minSamples, config.GatewaySkipSlowDefaultMinSamples)
	}
}

// 只有一条可用路由时绝不跳过——需求点名要求的那条规则。
//
// 这条用例走 pickAcrossChain（真实的选取入口）而不是 fastRoutes：那里存在
// 两条互相覆盖的单候选保护（候选数 <= 1，以及"可信测量不足两个"），
// 只测 fastRoutes 的话删掉任何一条都不会变红，规则就失去了守卫。
func TestSkipSlow_SingleCandidateIsNeverSkipped(t *testing.T) {
	policy := testPolicy(3, 0, 1)
	table := newLatencyTable()
	for i := 0; i < 5; i++ {
		table.observe("only", 30*time.Second)
	}
	g := &Gateway{skipSlow: policy, latency: table, logger: nil}

	only := latencyTestRoute(t, "only")
	// 先确认 fastRoutes 在单候选下原样返回。
	if kept := g.fastRoutes([]*Route{only}); len(kept) != 1 {
		t.Fatalf("fastRoutes 跳过了唯一候选：%+v", kept)
	}

	// 再看真实选取：无论观测多慢都必须选出这条路由的凭据。
	cred, route := g.pickAcrossChain([]*Route{only}, map[string]struct{}{}, time.Now())
	if cred == nil || route == nil {
		t.Fatal("唯一候选没有给出凭据（把慢当成了不可用）")
	}
	if route.ProviderID != "only" {
		t.Fatalf("选中 %s，期望 only", route.ProviderID)
	}
}

// 所有候选都慢时必须全部保留：把它们全跳过会把"这个供应商慢"升级成
// "这个模型不可用"，严格更糟。
func TestSkipSlow_NeverStarvesWhenAllCandidatesAreSlow(t *testing.T) {
	policy := testPolicy(0, 1000, 1) // 绝对上限，两个供应商都超
	table := newLatencyTable()
	for i := 0; i < 5; i++ {
		table.observe("a", 20*time.Second)
		table.observe("b", 30*time.Second)
	}
	g := &Gateway{skipSlow: policy, latency: table, logger: nil}
	routes := []*Route{{ProviderID: "a"}, {ProviderID: "b"}}
	kept := g.fastRoutes(routes)
	if len(kept) != 2 {
		t.Fatalf("全部过慢时被清空：%d 条留下", len(kept))
	}
}

// 只有一条可信测量时无法比较，因此不做任何筛选（另一条还没测过）。
func TestSkipSlow_NeedsTwoTrustedMeasurements(t *testing.T) {
	policy := testPolicy(2, 0, 3)
	table := newLatencyTable()
	for i := 0; i < 5; i++ {
		table.observe("fast", 200*time.Millisecond)
	}
	table.observe("unmeasured", 9*time.Second) // 只有 1 条，不可信
	g := &Gateway{skipSlow: policy, latency: table, logger: nil}
	routes := []*Route{{ProviderID: "fast"}, {ProviderID: "unmeasured"}}
	kept := g.fastRoutes(routes)
	if len(kept) != 2 {
		t.Fatalf("把还没测过的供应商筛掉了：%+v", kept)
	}
}

// 端到端：慢供应商在链条中排前面时，选择仍然落到快的那条。
func TestSkipSlow_PickPrefersFastProviderOverChainOrder(t *testing.T) {
	policy := testPolicy(3, 0, 2)
	table := newLatencyTable()
	for i := 0; i < 5; i++ {
		table.observe("slow", 6*time.Second)
		table.observe("fast", 400*time.Millisecond)
	}
	g := &Gateway{skipSlow: policy, latency: table, logger: nil}

	slowRoute := latencyTestRoute(t, "slow")
	fastRoute := latencyTestRoute(t, "fast")
	// 故意的顺序：慢的在前，模拟按优先级排出的链条。
	chain := []*Route{slowRoute, fastRoute}
	cred, route := g.pickAcrossChain(chain, map[string]struct{}{}, time.Now())
	if cred == nil || route == nil {
		t.Fatal("没有选出任何凭据")
	}
	if route.ProviderID != "fast" {
		t.Fatalf("选中了 %s，期望跳过慢的选 fast", route.ProviderID)
	}
	if cred.ID == "" {
		t.Fatal("凭据 ID 为空")
	}
}

// 慢供应商的凭据被冷却时不应算作"最快的替代"而挤掉健康兄弟：
// 先剔除无候选的路由，再做慢判定。
func TestSkipSlow_ExhaustedRouteDoesNotSuppressHealthySibling(t *testing.T) {
	policy := testPolicy(3, 0, 1)
	table := newLatencyTable()
	for i := 0; i < 3; i++ {
		table.observe("fast", 300*time.Millisecond)
	}
	g := &Gateway{skipSlow: policy, latency: table, logger: nil}

	// slow 路由没有任何凭据（没有候选），fast 是唯一真正可用的。
	slowRoute := &Route{ProviderID: "slow"}
	fastRoute := latencyTestRoute(t, "fast")
	chain := []*Route{slowRoute, fastRoute}

	cred, route := g.pickAcrossChain(chain, map[string]struct{}{}, time.Now())
	if cred == nil || route == nil {
		t.Fatal("没有选出任何凭据")
	}
	if route.ProviderID != "fast" {
		t.Fatalf("选中 %s，期望 fast", route.ProviderID)
	}
}

// latencyTestRoute 构造一条带可用凭据的路由：调度器、成员与优先队列都按
// 生产路径建立，避免测试自己搭一套与真实调度不同的结构。
func latencyTestRoute(t *testing.T, providerID string) *Route {
	t.Helper()
	cfg := config.Config{
		Providers: config.ProviderDictionaryConfig{
			providerID: {
				Type:     config.ProviderTypeOpenAI,
				Endpoint: "https://" + providerID + ".example",
				ApiKeys:  []config.ProviderKey{{Value: "key-" + providerID, Weight: 1}},
				Models:   []config.ProviderModel{{Local: "m1", Remote: "m1"}},
			},
		},
	}
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New(%s): %v", providerID, err)
	}
	t.Cleanup(g.Shutdown)
	route := g.RouteFor(providerID+"/m1", false)
	if route == nil {
		t.Fatalf("%s 路由缺失", providerID)
	}
	return route
}

// TestSkipSlow_ProbeRestoresProviderExcludedForBeingSlow 覆盖"饿死"这个正反馈
// 陷阱，以及它现在被探测窗口打破的方式。
//
// 背景（生产实测）：skipSlow 只看观测值，而被跳过的供应商拿不到新请求，
// 于是它的 EWMA 冻结在变慢那一刻，快的兄弟却持续刷新样本——差距只会拉大。
// deepseek-flash 就是这样：WorkBuddy2API 约 5.3s、opencode-go 约 18.5s
// （3.52 倍，超过默认 factor 3），后者被永久筛出、全天零请求。没有探测，
// 它即使恢复也没有任何机会证明自己。
//
// 这个测试锁住三件事：正常跳过仍然生效（不能立刻放行）、排除满一个周期后
// 重新放行、以及被放行后拿到的新观测确实能把它留在池里。
func TestSkipSlow_ProbeRestoresProviderExcludedForBeingSlow(t *testing.T) {
	table := newLatencyTable()
	policy := testPolicy(3, 0, 3)
	policy.probeInterval = 5 * time.Minute
	g := &Gateway{skipSlow: policy, latency: table, logger: nil}
	slowRoute := latencyTestRoute(t, "slow")
	fastRoute := latencyTestRoute(t, "fast")
	chain := []*Route{slowRoute, fastRoute}

	// 起点：两家都在 3 倍以内，正常轮转。
	for i := 0; i < 3; i++ {
		table.observe("fast", 5000*time.Millisecond)
		table.observe("slow", 9000*time.Millisecond)
	}
	if kept := g.fastRoutes(chain); len(kept) != 2 {
		t.Fatalf("起点应保留两家，实际 %d", len(kept))
	}

	// slow 劣化（上游限流、冷启动、账号被压）。EWMA 需要多次观测才越过 3 倍，
	// 这也解释了生产上"某天开始只落一家"是渐变的而非瞬时的。
	for i := 0; i < 10; i++ {
		table.observe("slow", 20000*time.Millisecond)
	}

	// 劣化后立刻请求：必须照常跳过，探测不能让人人都当场复活。
	kept := g.fastRoutes(chain)
	if len(kept) != 1 || kept[0].ProviderID != "fast" {
		t.Fatalf("劣化后应立即被跳过，实际保留 %d 条: %+v", len(kept), kept)
	}

	// 还没到探测周期：继续跳过。
	kept = g.fastRoutes(chain)
	if len(kept) != 1 || kept[0].ProviderID != "fast" {
		t.Fatalf("探测周期未到就放行了: %+v", kept)
	}

	// 把排除起点往前拨，模拟"已经被排除满一个周期"。
	table.mu.Lock()
	table.observations["slow"].excludedSince = time.Now().Add(-policy.probeInterval)
	table.mu.Unlock()

	// 周期到了：必须放行一次，否则恢复永远不可观测。
	kept = g.fastRoutes(chain)
	if len(kept) != 2 {
		t.Fatalf("排除满一个周期后应重新放行，实际保留 %d 条: %+v", len(kept), kept)
	}

	// 探测窗口内多次查询都要放行：一次请求会多次求值，一次性放行会在分发
	// 之前就被消耗掉。
	if kept = g.fastRoutes(chain); len(kept) != 2 {
		t.Fatalf("探测窗口内应持续放行，实际 %d 条", len(kept))
	}

	// 探测拿到新观测：slow 已恢复，EWMA 下降，此后留在池里。
	for i := 0; i < 12; i++ {
		table.observe("slow", 5000*time.Millisecond)
	}
	fastMs, _, _ := table.latency("fast", policy.minSamples)
	slowMs, _, _ := table.latency("slow", policy.minSamples)
	if tooSlow(slowMs, fastMs, policy) {
		t.Fatalf("恢复后 slow=%.0f fast=%.0f 不该再判慢", slowMs, fastMs)
	}
	if kept = g.fastRoutes(chain); len(kept) != 2 {
		t.Fatalf("恢复后应稳定留在池里，实际 %d 条", len(kept))
	}
}

// TestSkipSlow_ProbeDisabledKeepsExclusionPermanent 锁住 probeInterval: "0s" 的
// 语义：关掉探测就把老行为原样还回来（排除是永久的），这是给明确要求
// "只要最快的"的部署留的出口，不能让默认值偷偷覆盖它。
func TestSkipSlow_ProbeDisabledKeepsExclusionPermanent(t *testing.T) {
	table := newLatencyTable()
	policy := testPolicy(3, 0, 3)
	policy.probeInterval = 0
	g := &Gateway{skipSlow: policy, latency: table, logger: nil}

	for i := 0; i < 3; i++ {
		table.observe("fast", 5000*time.Millisecond)
		table.observe("slow", 9000*time.Millisecond)
	}
	for i := 0; i < 10; i++ {
		table.observe("slow", 20000*time.Millisecond)
	}

	table.mu.Lock()
	table.observations["slow"].excludedSince = time.Now().Add(-24 * time.Hour)
	table.mu.Unlock()

	chain := []*Route{latencyTestRoute(t, "slow"), latencyTestRoute(t, "fast")}
	kept := g.fastRoutes(chain)
	if len(kept) != 1 || kept[0].ProviderID != "fast" {
		t.Fatalf("探测关闭时排除应保持永久，实际保留 %d 条: %+v", len(kept), kept)
	}
}

// TestSkipSlow_NoMeasurementsKeepsEveryone 守住另一端：一条可信测量都没有时，
// 策略不该凭空筛人。
func TestSkipSlow_NoMeasurementsKeepsEveryone(t *testing.T) {
	table := newLatencyTable()
	policy := testPolicy(3, 0, 3)
	g := &Gateway{skipSlow: policy, latency: table, logger: nil}

	for i := 0; i < 2; i++ {
		table.observe("a", 5*time.Second)
		table.observe("b", 5*time.Second)
	}

	chain := []*Route{latencyTestRoute(t, "a"), latencyTestRoute(t, "b")}
	if kept := g.fastRoutes(chain); len(kept) != 2 {
		t.Fatalf("样本都不足时不应筛任何人，实际保留 %d 条", len(kept))
	}
}
