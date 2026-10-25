package server

import (
	"context"
	"net/url"
	"reflect"

	"github.com/mostlygeek/llama-swap/internal/capcompat"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// refreshCapabilitiesOnReady schedules capability discovery for a model that
// just became ready, which is the only moment its upstream can be asked.
//
// This runs on the event dispatcher's consumer goroutine, and a consumer that
// falls behind eventually blocks the publisher, which here is the process
// state machine itself. So it must not do any work inline: the probe goes to
// its own goroutine and this returns immediately.
func (s *Server) refreshCapabilitiesOnReady(change swaputil.ProcessStateChangeEvent) {
	if change.NewState != string(process.StateReady) {
		return
	}
	mc, ok := s.currentConfig().Models[change.ProcessName]
	if !ok || mc.Capabilities.DisableAuto {
		return
	}
	go s.refreshCapabilities(change.ProcessName, mc)
}

// refreshCapabilities probes one ready model and caches what it reports.
//
// The probe dials the model's own proxy address rather than going through the
// local router. Routing a synthetic request through the router would reset the
// model's TTL idle window and occupy a concurrency slot, and could start a
// model that had already stopped again by the time this ran.
func (s *Server) refreshCapabilities(modelID string, mc config.ModelConfig) {
	base, err := url.Parse(mc.Proxy)
	if err != nil {
		s.proxylog.Debugf("capcompat: <%s> unusable proxy URL %q: %v", modelID, mc.Proxy, err)
		return
	}

	client := capcompat.NewClient(base, mc.Timeouts)
	key := capcompat.LocalKey(modelID, mc)
	name := capcompat.UpstreamModelName(modelID, mc)

	// What the model advertised before the probe, so a refresh that learns
	// nothing new does not push a redundant model list to every UI client.
	before := s.resolveCapabilities(s.shutdownCtx, modelID, mc)

	if err := s.capcompat.Refresh(s.shutdownCtx, key, client, name); err != nil {
		// Debug, not warn. llama-swap also fronts image, speech and
		// transcription servers with no capability surface, and a model that
		// stopped again mid-probe is normal. Neither is worth a warning on
		// every model start.
		s.proxylog.Debugf("capcompat: <%s> discovery failed: %v", modelID, err)
		return
	}

	// The listing pushed when this model went ready was built before the
	// probe finished, so anything new has to announce itself.
	if after := s.resolveCapabilities(s.shutdownCtx, modelID, mc); !reflect.DeepEqual(before, after) {
		event.Emit(swaputil.ModelCapabilitiesChangedEvent{ModelID: modelID})
	}
}

// forgetStaleCapabilities drops the memoized discovery result for every model
// whose cache key no longer applies: one that was removed from the
// configuration, or whose command, proxy or served name changed. The stored row
// ages out on its own; the in-memory memo would otherwise be retained for the
// life of the process.
func (s *Server) forgetStaleCapabilities(previous, next map[string]config.ModelConfig) {
	if s.capcompat == nil {
		return
	}
	for modelID, oldModel := range previous {
		newModel, stillConfigured := next[modelID]
		if stillConfigured && capcompat.LocalKey(modelID, oldModel) == capcompat.LocalKey(modelID, newModel) {
			continue
		}
		s.capcompat.Forget(capcompat.LocalKey(modelID, oldModel))
	}
}

// resolveCapabilities returns the capabilities to advertise for a local model:
// what the config sets, with anything it leaves unset filled in from the last
// successful probe.
//
// Configured values win field by field, so a model that hand-sets one field
// still benefits from discovery for the rest. A model with
// capabilities.disableAuto gets exactly what its config says.
func (s *Server) resolveCapabilities(ctx context.Context, modelID string, mc config.ModelConfig) config.ModelCapConfig {
	caps := mc.Capabilities
	if caps.DisableAuto {
		return caps
	}
	auto, found := s.capcompat.Lookup(ctx, capcompat.LocalKey(modelID, mc))
	if !found {
		return caps
	}
	return caps.Merge(auto)
}
