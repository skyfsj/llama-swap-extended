export type ConnectionState = "connected" | "connecting" | "disconnected";

export type ModelStatus = "ready" | "sleeping" | "starting" | "stopping" | "stopped" | "shutdown" | "unknown";
export type ModelConfigStatus = "applied" | "modified" | "draining" | "restarting" | "rolling_back" | "apply_failed" | "removing" | "unloading";
export type PlaygroundModelType = "model" | "peer" | "selector" | "profile";

export interface ModelCapabilities {
  vision?: boolean;
  audio_transcriptions?: boolean;
  audio_speech?: boolean;
  image_generation?: boolean;
  image_to_image?: boolean;
  function_calling?: boolean;
  reranker?: boolean;
  translation?: boolean;
}

export interface Model {
  id: string;
  state: ModelStatus;
  backendType?: string;
  name: string;
  description: string;
  unlisted: boolean;
  disabled: boolean;
  maintenance: boolean;
  peerID: string;
  playgroundType?: PlaygroundModelType;
  aliases?: string[];
  capabilities?: ModelCapabilities;
  context_length?: number;
  // selector-only fields from the v1/models llamaswap metadata
  strategy?: string;
  targets?: string[];
  spillover?: number;
  configStatus?: ModelConfigStatus;
  appliedRevision?: number;
  desiredRevision?: number;
  oldRequests?: number;
  waitingRequests?: number;
  error?: string;
}

export interface ModelFile {
  id: string;
  name: string;
  path: string;
  relative_path: string;
  source_id: string;
  source_type: "directory" | "file" | "hf_cache" | "modelscope_cache" | string;
  repository?: string;
  revision?: string;
  format: string;
  size: number;
  modified_at: string;
  symlink?: boolean;
  registered_models?: string[];
  in_use_models?: string[];
}

export interface ModelFileSource {
  id: string;
  name: string;
  type: "directory" | "file" | "hf_cache" | "modelscope_cache" | string;
  path: string;
  configured: boolean;
  available: boolean;
  file_count: number;
  error?: string;
}

export interface ModelFilesResponse {
  data: ModelFile[];
  sources: ModelFileSource[];
  total: number;
  limit: number;
  offset: number;
  truncated: boolean;
  scanned_at: string;
}

export type ModelDownloadStatus = "queued" | "retrying" | "downloading" | "completed" | "failed" | "canceled" | string;

export interface ModelDownload {
  id: string;
  provider: "huggingface" | "modelscope" | string;
  repo_id: string;
  revision: string;
  source_id: string;
  include?: string[];
  exclude?: string[];
  status: ModelDownloadStatus;
  current_file?: string;
  total_files: number;
  completed_files: number;
  total_bytes: number;
  downloaded_bytes: number;
  attempts: number;
  next_retry_at?: string;
  error?: string;
  created_at: string;
  updated_at: string;
  started_at?: string;
  finished_at?: string;
}

export interface ModelDownloadsResponse {
  data: ModelDownload[];
  limit: number;
  offset: number;
}

export interface Profile {
  id: string;
  description: string;
  pins: Record<string, string>;
}

export interface ProfileState {
  active: string | null;
  profiles: Profile[];
}

export interface TokenMetrics {
  cache_tokens: number;
  draft_tokens: number;
  draft_acc_tokens: number;
  input_tokens: number;
  output_tokens: number;
  prompt_per_second: number;
  tokens_per_second: number;
}

export interface ActivityLogEntry {
  id: number;
  timestamp: string;
  model: string;
  req_path: string;
  resp_content_type: string;
  resp_status_code: number;
  tokens: TokenMetrics;
  /** Managed key/session attribution (the raw key is never returned). */
  key_id?: string;
  session_id?: string;
  /** Anthropic/cache telemetry attached by the metrics pipeline. */
  cache_creation_tokens?: number;
  reasoning_tokens?: number;
  cache_hit_ratio?: number;
  cache_creation_ratio?: number;
  repair_applied?: boolean;
  prefix_hash?: string;
  estimated_cost?: number;
  cost_estimated?: boolean;
  duration_ms: number;
  /** Milliseconds from request admission to the first visible response token. */
  first_token_ms: number;
  /** Raw request/response audit details are available for this activity row. */
  has_audit: boolean;
  error_msg?: string;
  metadata?: Record<string, string>;
}

