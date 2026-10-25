package mcpruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
)

type managerContextKey struct{}
type managerContext struct {
	manager *Manager
	owner   string
}

func WithManager(ctx context.Context, manager *Manager, owner string) context.Context {
	return context.WithValue(ctx, managerContextKey{}, managerContext{manager: manager, owner: owner})
}

type managedSession struct {
	owner      string
	ready      chan struct{}
	session    *Session
	err        error
	refs       int
	generation uint64
	lastUsed   time.Time
	timer      *time.Timer
	retired    bool
}
type Manager struct {
	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex
	sessions      map[string]*managedSession
	all           map[*managedSession]struct{}
	closed        bool
	idle          time.Duration
	nextOperation uint64
	operations    map[uint64]context.CancelFunc
	pins          map[string]*sessionPin
	done          chan struct{}
}

func NewManager(parent context.Context) *Manager {
	ctx, cancel := context.WithCancel(parent)
	manager := &Manager{ctx: ctx, cancel: cancel, sessions: map[string]*managedSession{}, all: map[*managedSession]struct{}{}, idle: 15 * time.Minute, operations: map[uint64]context.CancelFunc{}, pins: map[string]*sessionPin{}, done: make(chan struct{})}
	context.AfterFunc(ctx, manager.Close)
	return manager
}
func managerSessionKey(owner string, cfg config.MCPServerConfig) string {
	encoded, _ := json.Marshal(cfg)
	hash := sha256.New()
	hash.Write([]byte(owner))
	hash.Write([]byte{0})
	hash.Write(encoded)
	for _, name := range cfg.Env {
		hash.Write([]byte{0})
		hash.Write([]byte(name))
		hash.Write([]byte(os.Getenv(name)))
	}
	names := []string{}
	for _, name := range cfg.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		hash.Write([]byte{0})
		hash.Write([]byte(name))
		hash.Write([]byte(os.Getenv(name)))
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func (m *Manager) acquire(ctx context.Context, owner string, cfg config.MCPServerConfig) (*Session, error) {
	key := managerSessionKey(owner, cfg)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("MCP runtime is closed")
	}
	entry := m.sessions[key]
	if entry == nil {
		owned := 0
		for session := range m.all {
			if session.owner == owner {
				owned++
			}
		}
		if len(m.all) >= 128 || owned >= 32 {
			m.mu.Unlock()
			return nil, errors.New("MCP session capacity exceeded")
		}
		entry = &managedSession{owner: owner, ready: make(chan struct{}), refs: 1, lastUsed: time.Now()}
		m.sessions[key] = entry
		m.all[entry] = struct{}{}
		m.mu.Unlock()
		connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		stop := context.AfterFunc(m.ctx, cancel)
		session, err := connectSession(connectCtx, cfg, m.ctx)
		stop()
		cancel()
		m.mu.Lock()
		entry.session = session
		entry.err = err
		close(entry.ready)
		if err != nil || m.closed || entry.retired {
			if m.sessions[key] == entry {
				delete(m.sessions, key)
			}
			delete(m.all, entry)
			m.mu.Unlock()
			if session != nil {
				session.Close()
			}
			if err != nil {
				return nil, err
			}
			return nil, errors.New("MCP session was invalidated during connection")
		}
		m.mu.Unlock()
		go m.watch(key, entry)
		return m.lease(key, entry), nil
	}
	ready := entry.ready
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ready:
	}
	m.mu.Lock()
	if entry.err != nil {
		m.mu.Unlock()
		return nil, entry.err
	}
	if m.closed || entry.retired || m.sessions[key] != entry {
		m.mu.Unlock()
		return nil, errors.New("MCP session is no longer available")
	}
	entry.refs++
	entry.generation++
	if entry.timer != nil {
		entry.timer.Stop()
	}
	m.mu.Unlock()
	return m.lease(key, entry), nil
}
func (m *Manager) lease(key string, entry *managedSession) *Session {
	return &Session{ClientSession: entry.session.ClientSession, cancellation: entry.session.cancellation, httpCancellation: entry.session.httpCancellation, lifetime: m.ctx, manager: m, release: func() { m.release(key, entry) }}
}
func (m *Manager) watch(key string, entry *managedSession) {
	entry.session.ClientSession.Wait()
	m.mu.Lock()
	entry.retired = true
	if entry.timer != nil {
		entry.timer.Stop()
	}
	if m.sessions[key] == entry {
		delete(m.sessions, key)
	}
	delete(m.all, entry)
	m.mu.Unlock()
}
func (m *Manager) release(key string, entry *managedSession) {
	m.mu.Lock()
	entry.refs--
	entry.lastUsed = time.Now()
	entry.generation++
	generation := entry.generation
	if entry.refs == 0 {
		if entry.retired || m.closed {
			if m.sessions[key] == entry {
				delete(m.sessions, key)
			}
			delete(m.all, entry)
			m.mu.Unlock()
			entry.session.Close()
			return
		}
		entry.timer = time.AfterFunc(m.idle, func() {
			m.mu.Lock()
			if entry.refs != 0 || entry.generation != generation || m.sessions[key] != entry {
				m.mu.Unlock()
				return
			}
			delete(m.sessions, key)
			delete(m.all, entry)
			m.mu.Unlock()
			entry.session.Close()
		})
	}
	m.mu.Unlock()
}
func (m *Manager) Reset() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	pins := []string{}
	for id := range m.pins {
		pins = append(pins, id)
	}
	var closing []*Session
	for key, entry := range m.sessions {
		entry.retired = true
		delete(m.sessions, key)
		if entry.timer != nil {
			entry.timer.Stop()
		}
		if entry.refs == 0 && entry.session != nil {
			delete(m.all, entry)
			closing = append(closing, entry.session)
		}
	}
	m.mu.Unlock()
	for _, id := range pins {
		m.releasePin(id, nil)
	}
	for _, session := range closing {
		session.Close()
	}
}
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		<-m.done
		return
	}
	m.closed = true
	pins := []string{}
	for id := range m.pins {
		pins = append(pins, id)
	}
	var closing []*Session
	for entry := range m.all {
		entry.retired = true
		if entry.timer != nil {
			entry.timer.Stop()
		}
		if entry.session != nil {
			closing = append(closing, entry.session)
		}
	}
	m.sessions = map[string]*managedSession{}
	m.all = map[*managedSession]struct{}{}
	cancellations := []context.CancelFunc{}
	for _, cancel := range m.operations {
		cancellations = append(cancellations, cancel)
	}
	m.mu.Unlock()
	for _, cancel := range cancellations {
		cancel()
	}
	m.cancel()
	for _, id := range pins {
		m.releasePin(id, nil)
	}
	for _, session := range closing {
		session.Close()
	}
	close(m.done)
}

type lifetimeTransport struct {
	mcp.Transport
	lifetime context.Context
	cancel   context.CancelFunc
}

func (m *Manager) operation(ctx context.Context) (context.Context, func()) {
	operation, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return operation, func() {}
	}
	m.nextOperation++
	id := m.nextOperation
	m.operations[id] = cancel
	m.mu.Unlock()
	return operation, func() { cancel(); m.mu.Lock(); delete(m.operations, id); m.mu.Unlock() }
}
func (t *lifetimeTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	pinned, cancel := context.WithCancel(t.lifetime)
	t.cancel = cancel
	stop := context.AfterFunc(ctx, cancel)
	connection, err := t.Transport.Connect(pinned)
	detached := stop()
	if err != nil {
		cancel()
		return nil, err
	}
	if !detached && ctx.Err() != nil {
		cancel()
		connection.Close()
		return nil, ctx.Err()
	}
	return connection, nil
}
