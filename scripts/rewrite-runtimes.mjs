import fs from 'node:fs';
const path='ui/src/routes/Runtimes.svelte';
let s=fs.readFileSync(path,'utf8');
s=s.replace('import { RefreshCw, Terminal }', 'import { CircleAlert, Plus, RefreshCw, Search, Server, Terminal, X }');
s=s.replace('  import * as Dialog', '  import { Input } from "$lib/components/ui/input/index.js";\n  import { filterRuntimes, runtimeHasUpdate, runtimeNeedsAttention, type RuntimeFilter } from "$lib/runtimeCenter";\n  import * as Dialog');
s=s.replace('  let busy = $state("");', `  let busy = $state<Record<string, string>>({});
  let listError = $state("");
  let settingsError = $state("");
  let listLoaded = $state(false);
  let search = $state("");
  let filter = $state<RuntimeFilter>("all");
  let detailLoading = $state<Record<string, boolean>>({});
  const detailRequests: Record<string, number> = {};
  let disposed = false;
  let stageRequestSerial = 0;`);
s=s.replace('  let selectedRuntime = $derived', `  let visibleRuntimes = $derived(filterRuntimes(userRuntimes, search, filter, readiness, readinessErrors));
  let attentionCount = $derived(userRuntimes.filter((runtime) => runtimeNeedsAttention(runtime, readiness[runtime.name], readinessErrors[runtime.name])).length);
  let updateCount = $derived(userRuntimes.filter(runtimeHasUpdate).length);

  $effect(() => { if (!stageOpen) stageRequestSerial++; });

  let selectedRuntime = $derived`);
s=s.replace('function runtimePolicy(name: string): string {\n    return String(asRecord(runtimeConfig(name)?.update).policy ?? "automatic");','function runtimePolicy(name: string): string {\n    if (!settingsSnapshot) return "";\n    return String(asRecord(runtimeConfig(name)?.update).policy ?? "automatic");');
const a=s.indexOf('  async function loadSettings()');
const b=s.indexOf('  async function loadLMCache()',a);
s=s.slice(0,a)+`  async function loadSettings(): Promise<void> {
    try {
      const snapshot = await fetchSettings();
      if (disposed) return;
      settingsSnapshot = snapshot;
      settingsError = "";
    } catch (cause) {
      if (disposed) return;
      settingsSnapshot = null;
      settingsError = message(cause);
    }
  }

`+s.slice(b);
s=s.replaceAll('dashboardRequest !== lmcacheDashboardRequest', 'disposed || dashboardRequest !== lmcacheDashboardRequest');
s=s.replace('      lmcacheDashboardError = "";\n    }\n  }','      lmcacheDashboardError = "";\n      lmcacheDashboardLoading = false;\n    }\n  }');
const c=s.indexOf('  async function loadDetail('),d=s.indexOf('  async function refresh()',c);
s=s.slice(0,c)+`  async function loadDetail(name: string): Promise<void> {
    const serial = (detailRequests[name] ?? 0) + 1;
    detailRequests[name] = serial;
    detailLoading[name] = true;
    detailErrors[name] = "";
    try {
      const detail = await fetchRuntimeDetail(name);
      if (!disposed && serial === detailRequests[name]) details[name] = detail;
    } catch (cause) {
      if (!disposed && serial === detailRequests[name]) detailErrors[name] = message(cause);
    } finally {
      if (!disposed && serial === detailRequests[name]) detailLoading[name] = false;
    }
  }

`+s.slice(d);
s=s.replace('if (runtimes.length === 0) loading = true;\n    error = "";', 'if (!listLoaded) loading = true;');
s=s.replaceAll('if (serial !== requestSerial)', 'if (disposed || serial !== requestSerial)');
s=s.replace('      runtimes = Array.isArray(runtimeResult.value.data)', '      listLoaded = true;\n      listError = "";\n      runtimes = Array.isArray(runtimeResult.value.data)');
s=s.replace('if (selectedName && runtimes.some', 'if (detailOpen && selectedName && runtimes.some');
s=s.replace('if (serial === requestSerial) error = message(cause);','if (!disposed && serial === requestSerial) listError = message(cause);');
s=s.replace('if (serial === requestSerial) {','if (!disposed && serial === requestSerial) {');
s=s.replace('    if (loadPromise) return loadPromise;', '    if (disposed) return Promise.resolve();\n    if (loadPromise) return loadPromise;');
s=s.replace('  function openDetails(name: string): void {', `  async function loadAfterAction(): Promise<void> {
    // An in-flight poll may predate the mutation. Fetch again after it settles.
    if (loadPromise) await loadPromise;
    await load();
  }

  function openDetails(name: string): void {`);