export interface AuditConversation {
  id: string;
  activityId?: number;
  requestId?: string;
  keyId?: string;
  model: string;
  sessionId?: string;
  reqPath: string;
  timestamp: string;
  requestHeaders?: string | number[];
  requestBody?: string | number[];
  responseHeaders?: string | number[];
  responseBody?: string | number[];
  // Persisted bodies live in the server's blob store: the detail view returns a
  // reference and the logical size, and the body is streamed on demand. Only
  // stores that keep bodies inline (in-memory) populate the fields above.
  requestBodyRef?: string;
  responseBodyRef?: string;
  requestBodyBytes?: number;
  responseBodyBytes?: number;
  responseStatus: number;
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;
  cacheCreationTokens?: number;
  cacheHitRatio?: number;
  cacheCreationRatio?: number;
  reasoningTokens?: number;
  repairApplied?: boolean;
  prefixHash?: string;
  /** First-to-last visible token interval in ms; -1 when not measured. */
  firstTokenMs?: number;
  decodeMs?: number;
  durationMs?: number;
  /** Bounded [msSinceStart, cumulativeTokens] JSON curve; empty when not measured. */
  speedTimeline?: string;
  estimatedCost: number;
  complete: boolean;
  sizeBytes?: number;
}

export interface AuditConversationPage {
  data: AuditConversation[];
  page: number;
  limit: number;
  total: number;
  total_pages: number;
}

export interface ActivityPage {
  data: ActivityLogEntry[];
  page: number;
  limit: number;
  total: number;
  total_pages: number;
}

export interface LogData {
  source: "upstream" | "proxy";
  data: string;
}

export interface InflightRequestEntry {
  id: string;
  timestamp: string;
  model: string;
  req_path: string;
  method: string;
  phase?: string;
  phase_message?: string;
  output_preview?: string;
  req_headers: Record<string, string>;
  remote_ip: string;
  resp_headers: Record<string, string>;
  resp_bytes: number;
  elapsed_ms: number;
  /** Present only when the upstream explicitly reports token telemetry. */
  input_tokens?: number;
  output_tokens?: number;
  cached_tokens?: number;
  prompt_per_second?: number;
  tokens_per_second?: number;
  first_token_ms?: number;
  client_received_at_ms?: number;
  metadata?: Record<string, string>;
}

export interface ModelLoadConflict {
  id: string;
  name: string;
  state: string;
}

export interface InFlightStats {
  operation: "snapshot" | "upsert" | "remove";
  requests?: InflightRequestEntry[];
  request?: InflightRequestEntry;
  id?: string;
}

export interface BackendProgressEvent {
  model?: string;
  runtime?: string;
  operationId?: string;
  phase: string;
  progress: number;
  /** Bytes completed by a download/build stage; total=-1 means unknown. */
  completed?: number;
  total?: number;
  message?: string;
  error?: string;
  outputStream?: "stdout" | "stderr" | string;
  output?: string;
}

export interface RuntimeStatus {
  name: string;
  kind?: string;
  configured?: boolean;
  mode?: string;
  source?: string;
  state: string;
  current?: string;
  previous?: string;
  staged?: string;
  available?: string;
  pinned?: string;
  lastCheck?: string;
  lastUpdate?: string;
  lastError?: string;
  operationId?: string;
  updatedAt?: string;
}

