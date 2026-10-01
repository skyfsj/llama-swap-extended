import { describe, expect, it } from "vitest";
import { parsePrometheusMetrics, summarizeLMCacheMetrics, summarizeLMCacheMetricsText } from "./lmcacheMetrics";

describe("parsePrometheusMetrics", () => {
  it("parses comments, labels and escaped label values", () => {
    const samples = parsePrometheusMetrics(`# HELP lmcache_mp_l2_usage_bytes usage\n# TYPE lmcache_mp_l2_usage_bytes gauge\nlmcache_mp_l2_usage_bytes{l2_name="disk\\\"one"} 12.5 123\nlmcache_mp_lookup_hit 2`);
    expect(samples).toEqual([
      { name: "lmcache_mp_l2_usage_bytes", labels: { l2_name: 'disk"one' }, value: 12.5 },
      { name: "lmcache_mp_lookup_hit", labels: {}, value: 2 },
    ]);
  });

  it("ignores malformed samples while preserving Prometheus special floats", () => {
    expect(parsePrometheusMetrics("bad line\nlmcache_mp_lookup_hit NaN\nlmcache_mp_lookup_hit 4\n")).toEqual([
      { name: "lmcache_mp_lookup_hit", labels: {}, value: Number.NaN },
      { name: "lmcache_mp_lookup_hit", labels: {}, value: 4 },
    ]);
  });
});

describe("summarizeLMCacheMetrics", () => {
  it("aggregates lookup ratios, usage and labelled backends", () => {
    const summary = summarizeLMCacheMetricsText([
      "lmcache_mp.lookup_requested_tokens_total 10",
      "lmcache_mp.lookup_hit_tokens_total 7",
      "lmcache_mp.l1_memory_usage_bytes 2048",
      "lmcache_mp.l1_usage_ratio 0.25",
      "lmcache_mp.l2_usage_bytes{l2_name=\"ram\"} 3",
      "lmcache_mp.l2_usage_bytes{l2_name=\"disk\"} 4",
      "lmcache_mp.active_prefetch_jobs 2",
      "lmcache_mp.l1_read_failure_total 1",
      "lmcache_mp.l2_prefetch_failure 2",
    ].join("\n"));
    expect(summary.lookupRequested).toBe(10);
    expect(summary.lookupHit).toBe(7);
    expect(summary.lookupHitRatio).toBe(0.7);
    expect(summary.l1MemoryUsageBytes).toBe(2048);
    expect(summary.l1UsageRatio).toBe(0.25);
    expect(summary.l2UsageBytes).toEqual([{ name: "disk", value: 4 }, { name: "ram", value: 3 }]);
    expect(summary.activePrefetchJobs).toBe(2);
    expect(summary.failures).toBe(3);
  });

  it("does not invent a ratio when the denominator is zero or missing", () => {
    expect(summarizeLMCacheMetrics([{ name: "lookup_hit", labels: {}, value: 1 }]).lookupHitRatio).toBeUndefined();
    expect(summarizeLMCacheMetrics([]).l2UsageBytes).toEqual([]);
  });
});
