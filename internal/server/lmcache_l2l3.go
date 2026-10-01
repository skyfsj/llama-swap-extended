package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// lmcacheL3MinFreeBytes is the default free-disk requirement when
// lmcache.server.l3.maxBytes is unset: a 1 GiB floor keeps a misconfigured
// path (full disk, tmpfs with a tiny quota) from starting a cache that can
// never store anything.
const lmcacheL3MinFreeBytes = 1 << 30 // 1 GiB

// lmcacheServerArgs renders the real `lmcache server` CLI arguments for the
// effective configuration (LMCache 0.5.4 shape, verified in P0): the
// pinned-DRAM pool as --l1-size-gb, a float in GiB (LMCache multiplies the
// flag by 2^30), and the optional fs L2 adapter for the disk tier. A
// chunkSize of 0 means auto: the --chunk-size flag is omitted so the server
// applies its own library default.
func lmcacheServerArgs(eff config.LMCacheServerConfig) ([]string, error) {
	l2Bytes, err := eff.EffectiveL2MaxBytes()
	if err != nil {
		return nil, fmt.Errorf("lmcache.server.l2: %v", err)
	}
	gb := float64(l2Bytes) / float64(int64(1)<<30)
	args := []string{
		"server",
		"--host", eff.Host,
		"--port", strconv.Itoa(eff.Port),
		"--l1-size-gb", strconv.FormatFloat(gb, 'f', -1, 64),
		"--eviction-policy", eff.EvictionPolicy,
		"--http-host", eff.HTTPHost,
		"--http-port", strconv.Itoa(eff.HTTPPort),
	}
	if eff.ChunkSize > 0 {
		args = append(args, "--chunk-size", strconv.Itoa(eff.ChunkSize))
	}
	if eff.SeparateObjectGroups {
		args = append(args, "--separate-object-groups")
	}
	if eff.L3.Enabled {
		adapter, err := json.Marshal(map[string]string{"type": "fs", "base_path": eff.L3.Path})
		if err != nil {
			return nil, fmt.Errorf("lmcache.server.l3: %v", err)
		}
		args = append(args, "--l2-adapter", string(adapter))
	}
	return args, nil
}

// prepareL3Runtime runs the fail-closed disk-tier checks at spawn time:
// auto-create the path, probe writability, verify free disk space against
// l3.maxBytes (or the default floor). Any failure stops the server from
// starting with an explicit error — a broken disk tier must never be
// silently skipped.
func (svc *lmcacheService) prepareL3Runtime(eff config.LMCacheServerConfig) error {
	if !eff.L3.Enabled {
		return nil
	}
	path := eff.L3.Path
	if err := os.MkdirAll(path, 0o750); err != nil {
		return fmt.Errorf("lmcache L3: cannot create %s: %v", path, err)
	}
	probe := filepath.Join(path, fmt.Sprintf(".lmcache-write-probe-%d", os.Getpid()))
	f, err := os.Create(probe)
	if err != nil {
		return fmt.Errorf("lmcache L3: %s is not writable: %v", path, err)
	}
	_ = f.Close()
	_ = os.Remove(probe)
	required := int64(lmcacheL3MinFreeBytes)
	if mb := eff.L3.MaxBytes; mb != "" {
		if b, err := config.ParseByteSize(mb); err == nil {
			required = b
		}
	}
	free, err := svc.diskFree(path)
	if err != nil {
		return fmt.Errorf("lmcache L3: cannot stat %s: %v", path, err)
	}
	if free < required {
		return fmt.Errorf("lmcache L3: %s has only %d bytes free, need at least %d", path, free, required)
	}
	return nil
}

// l2UsageBytes fetches the pinned-DRAM usage from the management frontend
// (/status → storage_manager.l1_manager.memory_used_bytes). It returns nil
// when the number is unavailable (server not RUNNING, probe failure); the
// status endpoint renders nil as unavailable rather than a fake zero.
func (svc *lmcacheService) l2UsageBytes(cfg config.Config) *int64 {
	if svc.proc.Status().State != LMCacheStateRunning {
		return nil
	}
	eff := cfg.LMCache.Server.Effective()
	url := fmt.Sprintf("http://%s:%d/status", eff.HTTPHost, eff.HTTPPort)
	resp, err := svc.proc.healthClient.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var payload struct {
		StorageManager struct {
			L1Manager struct {
				MemoryUsedBytes int64 `json:"memory_used_bytes"`
			} `json:"l1_manager"`
		} `json:"storage_manager"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil
	}
	used := payload.StorageManager.L1Manager.MemoryUsedBytes
	return &used
}
