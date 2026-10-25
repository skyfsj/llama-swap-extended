package mcpruntime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

type sessionPin struct {
	lease *Session
	timer *time.Timer
	once  sync.Once
}

func ScopeKey(ctx context.Context, cfg config.MCPServerConfig) string {
	scope, _ := ctx.Value(managerContextKey{}).(managerContext)
	return managerSessionKey(scope.owner, cfg)
}
func Pin(ctx context.Context, cfg config.MCPServerConfig, id string, expires time.Time) error {
	scope, _ := ctx.Value(managerContextKey{}).(managerContext)
	if scope.manager == nil {
		return errors.New("MCP session manager is unavailable")
	}
	return scope.manager.pin(ctx, scope.owner, cfg, id, expires)
}
func ReleasePin(ctx context.Context, id string) {
	scope, _ := ctx.Value(managerContextKey{}).(managerContext)
	if scope.manager != nil {
		scope.manager.releasePin(id, nil)
	}
}
func (m *Manager) pin(ctx context.Context, owner string, cfg config.MCPServerConfig, id string, expires time.Time) error {
	lease, err := m.acquire(ctx, owner, cfg)
	if err != nil {
		return err
	}
	pin := &sessionPin{lease: lease}
	m.mu.Lock()
	if m.closed || m.pins[id] != nil {
		m.mu.Unlock()
		lease.Close()
		return errors.New("MCP session pin is unavailable")
	}
	m.pins[id] = pin
	pin.timer = time.AfterFunc(time.Until(expires), func() { m.releasePin(id, pin) })
	m.mu.Unlock()
	return nil
}
func (m *Manager) releasePin(id string, expected *sessionPin) {
	m.mu.Lock()
	pin := m.pins[id]
	if pin == nil || expected != nil && pin != expected {
		m.mu.Unlock()
		return
	}
	delete(m.pins, id)
	m.mu.Unlock()
	pin.once.Do(func() { pin.timer.Stop(); pin.lease.Close() })
}