export interface RuntimeManifest {
  name: string;
  version: string;
  kind: string;
  source: string;
  ref?: string;
  commit?: string;
  python?: string;
  pythonVersion?: string;
  vllm?: string;
  torch?: string;
  cuda?: string;
  rocm?: string;
  checksum?: string;
  fingerprint?: string;
  metadata?: Record<string, string>;
  installedAt?: string;
}

export interface RuntimeDetail {
  status: RuntimeStatus;
  versions?: Record<string, RuntimeManifest>;
  operations?: RuntimeOperation[];
}

export interface RuntimeOperation {
  id: string;
  name: string;
  action: string;
  version?: string;
  state: string;
  error?: string;
  timestamp?: string;
}

export interface RuntimeVersionCandidate {
  version: string;
  label?: string;
  ref?: string;
  commit?: string;
  digest?: string;
  installed?: boolean;
  current?: boolean;
  previous?: boolean;
  staged?: boolean;
  pinned?: boolean;
  recommended?: boolean;
}

export interface RuntimeCatalogResponse {
  data?: RuntimeVersionCandidate[];
  sourceType?: string;
  supported?: boolean;
  fetchedAt?: string;
}

export type RuntimeReadinessLevel = "pass" | "warning" | "block";

export interface RuntimeReadinessCheck {
  code: string;
  level: RuntimeReadinessLevel;
  title: string;
  detail: string;
}

export interface RuntimeIdleReason {
  code: string;
  title: string;
  detail: string;
  count?: number;
}

export interface RuntimeReadiness {
  runtime: {
    name: string;
    kind: string;
    mode: string;
    state: string;
    configured: boolean;
    current?: string;
    candidate?: string;
    staged?: string;
    previous?: string;
    pinned?: string;
  };
  selected: {
    version?: string;
    kind?: string;
    source?: string;
    ref?: string;
    commit?: string;
    installed: boolean;
    current: boolean;
    staged: boolean;
    previous: boolean;
    pinned: boolean;
  };
  idle: {
    scope: string;
    ready: boolean;
    reasons?: RuntimeIdleReason[];
  };
  models: RuntimeModelImpact[];
  resources: RuntimeResourceReadiness;
  rollback: {
    available: boolean;
    version?: string;
    automatic: boolean;
    detail: string;
  };
  checks: RuntimeReadinessCheck[];
  generatedAt?: string;
}

export interface RuntimeModelImpact {
  id: string;
  type: string;
  runtime: string;
  runtimeVersion?: string;
  state?: string;
  loaded: boolean;
  sleeping: boolean;
  inflight: number;
  legacyCommand: boolean;
  compatible: boolean;
  compatibility?: string;
  willRestart: boolean;
  declaredFootprint: boolean;
  footprint: ResourceFootprint;
}

export interface ResourceFootprint {
  vramMiB: number;
  ramMiB: number;
  gpus?: string[];
  priority: number;
  evictionPriority: number;
}

export interface RuntimeResourceReadiness {
  configured: boolean;
  budget: { vramMiB: number; ramMiB: number };
  usageVRAMMiB: number;
  usageRAMMiB: number;
  models: Array<{
    id: string;
    loaded: boolean;
    sleeping: boolean;
    inflight: number;
    declared: boolean;
    footprint: ResourceFootprint;
  }>;
  hardware: {
    detected: boolean;
    accelerators: number;
    vramMiB: number;
    systemRAMMiB: number;
  };
}

export interface BackendCacheState {
  supported: boolean;
  sleeping: boolean;
  cachedTokens?: number;
  lastReset?: string;
}

export interface BackendCacheReport {
  hit: boolean;
  cachedTokens: number;
  creationTokens: number;
  prefixHash?: string;
  observedAt?: string;
}

export interface BackendStatus {
  model: string;
  type?: string;
  runtime?: string;
  protocol?: string;
  apis?: string[];
  lifecycle?: Record<string, unknown>;
  resources?: Record<string, unknown>;
  pricing?: Record<string, unknown>;
  capabilities?: Record<string, boolean>;
  cacheState?: "unknown" | "awake" | "sleeping" | string;
  cache?: BackendCacheState;
  cacheReport?: BackendCacheReport;
  discovery?: {
    type?: string;
    version?: string;
    models?: string[];
    capabilities?: Record<string, boolean>;
    serverInfo?: Record<string, string>;
    source?: string;
    error?: string;
  };
}