s=s.replace('  async function openStage(name: string): Promise<void> {\n', '  async function openStage(name: string): Promise<void> {\n    if (busy[name]) return;\n    const serial = ++stageRequestSerial;\n');
s=s.replace('      stageCandidates = Array.isArray(payload.data)', '      if (disposed || serial !== stageRequestSerial || !stageOpen || name !== stageRuntimeName) return;\n      stageCandidates = Array.isArray(payload.data)');
s=s.replace('      stageError = message(cause);\n    } finally {\n      stageLoading = false;', '      if (!disposed && serial === stageRequestSerial && stageOpen) stageError = message(cause);\n    } finally {\n      if (!disposed && serial === stageRequestSerial) stageLoading = false;');
const e=s.indexOf('  async function stageSelected('),f=s.indexOf('  function rollback(',e);
s=s.slice(0,e)+`  async function stageSelected(candidate: RuntimeVersionCandidate): Promise<void> {
    const name = stageRuntimeName;
    if (!name || stageLoading || !stageOpen || busy[name] || !stageCandidates.includes(candidate)) return;
    const serial = stageRequestSerial;
    busy[name] = "stage";
    stageError = "";
    try {
      await postRuntimeAction(name, "stage", { version: candidate.version, ref: candidate.ref, commit: candidate.commit, digest: candidate.digest });
      if (serial === stageRequestSerial) stageOpen = false;
      await loadAfterAction();
    } catch (cause) {
      if (serial === stageRequestSerial && stageOpen) stageError = message(cause);
      else error = message(cause);
    } finally {
      delete busy[name];
    }
  }

  async function operation(name: string, path: string, body?: unknown): Promise<void> {
    if (busy[name]) return;
    busy[name] = path;
    error = "";
    try {
      await postRuntimeAction(name, path, body);
      await loadAfterAction();
    } catch (cause) {
      error = message(cause);
    } finally {
      delete busy[name];
    }
  }

  async function activate(name: string, version: string): Promise<void> {
    if (!version || busy[name]) return;
    busy[name] = "readiness";
    error = "";
    try {
      const check = await fetchRuntimeReadiness(name, version);
      if (disposed) return;
      const blocked = check.checks.find((item) => item.level === "block");
      if (blocked) {
        error = \`\${blocked.title}: \${blocked.detail}\`;
        return;
      }
      askConfirm($translate("controlPlane.runtimeCenter.actions.activate"), $translate("controlPlane.runtimeActivateConfirm", { name, version }), $translate("controlPlane.runtimeCenter.actions.activate"), () => operation(name, \`activate/\${encodeURIComponent(version)}\`));
    } catch (cause) {
      error = $translate("controlPlane.runtimeCenter.readiness.loadError", { message: message(cause) });
    } finally {
      delete busy[name];
    }
  }

`+s.slice(f);
s=s.replace('      busy = `${name}:delete:${version}`;', '      if (busy[name]) return;\n      busy[name] = `delete:${version}`;\n      error = "";');
s=s.replace('        await load();\n        if (details[name]) await loadDetail(name);', '        await loadAfterAction();');
s=s.replace('        busy = "";', '        delete busy[name];');
s=s.replace('    if (config) openWizard("edit", name, config);','    if (config) openWizard("edit", name, config);\n    else error = $translate("controlPlane.runtimeCenter.wizard.settingsUnavailable");');
s=s.replace('    lmcacheBusy = true;', '    if (lmcacheBusy) return false;\n    lmcacheBusy = true;'); // bool only first action
s=s.replace('async function commitLMCacheRuntimeAction(path: string): Promise<void> {\n    if (lmcacheBusy) return false;', 'async function commitLMCacheRuntimeAction(path: string): Promise<void> {\n    if (lmcacheBusy) return;');
s=s.replaceAll('      await load();\n      return true;', '      await loadAfterAction();\n      return true;');
s=s.replace('await postRuntimeAction("lmcache", path);\n      await load();', 'await postRuntimeAction("lmcache", path);\n      await loadAfterAction();');
s=s.replace('    return () => window.clearInterval(timer);','    return () => {\n      window.clearInterval(timer);\n      disposed = true;\n      requestSerial++;\n      stageRequestSerial++;\n      lmcacheDashboardRequest++;\n    };');
const start=s.indexOf('<section class="space-y-4"');
const end=s.indexOf('\n<RuntimeDetailSheet',start);
s=s.slice(0,start)+fs.readFileSync('build/runtime-page-markup.svelte','utf8')+s.slice(end);
s=s.replace('  detail={selectedDetail}', '  detail={selectedDetail}\n  loading={detailLoading[selectedName] ?? false}\n  error={detailErrors[selectedName] || ""}\n  readinessError={readinessErrors[selectedName] || ""}\n  actionError={error}\n  editable={Boolean(settingsSnapshot?.writable)}\n  onRetry={() => selectedName && void loadDetail(selectedName)}');
s=s.replace('busy={selectedRuntime ? busy.startsWith(`${selectedRuntime.name}:`) : false}', 'busy={Boolean(selectedRuntime && busy[selectedRuntime.name])}');
s=s.replace('busy={busy === `${stageRuntimeName}:stage`}', 'busy={busy[stageRuntimeName] === "stage"}');
s=s.replace('onCommitted={() => { void load(); }}','onCommitted={() => { void loadAfterAction(); }}');
s=s.replace('</Dialog.Title></Dialog.Header>', '</Dialog.Title><Dialog.Description>{$translate("controlPlane.runtimeCenter.logs.description")}</Dialog.Description></Dialog.Header>');
fs.writeFileSync(path,s);