export interface LMCacheRuntimeStatus {
  name: string;
  active: boolean;
  installed: boolean;
  version?: string;
  error?: string;
}

export type LMCacheServerState =
  | "NOT_INSTALLED"
  | "STOPPED"
  | "STARTING"
  | "RUNNING"
  | "STOPPING"
  | "ERROR"
  | "UPDATING";

export interface LMCacheServerStatus {
  enabled: boolean;
  state: LMCacheServerState;
  running: boolean;
  version?: string;
  venvPath?: string;
  logPath?: string;
  pid?: number;
  startedAt?: string;
  lastError?: string;
  healthy: boolean;
  healthCheckedAt?: string;
  host: string;
  port: number;
  httpHost: string;
  httpPort: number;
  l1SizeGB: number;
  evictionPolicy: string;
  chunkSize: number;
  l2Enabled: boolean;
  l2MaxBytes: number;
  l2UsedBytes?: number | null;
  l3Enabled: boolean;
  l3Path?: string;
}

export type LMCacheUpdateState =
  | "IDLE"
  | "CHECKING"
  | "UPDATE_AVAILABLE"
  | "STAGING"
  | "DOWNLOADING"
  | "BUILDING"
  | "STAGED"
  | "VERIFYING"
  | "WAITING_FOR_IDLE"
  | "ACTIVATING"
  | "HEALTH_CHECK"
  | "ACTIVE"
  | "ROLLBACK"
  | "DEGRADED"
  | string;

export interface LMCacheUpdateStatus {
  policy: string;
  channel: string;
  targetVersion?: string;
  current?: string;
  previous?: string;
  staged?: string;
  available?: string;
  pinned: boolean;
  pinnedVersion?: string;
  lastCheck?: string;
  lastUpdate?: string;
  state: LMCacheUpdateState;
  lastError?: string;
}

export interface LMCacheStatus {
  installed: boolean;
  allInstalled: boolean;
  version?: string;
  runtimes: LMCacheRuntimeStatus[];
  enabledModels: string[];
  usingModels: string[];
  installing: boolean;
  pendingRestart: boolean;
  server: LMCacheServerStatus;
  update: LMCacheUpdateStatus;
  error?: string;
}

export interface LMCacheDashboardHealth {
  healthy: boolean;
  status?: string;
  httpStatus: number;
  checkedAt: string;
  error?: string;
}

export interface LMCacheDashboardStatus {
  healthy?: boolean;
  engineType?: string;
  chunkSize?: number;
  activeSessions?: number;
  activePrefetchJobs?: number;
  l1MemoryUsedBytes?: number;
}

export interface LMCacheDashboardVersions {
  version?: string;
  lmcacheVersion?: string;
  commitId?: string;
  versionError?: string;
  lmcacheVersionError?: string;
  commitIdError?: string;
}

export interface LMCacheDashboardMetrics {
  body?: string;
  contentType?: string;
  fetchedAt?: string;
  error?: string;
}

export interface LMCacheDashboardPeriodicHealth {
  healthy?: boolean;
  unhealthyThreads?: unknown[];
  error?: string;
}

export interface LMCacheDashboardResponse {
  available: boolean;
  reason?: "stopped" | "error" | "unhealthy" | "invalid_endpoint" | "unavailable" | string;
  checkedAt: string;
  health: LMCacheDashboardHealth;
  status?: LMCacheDashboardStatus;
  adapters?: unknown;
  versions: LMCacheDashboardVersions;
  metrics: LMCacheDashboardMetrics;
  periodicHealth: LMCacheDashboardPeriodicHealth;
  errors?: Record<string, string>;
}

export interface ResourceModelStatus {
  id: string;
  loaded: boolean;
  inflight: number;
  sleeping: boolean;
  state?: string;
  footprint: {
    vramMiB: number;
    ramMiB: number;
    gpus?: string[];
    priority: number;
    evictionPriority: number;
  };
}

export interface ResourceStatus {
  enabled: boolean;
  budget?: { vramMiB: number; ramMiB: number };
  usageVRAMMiB?: number;
  usageRAMMiB?: number;
  models: ResourceModelStatus[];
  evictionCandidates?: string[];
}

export interface UIConfig {
  activity: {
    session_id: string[];
  };
}

export interface NetIOStat {
  name: string;
  bytes_recv: number;
  bytes_sent: number;
}

export interface SysStat {
  timestamp: string;
  cpu_util_per_core: number[];
  mem_total_mb: number;
  mem_used_mb: number;
  mem_free_mb: number;
  swap_total_mb: number;
  swap_used_mb: number;
  load_avg_1: number;
  load_avg_5: number;
  load_avg_15: number;
  net_io: NetIOStat[];
}

export interface GpuStat {
  timestamp: string;
  id: number;
  name: string;
  uuid: string;
  temp_c: number;
  vram_temp_c: number;
  gpu_util_pct: number;
  mem_util_pct: number;
  mem_used_mb: number;
  mem_total_mb: number;
  fan_speed_pct: number;
  power_draw_w: number;
}

export interface PerformanceResponse {
  sys_stats: SysStat[];
  gpu_stats: GpuStat[];
}

export interface APIEventEnvelope {
  type: "modelStatus" | "logData" | "activity" | "inflight" | "uiConfig" | "profileChanged" | "backendProgress" | "perfsys" | "perfgpu";
  data: string;
}

export interface HistogramData {
  bins: number[];
  min: number;
  max: number;
  binSize: number;
  p99: number;
  p95: number;
  p50: number;
}

/** One time bucket of a model's inference telemetry, from /api/metrics/speed. */
export interface SpeedPointData {
  timestamp: string;
  requests: number;
  /** Average prefill (prompt) rate in tokens/sec, negative when unreported. */
  prefill_tps: number;
  /** Average decode (generation) rate in tokens/sec, negative when unreported. */
  decode_tps: number;
  /** Average time to first token in ms, negative when unreported. */
  ttft_ms: number;
}

export interface SpeedSeriesData {
  model: string;
  points: SpeedPointData[];
}

/** One context-length bucket: the range is (min_tokens, max_tokens], 0 = open. */
export interface ContextBucketData {
  label: string;
  min_tokens: number;
  max_tokens: number;
  requests: number;
  avg_input_tokens: number;
  avg_output_tokens: number;
  prefill_tps: number;
  decode_tps: number;
  ttft_ms: number;
}

export interface ContextSeriesData {
  model: string;
  buckets: ContextBucketData[];
}

export interface SpeedReportData {
  bucket_seconds: number;
  series: SpeedSeriesData[];
  context: ContextSeriesData[];
}

export interface ActivityStatsData {
  total_requests: number;
  total_input_tokens: number;
  total_output_tokens: number;
  total_cache_tokens: number;
  total_cache_creation_tokens?: number;
  total_reasoning_tokens?: number;
  cache_hit_ratio?: number;
  cache_creation_ratio?: number;
  estimated_cost?: number;
  cost_estimated?: boolean;
  by_model?: ActivityModelUsage[];
  prompt_histogram: HistogramData | null;
  gen_histogram: HistogramData | null;
}

export interface ActivityModelUsage {
  model: string;
  requests: number;
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;
  cacheCreationTokens: number;
  cacheHitRatio?: number;
  cacheCreationRatio?: number;
  reasoningTokens: number;
  estimatedCost: number;
  costEstimated: boolean;
}

export interface VersionInfo {
  build_date: string;
  commit: string;
  version: string;
}

export interface HardwareSnapshot {
  schema_version: number;
  captured_at: string;
  capture: HardwareCapture;
  architecture: HardwareArchitecture;
  operating_system: HardwareOperatingSystem;
  environment: HardwareEnvironment;
  cpu: HardwareCPU;
  memory: HardwareMemory;
  accelerators: HardwareAccelerator[];
}

export interface HardwareCapture {
  scope: "inference_host";
  method: "detected" | "detected_and_edited" | "manual";
  detector: { name: string; version: string } | null;
}

export interface HardwareArchitecture {
  name: string;
  raw_name?: string | null;
}

export interface HardwareOperatingSystem {
  family: string;
  name: string | null;
  version: string | null;
  kernel: string | null;
  raw_family?: string | null;
}

export interface HardwareEnvironment {
  kind: string;
  name: string | null;
  version: string | null;
  raw_kind?: string | null;
}

export interface HardwareCPU {
  vendor: string | null;
  model: string | null;
  socket_count: number | null;
  physical_core_count: number | null;
  logical_thread_count: number | null;
}

export interface HardwareMemory {
  capacity_bytes: number;
}

export interface HardwareAccelerator {
  index: number;
  kind: "gpu" | "npu" | "other";
  raw_kind?: string | null;
  vendor: string | null;
  model: string | null;
  architecture: string | null;
  memory: {
    kind: "dedicated" | "unified" | "shared_system" | "unknown";
    capacity_bytes: number | null;
  };
  driver: { name: string | null; version: string | null } | null;
  power_limit_watts: number | null;
}

export type ScreenWidth = "xs" | "sm" | "md" | "lg" | "xl" | "2xl";

export type TextContentPart = {
  type: "text";
  text: string;
};

export type ImageContentPart = {
  type: "image_url";
  image_url: { url: string };
};

export type ContentPart = TextContentPart | ImageContentPart;

export interface ChatMessage {
  role: "user" | "assistant" | "system";
  content: string | ContentPart[];
  reasoning_content?: string;
  reasoningTimeMs?: number;
}

export function getTextContent(content: string | ContentPart[]): string {
  if (typeof content === "string") {
    return content;
  }
  const textParts = content.filter((part): part is TextContentPart => part.type === "text");
  return textParts.map((part) => part.text).join("\n");
}

export function getImageUrls(content: string | ContentPart[]): string[] {
  if (typeof content === "string") {
    return [];
  }
  return content
    .filter((part): part is ImageContentPart => part.type === "image_url")
    .map((part) => part.image_url.url);
}

export interface ChatCompletionRequest {
  model: string;
  messages: ChatMessage[];
  stream: boolean;
  temperature?: number;
  max_tokens?: number;
}

export interface ImageGenerationRequest {
  model: string;
  prompt: string;
  n?: number;
  size?: string;
}

export interface ImageGenerationResponse {
  created: number;
  data: Array<{
    url?: string;
    b64_json?: string;
  }>;
}

// SDAPI types (stable-diffusion.cpp)
export type ImageApiMode = "openai" | "sdapi";

export interface SdApiLora {
  name: string;
  path: string;
}

export interface SdApiLoraRef {
  path: string;
  multiplier: number;
}

export interface SdApiTxt2ImgRequest {
  model?: string;
  prompt: string;
  negative_prompt?: string;
  width?: number;
  height?: number;
  steps?: number;
  cfg_scale?: number;
  seed?: number;
  batch_size?: number;
  sampler_name?: string;
  scheduler?: string;
  lora?: SdApiLoraRef[];
}

export interface SdApiResponse {
  images: string[];
  parameters: Record<string, unknown>;
  info: string;
}

export interface AudioTranscriptionRequest {
  file: File;
  model: string;
}

export interface AudioTranscriptionResponse {
  text: string;
}

export interface SpeechGenerationRequest {
  model: string;
  input: string;
  voice: string;
}
