// Package modeldownload implements a durable, resumable model repository
// download queue. Tasks are persisted by the existing SQLite store while
// partial bytes remain beside their final destination as .part files.
package modeldownload

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/modelmanager"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

const (
	ProviderHuggingFace = "huggingface"
	ProviderModelScope  = "modelscope"

	staleLease       = time.Minute
	workerPoll       = 500 * time.Millisecond
	progressInterval = 750 * time.Millisecond
	progressBytes    = 1 << 20
	maxMetadataBytes = 32 << 20
	maxErrorBody     = 2048
)

func normalizeContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// Logger is the small logging surface needed by the queue. It deliberately
// avoids coupling the downloader to the server's logger implementation.
type Logger interface {
	Warnf(format string, args ...any)
}

// Config wires the queue to the durable task store and model source catalog.
type Config struct {
	Store    *store.Store
	Sources  *modelmanager.Manager
	Settings config.ModelDownloadsConfig
	Client   *http.Client
	Logger   Logger
	Progress func(swaputil.BackendProgressEvent)
}

// Request is the user-controlled part of a repository download task.
// Include/exclude patterns use path.Match-style globs against repository file
// names. Secrets are never accepted here; the queue resolves the selected
// provider's configured token or environment fallback at execution time.
type Request struct {
	Provider string
	RepoID   string
	Revision string
	SourceID string
	Include  []string
	Exclude  []string
}

// Manager owns queue workers. It can be started before the HTTP routes are
// served and closed before the SQLite store is closed.
type Manager struct {
	store     *store.Store
	sources   *modelmanager.Manager
	config    config.ModelDownloadsConfig
	providers map[string]downloadProvider
	logger    Logger
	progress  func(swaputil.BackendProgressEvent)

	mu      sync.Mutex
	started bool
	ctx     context.Context
	cancel  context.CancelFunc
	wake    chan struct{}
	wg      sync.WaitGroup
	active  map[string]context.CancelFunc
	ownerID string
	targets map[string]*sync.Mutex
}

type downloadProvider struct {
	name      string
	cacheType string
	baseURL   string
	tokenEnv  string
	token     string
	client    *http.Client
}

// retryableDownloadError marks an exhausted transient network or provider
// failure. The task worker uses it to schedule a later task-level retry;
// permanent validation, authorization, and integrity errors remain terminal.
type retryableDownloadError struct{ err error }

func (e *retryableDownloadError) Error() string { return e.err.Error() }
func (e *retryableDownloadError) Unwrap() error { return e.err }

func transientDownloadError(err error) error {
	if err == nil {
		return nil
	}
	var retryable *retryableDownloadError
	if errors.As(err, &retryable) {
		return err
	}
	return &retryableDownloadError{err: err}
}

func isRetryableTaskError(err error) bool {
	var retryable *retryableDownloadError
	return errors.As(err, &retryable)
}

// New validates the queue wiring and snapshots its configuration.
func New(c Config) (*Manager, error) {
	if c.Store == nil {
		return nil, errors.New("model download queue requires a store")
	}
	if c.Sources == nil {
		return nil, errors.New("model download queue requires model sources")
	}
	settings := c.Settings.Effective()
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	providers := make(map[string]downloadProvider, 2)
	for _, provider := range []downloadProvider{
		{name: ProviderHuggingFace, cacheType: config.ModelFileSourceHFCache, baseURL: settings.HFBaseURL, tokenEnv: settings.HFTokenEnv, token: settings.HFToken},
		{name: ProviderModelScope, cacheType: config.ModelFileSourceMSCache, baseURL: settings.ModelScopeBaseURL, tokenEnv: settings.ModelScopeTokenEnv, token: settings.ModelScopeToken},
	} {
		baseURL, err := normalizeBaseURL(provider.name, provider.baseURL)
		if err != nil {
			return nil, err
		}
		// A local/private mirror is a supported deployment shape, so private IPs
		// are intentionally allowed for the configured base URL. Metadata hosts
		// remain forbidden because provider tokens are attached to these requests.
		if parsed, parseErr := url.Parse(baseURL); parseErr != nil || isMetadataDownloadHost(parsed.Hostname()) {
			return nil, fmt.Errorf("%s model download base URL must not target a metadata host", provider.name)
		}
		provider.baseURL = baseURL
		provider.client = guardedDownloadClient(c.Client, baseURL)
		providers[provider.name] = provider
	}
	return &Manager{
		store:     c.Store,
		sources:   c.Sources,
		config:    settings,
		providers: providers,
		logger:    c.Logger,
		progress:  c.Progress,
		wake:      make(chan struct{}, 1),
		active:    make(map[string]context.CancelFunc),
		ownerID:   newID("queue"),
		targets:   make(map[string]*sync.Mutex),
	}, nil
}

// guardedDownloadClient copies an injected HTTP client and validates every
// redirect before net/http follows it. Hugging Face commonly redirects model
// blobs to a CDN, so public cross-host redirects remain allowed; destinations
// that resolve lexically to loopback/private/metadata addresses are rejected.
// A configured local mirror may redirect within its own hostname, but it may
// not use an open redirect to reach a different private host.
func guardedDownloadClient(source *http.Client, baseURL string) *http.Client {
	if source == nil {
		source = &http.Client{}
	} else {
		copyClient := *source
		source = &copyClient
	}
	base, _ := url.Parse(strings.TrimSpace(baseURL))
	originalRedirect := source.CheckRedirect
	source.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if originalRedirect != nil {
			if err := originalRedirect(req, via); err != nil {
				return err
			}
		}
		if req == nil || req.URL == nil || !downloadURLAllowed(req.URL) {
			return errors.New("model download redirect is not allowed")
		}
		if len(via) >= 10 {
			return errors.New("model download followed too many redirects")
		}
		if base != nil && !downloadRedirectAllowed(base, req.URL) {
			return errors.New("model download redirect targets a private or local host")
		}
		return nil
	}
	return source
}

func downloadURLAllowed(value *url.URL) bool {
	return value != nil && (strings.EqualFold(value.Scheme, "http") || strings.EqualFold(value.Scheme, "https")) && value.Host != "" && value.User == nil
}

func downloadRedirectAllowed(base, destination *url.URL) bool {
	if !downloadURLAllowed(base) || !downloadURLAllowed(destination) {
		return false
	}
	// A Hugging Face request may carry HF_TOKEN in Authorization. Never let a
	// secure base URL downgrade to clear-text HTTP, even when the hostname is
	// unchanged; net/http otherwise treats that as an ordinary same-host
	// redirect and the token could leave the TLS boundary.
	if strings.EqualFold(base.Scheme, "https") && !strings.EqualFold(destination.Scheme, "https") {
		return false
	}
	baseHost := normalizeDownloadHost(base.Hostname())
	destinationHost := normalizeDownloadHost(destination.Hostname())
	if baseHost == "" || destinationHost == "" {
		return false
	}
	if strings.EqualFold(baseHost, destinationHost) {
		return true
	}
	return !restrictedDownloadHost(destinationHost)
}

func normalizeDownloadHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

func restrictedDownloadHost(host string) bool {
	host = normalizeDownloadHost(host)
	if host == "" {
		return true
	}
	if ip, ok := downloadIPLiteral(host); ok {
		return restrictedDownloadIP(ip)
	}
	if strings.ContainsRune(host, '%') {
		// '%' is not valid in a DNS label. If it was intended as an IPv6 zone
		// marker but could not be parsed, fail closed instead of treating the
		// malformed literal as a public hostname.
		return true
	}
	// A dotted numeric hostname with a leading zero is ambiguous across URL
	// parsers: one may treat 0127 as octal while another treats it as decimal.
	// Evaluate every accepted interpretation and reject the host when any one
	// reaches a private destination.
	for _, ip := range numericDownloadIPv4Candidates(host) {
		if restrictedDownloadIP(ip) {
			return true
		}
	}
	switch {
	case host == "localhost", strings.HasSuffix(host, ".localhost"), host == "ip6-localhost":
		return true
	case isMetadataDownloadHost(host):
		return true
	case host == "host.docker.internal", host == "kubernetes.default.svc", strings.HasSuffix(host, ".cluster.local"), strings.HasSuffix(host, ".svc"):
		return true
	default:
		return false
	}
}

// isMetadataDownloadHost identifies the well-known metadata aliases without
// treating an operator's ordinary private mirror as forbidden. The host is
// normalized at the call sites, but normalizing again keeps this helper safe
// for direct tests and future callers.
func isMetadataDownloadHost(host string) bool {
	host = normalizeDownloadHost(host)
	if ip, ok := downloadIPLiteral(host); ok {
		return isMetadataDownloadIP(ip)
	}
	if strings.ContainsRune(host, '%') {
		return true
	}
	for _, ip := range numericDownloadIPv4Candidates(host) {
		if isMetadataDownloadIP(ip) {
			return true
		}
	}
	switch host {
	case "metadata", "metadata.google.internal", "instance-data":
		return true
	default:
		return false
	}
}

func isMetadataDownloadIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// These are provider-reserved metadata destinations. A private mirror is
	// otherwise supported, but these addresses must never be accepted as the
	// configured base endpoint because the first request carries HF credentials.
	for _, reserved := range []string{
		"169.254.169.254", // AWS/GCP/Azure-compatible IPv4 metadata address
		"169.254.170.2",   // ECS task metadata
		"100.100.100.200", // Alibaba Cloud metadata
		"fd00:ec2::254",   // AWS IPv6 metadata endpoint
	} {
		if ip.Equal(net.ParseIP(reserved)) {
			return true
		}
	}
	return false
}

// downloadIPLiteral parses an IPv4/IPv6 literal, including the zone suffix
// accepted in bracketed IPv6 URLs (for example [fe80::1%25eth0]). The zone is
// not part of the address and therefore must not weaken private-destination
// checks.
func downloadIPLiteral(host string) (net.IP, bool) {
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]") {
		if len(host) < 2 || host[0] != '[' || host[len(host)-1] != ']' {
			return nil, false
		}
		host = host[1 : len(host)-1]
	}
	if host == "" {
		return nil, false
	}
	if zone := strings.LastIndexByte(host, '%'); zone >= 0 {
		if zone == 0 || zone == len(host)-1 {
			return nil, false
		}
		host = host[:zone]
	}
	ip := net.ParseIP(host)
	return ip, ip != nil
}

func restrictedDownloadIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		cgnat := v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
		return v4.IsLoopback() || v4.IsPrivate() || v4.IsLinkLocalUnicast() || v4.IsUnspecified() || v4.IsMulticast() || cgnat
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// numericDownloadIPv4 recognizes the legacy numeric IPv4 spellings accepted
// by URL/network libraries (decimal/hex/octal and abbreviated dotted forms).
// Treating them as IP literals closes redirects such as 2130706433 to
// 127.0.0.1 that would otherwise look like an ordinary public hostname.
func numericDownloadIPv4(host string) (net.IP, bool) {
	candidates := numericDownloadIPv4Candidates(host)
	if len(candidates) == 0 {
		return nil, false
	}
	return candidates[0], true
}

// numericDownloadIPv4Candidates returns all IPv4 interpretations of a legacy
// numeric hostname. The first candidate preserves the historical base-0
// interpretation used by numericDownloadIPv4; additional candidates cover
// decimal parsers for components such as 08 and 0127. At most 16 combinations
// can exist (four dotted components with two interpretations each).
func numericDownloadIPv4Candidates(host string) []net.IP {
	parts := strings.Split(host, ".")
	if len(parts) < 1 || len(parts) > 4 {
		return nil
	}
	values := make([][]uint64, len(parts))
	for i, part := range parts {
		if part == "" {
			return nil
		}
		values[i] = parseDownloadIPv4Component(part)
		if len(values[i]) == 0 {
			return nil
		}
	}
	candidates := make([]net.IP, 0, 16)
	var visit func(int, []uint64)
	visit = func(index int, selected []uint64) {
		if index == len(values) {
			if ip, ok := composeDownloadIPv4(selected); ok {
				for _, existing := range candidates {
					if existing.Equal(ip) {
						return
					}
				}
				candidates = append(candidates, ip)
			}
			return
		}
		for _, value := range values[index] {
			visit(index+1, append(selected, value))
		}
	}
	visit(0, nil)
	return candidates
}

func parseDownloadIPv4Component(part string) []uint64 {
	values := make([]uint64, 0, 2)
	if value, err := strconv.ParseUint(part, 0, 32); err == nil {
		values = append(values, value)
	}
	if value, err := strconv.ParseUint(part, 10, 32); err == nil {
		for _, existing := range values {
			if existing == value {
				return values
			}
		}
		values = append(values, value)
	}
	return values
}

func composeDownloadIPv4(values []uint64) (net.IP, bool) {
	if len(values) < 1 || len(values) > 4 {
		return nil, false
	}
	var number uint64
	switch len(values) {
	case 1:
		if values[0] > 0xffffffff {
			return nil, false
		}
		number = values[0]
	case 2:
		if values[0] > 0xff || values[1] > 0xffffff {
			return nil, false
		}
		number = values[0]<<24 | values[1]
	case 3:
		if values[0] > 0xff || values[1] > 0xff || values[2] > 0xffff {
			return nil, false
		}
		number = values[0]<<24 | values[1]<<16 | values[2]
	case 4:
		for _, value := range values {
			if value > 0xff {
				return nil, false
			}
		}
		number = values[0]<<24 | values[1]<<16 | values[2]<<8 | values[3]
	}
	return net.IPv4(byte(number>>24), byte(number>>16), byte(number>>8), byte(number)), true
}

// Start recovers stale leases and starts the configured number of workers.
// A disabled queue still accepts no new work from the server but leaves
// existing tasks durable for a later enabled configuration.
func (m *Manager) Start(parent context.Context) error {
	if m == nil {
		return errors.New("model download queue is nil")
	}
	parent = normalizeContext(parent)
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	m.ctx = ctx
	m.cancel = cancel
	m.started = true
	enabled := m.config.Enabled == nil || *m.config.Enabled
	m.mu.Unlock()
	if !enabled {
		return nil
	}
	recoverCtx, recoverCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := m.store.RequeueStaleModelDownloads(recoverCtx, time.Now().UTC().Add(-staleLease))
	recoverCancel()
	if err != nil {
		m.Close()
		return fmt.Errorf("recovering model download queue: %w", err)
	}
	workers := m.config.Workers
	if workers < 1 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		m.wg.Add(1)
		go m.workerLoop(fmt.Sprintf("%s-%d", m.ownerID, i+1))
	}
	return nil
}

// Close stops workers without discarding .part files. Tasks owned by this
// process are requeued by the worker so a later process can resume them.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return
	}
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
}

// Enqueue validates a request, resolves its destination from the configured
// source catalog, and persists it before waking a worker. Active duplicates
// are coalesced so two UI clicks cannot download the same repository twice.
func (m *Manager) Enqueue(ctx context.Context, request Request) (store.ModelDownloadTask, bool, error) {
	if m == nil {
		return store.ModelDownloadTask{}, false, errors.New("model download queue is nil")
	}
	if m.config.Enabled != nil && !*m.config.Enabled {
		return store.ModelDownloadTask{}, false, errors.New("model download queue is disabled")
	}
	ctx = normalizeContext(ctx)
	request, err := normalizeRequest(request)
	if err != nil {
		return store.ModelDownloadTask{}, false, err
	}
	provider, err := m.provider(request.Provider)
	if err != nil {
		return store.ModelDownloadTask{}, false, err
	}
	destination, err := m.sources.ResolveDownloadSourceForProvider(request.SourceID, provider.cacheType)
	if err != nil {
		return store.ModelDownloadTask{}, false, err
	}
	request.SourceID = destination.ID
	active, found, err := m.store.FindActiveModelDownload(ctx, request.Provider, request.RepoID, request.Revision, request.SourceID, request.Include, request.Exclude)
	if err != nil {
		return store.ModelDownloadTask{}, false, err
	}
	if found {
		return active, true, nil
	}
	task := store.ModelDownloadTask{
		ID:       newID("download"),
		Provider: request.Provider,
		RepoID:   request.RepoID,
		Revision: request.Revision,
		SourceID: request.SourceID,
		Include:  request.Include,
		Exclude:  request.Exclude,
		Status:   store.ModelDownloadQueued,
	}
	if err := m.store.CreateModelDownload(ctx, task); err != nil {
		return store.ModelDownloadTask{}, false, err
	}
	m.signal()
	return task, false, nil
}

func (m *Manager) List(ctx context.Context, limit, offset int) ([]store.ModelDownloadTask, error) {
	return m.store.ListModelDownloads(normalizeContext(ctx), limit, offset)
}

func (m *Manager) Get(ctx context.Context, id string) (store.ModelDownloadTask, bool, error) {
	return m.store.GetModelDownload(normalizeContext(ctx), id)
}

// Cancel marks a task canceled before canceling its in-process context. The
// order prevents a worker from converting a user cancellation into a retryable
// queued state during the short race between the two operations.
func (m *Manager) Cancel(ctx context.Context, id string) error {
	ctx = normalizeContext(ctx)
	id = strings.TrimSpace(id)
	if err := m.store.CancelModelDownload(ctx, id); err != nil {
		return err
	}
	m.mu.Lock()
	cancel := m.active[id]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.signal()
	return nil
}

func (m *Manager) Retry(ctx context.Context, id string) error {
	ctx = normalizeContext(ctx)
	if err := m.store.RetryModelDownload(ctx, strings.TrimSpace(id)); err != nil {
		return err
	}
	m.signal()
	return nil
}

// Delete removes terminal queue history after confirming that no worker is
// still shutting the task down. Model files and resumable .part files remain
// untouched; they are storage data rather than queue metadata.
func (m *Manager) Delete(ctx context.Context, id string) error {
	if m == nil {
		return errors.New("model download queue is nil")
	}
	ctx = normalizeContext(ctx)
	id = strings.TrimSpace(id)
	m.mu.Lock()
	_, active := m.active[id]
	m.mu.Unlock()
	if active {
		return store.ErrModelDownloadNotTerminal
	}
	return m.store.DeleteModelDownload(ctx, id)
}

func (m *Manager) workerLoop(workerID string) {
	defer m.wg.Done()
	for {
		if m.ctx.Err() != nil {
			return
		}
		task, found, err := m.store.ClaimNextModelDownload(m.ctx, workerID, time.Now().UTC())
		if err != nil {
			if m.ctx.Err() != nil {
				return
			}
			m.warnf("claiming model download failed: %v", err)
			if !m.waitForWork() {
				return
			}
			continue
		}
		if !found {
			if !m.waitForWork() {
				return
			}
			continue
		}

		taskCtx, taskCancel := context.WithCancel(m.ctx)
		m.mu.Lock()
		m.active[task.ID] = taskCancel
		m.mu.Unlock()
		if current, currentFound, currentErr := m.store.GetModelDownload(context.Background(), task.ID); currentErr == nil && currentFound && current.Status == store.ModelDownloadCanceled {
			taskCancel()
		}
		unlockTarget := m.lockTarget(task)
		err = m.processTask(taskCtx, workerID, task)
		unlockTarget()
		taskCancel()
		m.mu.Lock()
		delete(m.active, task.ID)
		m.mu.Unlock()
		latest, found, latestErr := m.store.GetModelDownload(context.Background(), task.ID)
		if !found || latestErr != nil {
			latest = task
		}

		switch {
		case err == nil:
			m.emitProgress(task, "completed", 1, "model download completed", nil)
			if finishErr := m.store.FinishModelDownload(context.Background(), task.ID, workerID, store.ModelDownloadCompleted, "", latest.TotalFiles, latest.DownloadedBytes); finishErr != nil && !errors.Is(finishErr, store.ErrModelDownloadNotOwned) {
				m.warnf("finishing model download %s failed: %v", task.ID, finishErr)
			}
		case errors.Is(err, context.Canceled):
			m.emitProgress(task, "canceled", 0, "model download canceled", err)
			if m.ctx.Err() != nil {
				if requeueErr := m.store.RequeueOwnedModelDownload(context.Background(), task.ID, workerID); requeueErr != nil && !errors.Is(requeueErr, store.ErrModelDownloadNotOwned) {
					m.warnf("requeueing model download %s failed: %v", task.ID, requeueErr)
				}
				return
			}
			// A user cancellation already changed the row to canceled. If a
			// future cancellation source is added, this branch still leaves a
			// terminal task rather than silently retrying it.
			if finishErr := m.store.FinishModelDownload(context.Background(), task.ID, workerID, store.ModelDownloadCanceled, "canceled", latest.CompletedFiles, latest.DownloadedBytes); finishErr != nil && !errors.Is(finishErr, store.ErrModelDownloadNotOwned) {
				m.warnf("canceling model download %s failed: %v", task.ID, finishErr)
			}
		case isRetryableTaskError(err) && task.Attempts <= m.config.MaxTaskRetries:
			delay := m.taskRetryDelay(task.Attempts)
			// SQLite stores queue timestamps as whole seconds, so a shorter delay
			// would round down to an immediately claimable retry.
			if delay < time.Second {
				delay = time.Second
			}
			retryAt := time.Now().UTC().Add(delay).Truncate(time.Second)
			message := fmt.Sprintf("temporary download failure; retrying automatically (%d/%d): %v", task.Attempts, m.config.MaxTaskRetries, err)
			if retryErr := m.store.RetryOwnedModelDownload(context.Background(), task.ID, workerID, message, retryAt); retryErr != nil && !errors.Is(retryErr, store.ErrModelDownloadNotOwned) {
				m.warnf("scheduling automatic retry for model download %s failed: %v", task.ID, retryErr)
				if finishErr := m.store.FinishModelDownload(context.Background(), task.ID, workerID, store.ModelDownloadFailed, err.Error(), latest.CompletedFiles, latest.DownloadedBytes); finishErr != nil && !errors.Is(finishErr, store.ErrModelDownloadNotOwned) {
					m.warnf("recording model download %s failure failed: %v", task.ID, finishErr)
				}
				continue
			}
			m.emitProgress(task, "retrying", 0, message, err)
		default:
			m.emitProgress(task, "error", 0, "model download failed", err)
			if finishErr := m.store.FinishModelDownload(context.Background(), task.ID, workerID, store.ModelDownloadFailed, err.Error(), latest.CompletedFiles, latest.DownloadedBytes); finishErr != nil && !errors.Is(finishErr, store.ErrModelDownloadNotOwned) {
				m.warnf("recording model download %s failure failed: %v", task.ID, finishErr)
			}
		}
	}
}

func (m *Manager) taskRetryDelay(attempt int) time.Duration {
	delay := m.config.RetryBackoff
	for i := 1; i < attempt && delay < time.Minute; i++ {
		delay *= 2
	}
	if delay > time.Minute {
		return time.Minute
	}
	return delay
}

func (m *Manager) lockTarget(task store.ModelDownloadTask) func() {
	key := task.Provider + "\x00" + task.SourceID + "\x00" + task.RepoID
	m.mu.Lock()
	lock := m.targets[key]
	if lock == nil {
		lock = &sync.Mutex{}
		m.targets[key] = lock
	}
	m.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (m *Manager) provider(name string) (downloadProvider, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	provider, ok := m.providers[name]
	if !ok {
		return downloadProvider{}, fmt.Errorf("unsupported model download provider %q", name)
	}
	return provider, nil
}

func (m *Manager) waitForWork() bool {
	timer := time.NewTimer(workerPoll)
	defer timer.Stop()
	select {
	case <-m.ctx.Done():
		return false
	case <-m.wake:
		return true
	case <-timer.C:
		return true
	}
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) warnf(format string, args ...any) {
	if m.logger != nil {
		m.logger.Warnf(format, args...)
	}
}

func (m *Manager) emitProgress(task store.ModelDownloadTask, phase string, progress float64, message string, progressErr error) {
	m.emitProgressBytes(task, phase, progress, 0, 0, message, progressErr)
}

func (m *Manager) emitProgressBytes(task store.ModelDownloadTask, phase string, progress float64, completed, total int64, message string, progressErr error) {
	if m == nil || m.progress == nil {
		return
	}
	if progress < 0 {
		progress = 0
	}
	if progress > 1 {
		progress = 1
	}
	event := swaputil.BackendProgressEvent{
		Model:     task.RepoID,
		Phase:     phase,
		Progress:  progress,
		Completed: completed,
		Total:     total,
		Message:   message,
	}
	if progressErr != nil {
		event.Error = progressErr.Error()
	}
	m.progress(event)
}

type downloadWorkItem struct {
	filename     string
	fileURL      string
	final        string
	snapshot     string
	expectedSize int64
	expectedSHA  string
}

type taskDownloadProgress struct {
	manager       *Manager
	ctx           context.Context
	workerID      string
	task          store.ModelDownloadTask
	totalFiles    int
	totalBytes    int64
	progressTotal int64

	mu               sync.Mutex
	fileBytes        map[string]int64
	completed        map[string]struct{}
	active           map[string]struct{}
	lastPersist      time.Time
	lastPersistBytes int64
}

func newTaskDownloadProgress(ctx context.Context, manager *Manager, workerID string, task store.ModelDownloadTask, totalFiles int, totalBytes int64) *taskDownloadProgress {
	progressTotal := totalBytes
	if progressTotal <= 0 {
		progressTotal = -1
	}
	return &taskDownloadProgress{
		manager:       manager,
		ctx:           ctx,
		workerID:      workerID,
		task:          task,
		totalFiles:    totalFiles,
		totalBytes:    totalBytes,
		progressTotal: progressTotal,
		fileBytes:     make(map[string]int64, totalFiles),
		completed:     make(map[string]struct{}, totalFiles),
		active:        make(map[string]struct{}, totalFiles),
	}
}

func (p *taskDownloadProgress) activate(filename string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active[filename] = struct{}{}
	return p.persistLocked(true)
}

func (p *taskDownloadProgress) update(filename string, value int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if value < 0 {
		value = 0
	}
	p.fileBytes[filename] = value
	return p.persistLocked(false)
}

func (p *taskDownloadProgress) complete(filename string, value int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if value < 0 {
		value = 0
	}
	p.fileBytes[filename] = value
	p.completed[filename] = struct{}{}
	delete(p.active, filename)
	return p.persistLocked(true)
}

func (p *taskDownloadProgress) deactivate(filename string) {
	p.mu.Lock()
	delete(p.active, filename)
	p.mu.Unlock()
}

func (p *taskDownloadProgress) persistLocked(force bool) error {
	downloadedBytes := int64(0)
	for _, value := range p.fileBytes {
		downloadedBytes = saturatingDownloadBytes(downloadedBytes, value)
	}
	byteDelta := downloadedBytes - p.lastPersistBytes
	if byteDelta < 0 {
		byteDelta = -byteDelta
	}
	if !force && byteDelta < progressBytes && time.Since(p.lastPersist) < progressInterval {
		return nil
	}
	activeFiles := make([]string, 0, len(p.active))
	for filename := range p.active {
		activeFiles = append(activeFiles, filename)
	}
	sort.Strings(activeFiles)
	currentFiles := strings.Join(activeFiles, " · ")
	completedFiles := len(p.completed)
	value := float64(completedFiles)
	if p.totalBytes > 0 {
		value = float64(downloadedBytes) / float64(p.totalBytes)
	} else if p.totalFiles > 0 {
		value /= float64(p.totalFiles)
	}
	if err := p.manager.store.UpdateModelDownloadProgress(p.ctx, p.task.ID, p.workerID, currentFiles, completedFiles, p.totalFiles, downloadedBytes, p.totalBytes); err != nil {
		return err
	}
	p.lastPersist = time.Now()
	p.lastPersistBytes = downloadedBytes
	p.manager.emitProgressBytes(p.task, "downloading", value, downloadedBytes, p.progressTotal, currentFiles, nil)
	return nil
}

func (m *Manager) processTask(ctx context.Context, workerID string, task store.ModelDownloadTask) error {
	ctx = normalizeContext(ctx)
	m.emitProgress(task, "checking", 0, "resolving model metadata", nil)
	provider, err := m.provider(task.Provider)
	if err != nil {
		return err
	}
	destination, err := m.sources.ResolveDownloadSourceForProvider(task.SourceID, provider.cacheType)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destination.Path, 0o755); err != nil {
		return fmt.Errorf("create download source %s: %w", destination.Path, err)
	}
	info, err := m.fetchModelInfo(ctx, provider, task.RepoID, task.Revision)
	if err != nil {
		return err
	}
	siblings, err := selectSiblings(info.Siblings, task.Include, task.Exclude)
	if err != nil {
		return err
	}
	totalBytes := int64(0)
	for _, sibling := range siblings {
		size, _, err := siblingExpected(sibling)
		if err != nil {
			return err
		}
		if size > 0 {
			totalBytes = saturatingDownloadBytes(totalBytes, size)
		}
	}
	if err := m.store.UpdateModelDownloadManifest(ctx, task.ID, workerID, len(siblings), totalBytes); err != nil {
		return err
	}
	m.emitProgress(task, "downloading", 0, fmt.Sprintf("downloading %d model files", len(siblings)), nil)

	resolvedRevision := info.SHA
	if resolvedRevision == "" {
		resolvedRevision = task.Revision
	}
	items := make([]downloadWorkItem, 0, len(siblings))
	for _, sibling := range siblings {
		filename, err := safeRepositoryPath(sibling.RFilename)
		if err != nil {
			return err
		}
		size, expectedSHA, err := siblingExpected(sibling)
		if err != nil {
			return err
		}
		item := downloadWorkItem{
			filename:     filename,
			fileURL:      resolveFileURL(provider, task.RepoID, task.Revision, filename),
			expectedSize: size,
			expectedSHA:  expectedSHA,
		}
		switch destination.Type {
		case config.ModelFileSourceHFCache:
			cacheTarget, targetErr := hfCacheTarget(destination.Path, task.RepoID, resolvedRevision, task.Revision, filename, expectedSHA)
			if targetErr != nil {
				return targetErr
			}
			item.final = cacheTarget.Blob
			item.snapshot = cacheTarget.Snapshot
		case config.ModelFileSourceMSCache:
			finalPath, targetErr := modelScopeCacheTarget(destination.Path, task.RepoID, resolvedRevision, task.Revision, filename)
			if targetErr != nil {
				return targetErr
			}
			item.final = finalPath
		default:
			finalPath, targetErr := directoryTarget(destination.Path, task.RepoID, filename)
			if targetErr != nil {
				return targetErr
			}
			item.final = finalPath
		}
		items = append(items, item)
	}

	if len(items) > 0 {
		downloadCtx, cancelDownloads := context.WithCancel(ctx)
		defer cancelDownloads()
		progress := newTaskDownloadProgress(downloadCtx, m, workerID, task, len(items), totalBytes)
		jobs := make(chan downloadWorkItem)
		errorsCh := make(chan error, 1)
		fileWorkers := m.config.FileWorkers
		if fileWorkers < 1 {
			fileWorkers = 1
		}
		if fileWorkers > len(items) {
			fileWorkers = len(items)
		}

		var targetMu sync.Mutex
		targets := make(map[string]*sync.Mutex, len(items))
		lockTarget := func(path string) func() {
			targetMu.Lock()
			lock := targets[path]
			if lock == nil {
				lock = &sync.Mutex{}
				targets[path] = lock
			}
			targetMu.Unlock()
			lock.Lock()
			return lock.Unlock
		}

		var workers sync.WaitGroup
		for range fileWorkers {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for item := range jobs {
					if downloadCtx.Err() != nil {
						return
					}
					unlock := lockTarget(item.final)
					if downloadCtx.Err() != nil {
						unlock()
						return
					}
					err := progress.activate(item.filename)
					var actual int64
					if err == nil {
						actual, err = m.downloadFile(downloadCtx, provider, item.fileURL, item.final, item.expectedSize, item.expectedSHA, func(value int64) error {
							return progress.update(item.filename, value)
						})
					}
					if err == nil && item.snapshot != "" {
						err = ensureSnapshotEntry(item.final, item.snapshot)
					}
					if err == nil {
						err = progress.complete(item.filename, actual)
					} else {
						progress.deactivate(item.filename)
					}
					unlock()
					if err != nil {
						select {
						case errorsCh <- fmt.Errorf("download %s: %w", item.filename, err):
							cancelDownloads()
						default:
						}
						return
					}
				}
			}()
		}

	feedJobs:
		for _, item := range items {
			select {
			case jobs <- item:
			case <-downloadCtx.Done():
				break feedJobs
			}
		}
		close(jobs)
		workers.Wait()
		select {
		case downloadErr := <-errorsCh:
			return downloadErr
		default:
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if destination.Type == config.ModelFileSourceHFCache {
		if err := writeHFRef(destination.Path, task.RepoID, task.Revision, resolvedRevision); err != nil {
			return err
		}
	}
	return nil
}

// saturatingDownloadBytes keeps aggregate progress counters monotonic even
// when untrusted Hugging Face metadata advertises enough large files to
// overflow int64. A saturated total is still an honest "unknown exact size"
// indicator; it must never wrap negative and make a task appear complete.
func saturatingDownloadBytes(total, value int64) int64 {
	if total < 0 {
		total = 0
	}
	if value <= 0 {
		return total
	}
	if total > math.MaxInt64-value {
		return math.MaxInt64
	}
	return total + value
}

type hfCacheFileTarget struct {
	Blob     string
	Snapshot string
}

func directoryTarget(root, repoID, filename string) (string, error) {
	repoRoot, err := safeJoin(root, filepath.FromSlash(repoID))
	if err != nil {
		return "", err
	}
	return safeJoin(repoRoot, filepath.FromSlash(filename))
}

func hfCacheTarget(root, repoID, resolvedRevision, requestedRevision, filename, expectedSHA string) (hfCacheFileTarget, error) {
	repoDir := "models--" + strings.ReplaceAll(repoID, "/", "--")
	repoRoot, err := safeJoin(root, repoDir)
	if err != nil {
		return hfCacheFileTarget{}, err
	}
	commit := cacheRevision(repoID, requestedRevision, resolvedRevision)
	snapshot, err := safeJoin(filepath.Join(repoRoot, "snapshots", commit), filepath.FromSlash(filename))
	if err != nil {
		return hfCacheFileTarget{}, err
	}
	blobName := expectedSHA
	if !isHexDigest(blobName) {
		sum := sha256.Sum256([]byte(repoID + "\x00" + resolvedRevision + "\x00" + filename))
		blobName = hex.EncodeToString(sum[:])
	}
	blob, err := safeJoin(filepath.Join(repoRoot, "blobs"), blobName)
	if err != nil {
		return hfCacheFileTarget{}, err
	}
	return hfCacheFileTarget{Blob: blob, Snapshot: snapshot}, nil
}

func modelScopeCacheTarget(root, repoID, resolvedRevision, requestedRevision, filename string) (string, error) {
	repoDir := strings.ReplaceAll(repoID, "/", "--")
	if repoDir == "" || !strings.Contains(repoDir, "--") {
		return "", errors.New("invalid ModelScope repository identifier")
	}
	revision := modelScopeCacheRevision(repoID, requestedRevision, resolvedRevision)
	repoRoot, err := safeJoin(root, filepath.Join("models", repoDir, "snapshots", revision))
	if err != nil {
		return "", err
	}
	return safeJoin(repoRoot, filepath.FromSlash(filename))
}

func modelScopeCacheRevision(repoID, requested, resolved string) string {
	for _, candidate := range []string{resolved, requested} {
		if isSafeRevision(candidate) && !strings.Contains(candidate, "/") {
			return candidate
		}
	}
	sum := sha256.Sum256([]byte(repoID + "\x00" + requested + "\x00" + resolved))
	return hex.EncodeToString(sum[:])[:40]
}

func cacheRevision(repoID, requested, resolved string) string {
	if isSafeRevision(resolved) {
		return resolved
	}
	if isSafeRevision(requested) {
		return requested
	}
	sum := sha256.Sum256([]byte(repoID + "\x00" + requested))
	return hex.EncodeToString(sum[:])[:40]
}

func isSafeRevision(value string) bool {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") {
		return false
	}
	clean := pathpkg.Clean(value)
	return clean == value && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../") && !strings.HasPrefix(clean, "/")
}

func writeHFRef(root, repoID, revision, resolved string) error {
	if err := rejectSymlinkPath(root); err != nil {
		return fmt.Errorf("HF cache root: %w", err)
	}
	repoRoot, err := safeJoin(root, "models--"+strings.ReplaceAll(repoID, "/", "--"))
	if err != nil {
		return err
	}
	relRevision, err := safeRepositoryPath(revision)
	if err != nil {
		return fmt.Errorf("invalid HF revision %q: %w", revision, err)
	}
	refPath, err := safeJoin(filepath.Join(repoRoot, "refs"), filepath.FromSlash(relRevision))
	if err != nil {
		return err
	}
	if err := rejectSymlinkParents(refPath); err != nil {
		return fmt.Errorf("HF ref parent: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(refPath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(refPath), ".ref-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := io.WriteString(tmp, resolved+"\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, refPath)
}

func ensureSnapshotEntry(blob, snapshot string) error {
	if err := rejectSymlinkPath(blob); err != nil {
		return fmt.Errorf("HF blob: %w", err)
	}
	if err := rejectSymlinkParents(snapshot); err != nil {
		return fmt.Errorf("HF snapshot parent: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(snapshot), 0o755); err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Dir(snapshot), blob)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(snapshot); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if existing, readErr := os.Readlink(snapshot); readErr == nil && filepath.Clean(existing) == filepath.Clean(rel) {
				return nil
			}
		} else if info.IsDir() {
			return fmt.Errorf("snapshot path is a directory: %s", snapshot)
		}
		if err := os.Remove(snapshot); err != nil {
			return err
		}
	}
	if err := os.Symlink(rel, snapshot); err == nil {
		return nil
	}
	// Windows may not grant symlink creation to the service account. A copied
	// snapshot entry keeps the downloaded model usable; the content-addressed
	// blob remains the source of truth for subsequent tasks.
	return copyFileAtomic(blob, snapshot)
}

func copyFileAtomic(source, target string) error {
	if err := rejectSymlinkPath(source); err != nil {
		return fmt.Errorf("copy source: %w", err)
	}
	if err := rejectSymlinkParents(target); err != nil {
		return fmt.Errorf("copy target parent: %w", err)
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(target), ".snapshot-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

const mib = int64(1024 * 1024)

var errRangeDownloadUnsupported = errors.New("server does not support byte range downloads")

// chunkManifest records completed byte ranges next to a sparse .part file.
// The sidecar makes an interrupted parallel download resumable without
// mistaking unwritten sparse blocks for valid model data.
type chunkManifest struct {
	Version   int    `json:"version"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256,omitempty"`
	ChunkSize int64  `json:"chunk_size"`
	Completed []int  `json:"completed"`
}

type chunkDownloadState struct {
	file         *os.File
	part         string
	manifestPath string
	manifest     chunkManifest
	completed    map[int]struct{}
	downloaded   int64
	progress     func(int64) error
	mu           sync.Mutex
}

func (m *Manager) shouldChunkDownload(part string, expectedSize int64) bool {
	if expectedSize <= 0 || m.config.ChunkWorkers < 2 || m.config.ChunkSizeMiB <= 0 || m.config.ChunkThresholdMiB <= 0 {
		return false
	}
	if expectedSize < int64(m.config.ChunkThresholdMiB)*mib {
		return false
	}
	if _, err := os.Lstat(part + ".chunks.json"); err == nil {
		return true
	}
	size, err := regularFileSize(part)
	return err == nil && size == 0
}

func chunkSizeBytes(config config.ModelDownloadsConfig) int64 {
	return int64(config.ChunkSizeMiB) * mib
}

func chunkCount(size, chunkSize int64) (int, error) {
	if size <= 0 || chunkSize <= 0 {
		return 0, errors.New("invalid chunk download size")
	}
	count := (size + chunkSize - 1) / chunkSize
	if count > int64(math.MaxInt) {
		return 0, errors.New("model file requires too many download chunks")
	}
	return int(count), nil
}

func chunkBounds(index int, size, chunkSize int64) (int64, int64) {
	start := int64(index) * chunkSize
	end := start + chunkSize - 1
	if end >= size {
		end = size - 1
	}
	return start, end
}

func loadChunkManifest(path string) (chunkManifest, bool, error) {
	if err := rejectSymlink(path); err != nil {
		return chunkManifest{}, false, err
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return chunkManifest{}, false, nil
	}
	if err != nil {
		return chunkManifest{}, false, err
	}
	var manifest chunkManifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		return chunkManifest{}, false, fmt.Errorf("decode chunk download state: %w", err)
	}
	return manifest, true, nil
}

func writeChunkManifest(path string, manifest chunkManifest) error {
	if err := rejectSymlink(path); err != nil {
		return err
	}
	contents, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".chunks-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(contents); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func removeChunkArtifacts(part string) error {
	for _, path := range []string{part + ".chunks.json", part} {
		if err := rejectSymlink(path); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (state *chunkDownloadState) recordChunk(index int, data []byte, expected int64) error {
	if int64(len(data)) != expected {
		return fmt.Errorf("downloaded chunk %d has %d bytes, want %d", index, len(data), expected)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if _, done := state.completed[index]; done {
		return nil
	}
	start, _ := chunkBounds(index, state.manifest.Size, state.manifest.ChunkSize)
	if written, err := state.file.WriteAt(data, start); err != nil {
		return err
	} else if written != len(data) {
		return io.ErrShortWrite
	}
	// Persist the bytes before persisting their completed marker. A crash can
	// repeat a range safely, but must never skip one based on an unflushed map.
	if err := state.file.Sync(); err != nil {
		return err
	}
	state.completed[index] = struct{}{}
	state.manifest.Completed = append(state.manifest.Completed, index)
	sort.Ints(state.manifest.Completed)
	state.downloaded = saturatingDownloadBytes(state.downloaded, expected)
	if err := writeChunkManifest(state.manifestPath, state.manifest); err != nil {
		return err
	}
	if state.progress != nil {
		return state.progress(state.downloaded)
	}
	return nil
}

func (m *Manager) downloadChunk(ctx context.Context, provider downloadProvider, fileURL string, start, end, total int64) ([]byte, error) {
	want := end - start + 1
	for attempt := 0; attempt <= m.config.MaxRetries; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		authorizeProviderRequest(request, provider)
		response, err := provider.client.Do(request)
		if err != nil {
			if !shouldRetry(attempt, m.config.MaxRetries) {
				return nil, transientDownloadError(fmt.Errorf("download range %s: %w", fileURL, err))
			}
			if err := m.waitRetry(ctx, attempt, 0); err != nil {
				return nil, err
			}
			continue
		}
		if response.StatusCode == http.StatusOK {
			response.Body.Close()
			return nil, errRangeDownloadUnsupported
		}
		if response.StatusCode != http.StatusPartialContent {
			retryAfter := retryAfter(response)
			message := responseError(response)
			if !shouldRetryStatus(response.StatusCode, attempt, m.config.MaxRetries) {
				err := fmt.Errorf("download range %s: %s", fileURL, message)
				if response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
					return nil, transientDownloadError(err)
				}
				return nil, err
			}
			if err := m.waitRetry(ctx, attempt, retryAfter); err != nil {
				return nil, err
			}
			continue
		}
		actualStart, actualEnd, actualTotal, ok := contentRange(response.Header.Get("Content-Range"))
		if !ok || actualStart != start || actualEnd != end || actualTotal != total {
			response.Body.Close()
			return nil, fmt.Errorf("server returned invalid content range for bytes %d-%d", start, end)
		}
		contents, readErr := io.ReadAll(io.LimitReader(response.Body, want+1))
		response.Body.Close()
		if readErr != nil || int64(len(contents)) != want {
			if !shouldRetry(attempt, m.config.MaxRetries) {
				if readErr != nil {
					return nil, transientDownloadError(readErr)
				}
				return nil, transientDownloadError(fmt.Errorf("download range %d-%d has %d bytes, want %d", start, end, len(contents), want))
			}
			if err := m.waitRetry(ctx, attempt, 0); err != nil {
				return nil, err
			}
			continue
		}
		return contents, nil
	}
	return nil, transientDownloadError(fmt.Errorf("download range %d-%d failed after %d retries", start, end, m.config.MaxRetries))
}

// downloadFileChunks downloads one large, known-size file with bounded Range
// requests. If the provider ignores Range, it reports handled=false and the
// caller falls back to the normal sequential/resumable path.
func (m *Manager) downloadFileChunks(ctx context.Context, provider downloadProvider, fileURL, final string, expectedSize int64, expectedSHA string, progress func(int64) error) (actual int64, handled bool, err error) {
	chunkSize := chunkSizeBytes(m.config)
	count, err := chunkCount(expectedSize, chunkSize)
	if err != nil {
		return 0, false, err
	}
	part := final + ".part"
	manifestPath := part + ".chunks.json"
	manifest, found, err := loadChunkManifest(manifestPath)
	if err != nil {
		return 0, true, err
	}
	if found && (manifest.Version != 1 || manifest.Size != expectedSize || manifest.ChunkSize != chunkSize || manifest.SHA256 != expectedSHA) {
		if err := removeChunkArtifacts(part); err != nil {
			return 0, true, err
		}
		found = false
	}
	partSize, err := regularFileSize(part)
	if err != nil {
		return 0, true, err
	}
	if found && partSize != expectedSize {
		if err := removeChunkArtifacts(part); err != nil {
			return 0, true, err
		}
		found = false
	}
	if !found && partSize != 0 {
		return 0, false, nil
	}
	if err := rejectSymlinkParents(final); err != nil {
		return 0, true, fmt.Errorf("download target parent: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return 0, true, err
	}
	flags := os.O_CREATE | os.O_RDWR
	if !found {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(part, flags, 0o600)
	if err != nil {
		return 0, true, err
	}
	if !found {
		if err := file.Truncate(expectedSize); err != nil {
			file.Close()
			return 0, true, err
		}
		manifest = chunkManifest{Version: 1, Size: expectedSize, SHA256: expectedSHA, ChunkSize: chunkSize, Completed: []int{}}
		if err := writeChunkManifest(manifestPath, manifest); err != nil {
			file.Close()
			return 0, true, err
		}
	}
	state := &chunkDownloadState{file: file, part: part, manifestPath: manifestPath, manifest: manifest, completed: make(map[int]struct{}, len(manifest.Completed)), progress: progress}
	pending := make([]int, 0, count)
	for _, index := range manifest.Completed {
		if index < 0 || index >= count {
			file.Close()
			return 0, true, fmt.Errorf("chunk download state has invalid chunk %d", index)
		}
		if _, duplicate := state.completed[index]; duplicate {
			continue
		}
		state.completed[index] = struct{}{}
		start, end := chunkBounds(index, expectedSize, chunkSize)
		state.downloaded = saturatingDownloadBytes(state.downloaded, end-start+1)
	}
	for index := 0; index < count; index++ {
		if _, done := state.completed[index]; !done {
			pending = append(pending, index)
		}
	}
	if progress != nil {
		if err := progress(state.downloaded); err != nil {
			file.Close()
			return 0, true, err
		}
	}
	if len(pending) > 0 {
		// Probe the first missing range before starting other workers so a server
		// that ignores Range can cleanly use the sequential downloader instead.
		first := pending[0]
		start, end := chunkBounds(first, expectedSize, chunkSize)
		contents, chunkErr := m.downloadChunk(ctx, provider, fileURL, start, end, expectedSize)
		if errors.Is(chunkErr, errRangeDownloadUnsupported) {
			file.Close()
			if removeErr := removeChunkArtifacts(part); removeErr != nil {
				return 0, true, removeErr
			}
			return 0, false, nil
		}
		if chunkErr != nil {
			file.Close()
			return 0, true, chunkErr
		}
		if err := state.recordChunk(first, contents, end-start+1); err != nil {
			file.Close()
			return 0, true, err
		}
		pending = pending[1:]
	}
	if len(pending) > 0 {
		downloadCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		jobs := make(chan int)
		errorsCh := make(chan error, 1)
		workers := m.config.ChunkWorkers
		if workers > len(pending) {
			workers = len(pending)
		}
		var group sync.WaitGroup
		for range workers {
			group.Add(1)
			go func() {
				defer group.Done()
				for index := range jobs {
					start, end := chunkBounds(index, expectedSize, chunkSize)
					contents, chunkErr := m.downloadChunk(downloadCtx, provider, fileURL, start, end, expectedSize)
					if chunkErr == nil {
						chunkErr = state.recordChunk(index, contents, end-start+1)
					}
					if chunkErr != nil {
						select {
						case errorsCh <- chunkErr:
							cancel()
						default:
						}
						return
					}
				}
			}()
		}
	feedChunks:
		for _, index := range pending {
			select {
			case jobs <- index:
			case <-downloadCtx.Done():
				break feedChunks
			}
		}
		close(jobs)
		group.Wait()
		select {
		case chunkErr := <-errorsCh:
			file.Close()
			return 0, true, chunkErr
		default:
		}
		if err := ctx.Err(); err != nil {
			file.Close()
			return 0, true, err
		}
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return 0, true, err
	}
	if err := file.Close(); err != nil {
		return 0, true, err
	}
	if err := verifyFile(part, expectedSize, expectedSHA); err != nil {
		if removeErr := removeChunkArtifacts(part); removeErr != nil {
			return 0, true, removeErr
		}
		return 0, true, err
	}
	if err := finalizePart(part, final); err != nil {
		return 0, true, err
	}
	if err := os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return expectedSize, true, err
	}
	return expectedSize, true, nil
}

func (m *Manager) downloadFile(ctx context.Context, provider downloadProvider, fileURL, final string, expectedSize int64, expectedSHA string, progress func(int64) error) (int64, error) {
	ctx = normalizeContext(ctx)
	if err := rejectSymlinkParents(final); err != nil {
		return 0, fmt.Errorf("download target parent: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return 0, err
	}
	if complete, size, err := completeFile(final, expectedSize, expectedSHA); err != nil {
		return 0, err
	} else if complete {
		if progress != nil {
			if err := progress(size); err != nil {
				return 0, err
			}
		}
		return size, nil
	}
	part := final + ".part"
	if err := rejectSymlink(part); err != nil {
		return 0, err
	}
	if m.shouldChunkDownload(part, expectedSize) {
		actual, handled, err := m.downloadFileChunks(ctx, provider, fileURL, final, expectedSize, expectedSHA, progress)
		if handled {
			return actual, err
		}
	}
	for attempt := 0; attempt <= m.config.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		partSize, err := regularFileSize(part)
		if err != nil {
			return 0, err
		}
		if expectedSize > 0 && partSize > expectedSize {
			if err := os.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
				return 0, err
			}
			partSize = 0
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
		if err != nil {
			return 0, err
		}
		authorizeProviderRequest(request, provider)
		if partSize > 0 {
			request.Header.Set("Range", fmt.Sprintf("bytes=%d-", partSize))
		}
		response, err := provider.client.Do(request)
		if err != nil {
			if !shouldRetry(attempt, m.config.MaxRetries) {
				return 0, transientDownloadError(fmt.Errorf("download %s: %w", fileURL, err))
			}
			if err := m.waitRetry(ctx, attempt, 0); err != nil {
				return 0, err
			}
			continue
		}
		if response.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			response.Body.Close()
			if expectedSize > 0 && partSize == expectedSize {
				if err := verifyFile(part, expectedSize, expectedSHA); err == nil {
					if err := finalizePart(part, final); err != nil {
						return 0, err
					}
					if progress != nil {
						if err := progress(expectedSize); err != nil {
							return 0, err
						}
					}
					return expectedSize, nil
				}
			}
			if err := os.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
				return 0, err
			}
			if !shouldRetry(attempt, m.config.MaxRetries) {
				return 0, errors.New("server rejected the resume range")
			}
			if err := m.waitRetry(ctx, attempt, 0); err != nil {
				return 0, err
			}
			continue
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			retryAfter := retryAfter(response)
			message := responseError(response)
			if !shouldRetryStatus(response.StatusCode, attempt, m.config.MaxRetries) {
				err := fmt.Errorf("download %s: %s", fileURL, message)
				if response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
					return 0, transientDownloadError(err)
				}
				return 0, err
			}
			if err := m.waitRetry(ctx, attempt, retryAfter); err != nil {
				return 0, err
			}
			continue
		}

		resume := partSize > 0 && response.StatusCode == http.StatusPartialContent
		if response.StatusCode == http.StatusPartialContent {
			start, ok := contentRangeStart(response.Header.Get("Content-Range"))
			if !ok || start != partSize {
				response.Body.Close()
				if !shouldRetry(attempt, m.config.MaxRetries) {
					return 0, errors.New("server returned an invalid content range")
				}
				if err := m.waitRetry(ctx, attempt, 0); err != nil {
					return 0, err
				}
				continue
			}
		}
		if partSize > 0 && response.StatusCode == http.StatusOK {
			partSize = 0
			resume = false
		}
		flags := os.O_CREATE | os.O_WRONLY
		if !resume {
			flags |= os.O_TRUNC
		} else {
			flags |= os.O_APPEND
		}
		file, openErr := os.OpenFile(part, flags, 0o600)
		if openErr != nil {
			response.Body.Close()
			return 0, openErr
		}
		if progress != nil && partSize == 0 {
			if err := progress(0); err != nil {
				file.Close()
				response.Body.Close()
				return 0, err
			}
		}
		reader := &progressReader{reader: response.Body, base: partSize, value: partSize, callback: progress}
		_, copyErr := io.CopyBuffer(file, reader, make([]byte, 64*1024))
		response.Body.Close()
		syncErr := file.Sync()
		closeErr := file.Close()
		if copyErr == nil {
			copyErr = reader.err
		}
		if copyErr == nil {
			copyErr = syncErr
		}
		if copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			if errors.Is(copyErr, context.Canceled) {
				return 0, context.Canceled
			}
			if !shouldRetry(attempt, m.config.MaxRetries) {
				return 0, transientDownloadError(fmt.Errorf("download %s: %w", fileURL, copyErr))
			}
			if err := m.waitRetry(ctx, attempt, 0); err != nil {
				return 0, err
			}
			continue
		}
		actual, statErr := regularFileSize(part)
		if statErr != nil {
			return 0, statErr
		}
		if expectedSize > 0 && actual != expectedSize {
			if actual > expectedSize {
				if err := os.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
					return 0, err
				}
			}
			if !shouldRetry(attempt, m.config.MaxRetries) {
				return 0, fmt.Errorf("download %s: got %d bytes, want %d", fileURL, actual, expectedSize)
			}
			if err := m.waitRetry(ctx, attempt, 0); err != nil {
				return 0, err
			}
			continue
		}
		if err := verifyFile(part, expectedSize, expectedSHA); err != nil {
			if removeErr := os.Remove(part); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return 0, removeErr
			}
			if !shouldRetry(attempt, m.config.MaxRetries) {
				return 0, err
			}
			if err := m.waitRetry(ctx, attempt, 0); err != nil {
				return 0, err
			}
			continue
		}
		if err := finalizePart(part, final); err != nil {
			return 0, err
		}
		if progress != nil {
			if err := progress(actual); err != nil {
				return 0, err
			}
		}
		return actual, nil
	}
	return 0, transientDownloadError(fmt.Errorf("download %s failed after %d retries", fileURL, m.config.MaxRetries))
}

type progressReader struct {
	reader   io.Reader
	base     int64
	value    int64
	last     int64
	lastTime time.Time
	callback func(int64) error
	err      error
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.value += int64(n)
	if r.callback != nil && r.err == nil && (r.value-r.last >= progressBytes || time.Since(r.lastTime) >= progressInterval || (err == io.EOF && r.value != r.last)) {
		r.err = r.callback(r.value)
		if r.err != nil {
			return n, r.err
		}
		r.last = r.value
		r.lastTime = time.Now()
	}
	return n, err
}

func (m *Manager) waitRetry(ctx context.Context, attempt int, retryAfter time.Duration) error {
	delay := m.config.RetryBackoff
	for i := 0; i < attempt; i++ {
		if delay >= time.Minute/2 {
			delay = time.Minute
			break
		}
		delay *= 2
	}
	if delay > time.Minute {
		delay = time.Minute
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func completeFile(path string, expectedSize int64, expectedSHA string) (bool, int64, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, 0, fmt.Errorf("refusing to overwrite symlink: %s", path)
	}
	if !info.Mode().IsRegular() {
		return false, 0, fmt.Errorf("download target is not a regular file: %s", path)
	}
	if expectedSize > 0 && info.Size() != expectedSize {
		return false, 0, nil
	}
	if err := verifyFile(path, expectedSize, expectedSHA); err != nil {
		return false, 0, nil
	}
	return true, info.Size(), nil
}

func regularFileSize(path string) (int64, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("refusing to use symlink as partial download: %s", path)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("partial download is not a regular file: %s", path)
	}
	return info.Size(), nil
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to overwrite symlink: %s", path)
	}
	return nil
}

func finalizePart(part, final string) error {
	if err := rejectSymlinkParents(final); err != nil {
		return err
	}
	if err := rejectSymlink(final); err != nil {
		return err
	}
	if _, err := os.Lstat(final); err == nil {
		if err := os.Remove(final); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(part, final)
}

func verifyFile(path string, expectedSize int64, expectedSHA string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("download is not a regular file: %s", path)
	}
	if expectedSize > 0 && info.Size() != expectedSize {
		return fmt.Errorf("download size %d does not match expected %d", info.Size(), expectedSize)
	}
	if expectedSHA == "" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expectedSHA) {
		return fmt.Errorf("download checksum mismatch for %s", path)
	}
	return nil
}

func (m *Manager) fetchModelInfo(ctx context.Context, provider downloadProvider, repoID, revision string) (hfModelInfo, error) {
	ctx = normalizeContext(ctx)
	switch provider.name {
	case ProviderHuggingFace:
		endpoint := provider.baseURL + "/api/models/" + escapedRepoID(repoID) + "?revision=" + url.QueryEscape(revision)
		body, err := m.getWithRetry(ctx, provider, endpoint)
		if err != nil {
			return hfModelInfo{}, err
		}
		var info hfModelInfo
		if err := json.Unmarshal(body, &info); err != nil {
			return hfModelInfo{}, fmt.Errorf("decode Hugging Face model metadata: %w", err)
		}
		return info, nil
	case ProviderModelScope:
		query := url.Values{"Revision": {revision}, "Recursive": {"true"}}
		endpoint := provider.baseURL + "/api/v1/models/" + escapedRepoID(repoID) + "/repo/files?" + query.Encode()
		body, err := m.getWithRetry(ctx, provider, endpoint)
		if err != nil {
			return hfModelInfo{}, err
		}
		return decodeModelScopeModelInfo(body)
	default:
		return hfModelInfo{}, fmt.Errorf("unsupported model download provider %q", provider.name)
	}
}

type modelScopeFilesResponse struct {
	Code    int    `json:"Code"`
	Success bool   `json:"Success"`
	Message string `json:"Message"`
	Data    struct {
		Files []modelScopeFileEntry `json:"Files"`
	} `json:"Data"`
}

type modelScopeFileEntry struct {
	Path     string `json:"Path"`
	Revision string `json:"Revision"`
	SHA256   string `json:"Sha256"`
	Size     int64  `json:"Size"`
	Type     string `json:"Type"`
}

func decodeModelScopeModelInfo(body []byte) (hfModelInfo, error) {
	var response modelScopeFilesResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return hfModelInfo{}, fmt.Errorf("decode ModelScope model metadata: %w", err)
	}
	if response.Code != 0 && response.Code != http.StatusOK {
		message := strings.TrimSpace(response.Message)
		if message == "" {
			message = "request failed"
		}
		return hfModelInfo{}, fmt.Errorf("ModelScope model metadata: %s", message)
	}
	info := hfModelInfo{Siblings: make([]hfFileEntry, 0, len(response.Data.Files))}
	for _, file := range response.Data.Files {
		if strings.EqualFold(strings.TrimSpace(file.Type), "tree") {
			continue
		}
		if info.SHA == "" {
			info.SHA = strings.TrimSpace(file.Revision)
		}
		info.Siblings = append(info.Siblings, hfFileEntry{
			RFilename: file.Path,
			Size:      file.Size,
			LFS:       &hfLFSMeta{Size: file.Size, SHA256: file.SHA256},
		})
	}
	return info, nil
}

type hfModelInfo struct {
	SHA      string        `json:"sha"`
	Siblings []hfFileEntry `json:"siblings"`
}

type hfFileEntry struct {
	RFilename string     `json:"rfilename"`
	Size      int64      `json:"size"`
	LFS       *hfLFSMeta `json:"lfs"`
}

type hfLFSMeta struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func siblingExpected(entry hfFileEntry) (int64, string, error) {
	if strings.TrimSpace(entry.RFilename) == "" {
		return 0, "", errors.New("model repository metadata contains a file without a path")
	}
	size := entry.Size
	checksum := ""
	if entry.LFS != nil {
		if entry.LFS.Size > 0 {
			size = entry.LFS.Size
		}
		checksum = strings.ToLower(strings.TrimSpace(entry.LFS.SHA256))
	}
	if size < 0 {
		return 0, "", fmt.Errorf("model repository metadata contains a negative size for %s", entry.RFilename)
	}
	if checksum != "" && !isHexDigest(checksum) {
		checksum = ""
	}
	return size, checksum, nil
}

func selectSiblings(entries []hfFileEntry, include, exclude []string) ([]hfFileEntry, error) {
	selected := make([]hfFileEntry, 0, len(entries))
	for _, entry := range entries {
		name, err := safeRepositoryPath(entry.RFilename)
		if err != nil {
			return nil, fmt.Errorf("invalid repository file %q: %w", entry.RFilename, err)
		}
		if len(include) > 0 && !matchesPattern(name, include) {
			continue
		}
		if matchesPattern(name, exclude) {
			continue
		}
		entry.RFilename = name
		selected = append(selected, entry)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].RFilename < selected[j].RFilename })
	return selected, nil
}

func matchesPattern(name string, patterns []string) bool {
	for _, pattern := range patterns {
		if ok, _ := pathpkg.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

func safeRepositoryPath(value string) (string, error) {
	if hasUnsafeDownloadRune(value, true) {
		return "", errors.New("repository path contains whitespace or control characters")
	}
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return "", errors.New("repository path must be a relative slash-separated path")
	}
	// Do not silently canonicalize traversal or empty segments. Although
	// path.Clean would keep `a/../b` inside the root, accepting it makes the
	// remote metadata's requested name differ from the path that is actually
	// written and leaves room for platform-specific normalization surprises.
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("repository path contains an invalid path segment")
		}
	}
	clean := pathpkg.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("repository path escapes the destination")
	}
	return clean, nil
}

func safeJoin(root, relative string) (string, error) {
	root = filepath.Clean(root)
	if root == "" || !filepath.IsAbs(root) {
		return "", errors.New("download destination must be an absolute path")
	}
	if err := rejectSymlinkPath(root); err != nil {
		return "", fmt.Errorf("download destination: %w", err)
	}
	candidate := filepath.Clean(filepath.Join(root, relative))
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("download path escapes the configured source")
	}
	if err := rejectSymlinkParents(candidate); err != nil {
		return "", fmt.Errorf("download path parent: %w", err)
	}
	return candidate, nil
}

func (m *Manager) getWithRetry(ctx context.Context, provider downloadProvider, endpoint string) ([]byte, error) {
	ctx = normalizeContext(ctx)
	for attempt := 0; attempt <= m.config.MaxRetries; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		authorizeProviderRequest(request, provider)
		response, err := provider.client.Do(request)
		if err != nil {
			if !shouldRetry(attempt, m.config.MaxRetries) {
				return nil, transientDownloadError(fmt.Errorf("request %s: %w", endpoint, err))
			}
			if err := m.waitRetry(ctx, attempt, 0); err != nil {
				return nil, err
			}
			continue
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			retryAfter := retryAfter(response)
			message := responseError(response)
			if !shouldRetryStatus(response.StatusCode, attempt, m.config.MaxRetries) {
				err := fmt.Errorf("request %s: %s", endpoint, message)
				if response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
					return nil, transientDownloadError(err)
				}
				return nil, err
			}
			if err := m.waitRetry(ctx, attempt, retryAfter); err != nil {
				return nil, err
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes+1))
		response.Body.Close()
		if len(body) > maxMetadataBytes {
			return nil, errors.New("model repository metadata response is too large")
		}
		if readErr != nil {
			if !shouldRetry(attempt, m.config.MaxRetries) {
				return nil, transientDownloadError(readErr)
			}
			if err := m.waitRetry(ctx, attempt, 0); err != nil {
				return nil, err
			}
			continue
		}
		return body, nil
	}
	return nil, transientDownloadError(fmt.Errorf("request %s failed after %d retries", endpoint, m.config.MaxRetries))
}

func resolveFileURL(provider downloadProvider, repoID, revision, filename string) string {
	if provider.name == ProviderModelScope {
		query := url.Values{"Revision": {revision}, "FilePath": {filename}}
		return provider.baseURL + "/api/v1/models/" + escapedRepoID(repoID) + "/repo?" + query.Encode()
	}
	parts := []string{provider.baseURL, escapedRepoID(repoID), "resolve", url.PathEscape(revision)}
	for _, part := range strings.Split(filename, "/") {
		parts = append(parts, url.PathEscape(part))
	}
	return strings.Join(parts, "/") + "?download=true"
}

func escapedRepoID(repoID string) string {
	parts := strings.Split(repoID, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func authorizeProviderRequest(request *http.Request, provider downloadProvider) {
	token := strings.TrimSpace(provider.token)
	if token == "" {
		tokenEnv := strings.TrimSpace(provider.tokenEnv)
		token = strings.TrimSpace(os.Getenv(tokenEnv))
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("User-Agent", "llama-swap-model-downloader/1")
}

func responseError(response *http.Response) string {
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
	message := strings.TrimSpace(string(body))
	if message == "" {
		return response.Status
	}
	return response.Status + ": " + message
}

func retryAfter(response *http.Response) time.Duration {
	value := strings.TrimSpace(response.Header.Get("Retry-After"))
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func shouldRetry(attempt, maxRetries int) bool { return attempt < maxRetries }

func shouldRetryStatus(status, attempt, maxRetries int) bool {
	if !shouldRetry(attempt, maxRetries) {
		return false
	}
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

func contentRangeStart(value string) (int64, bool) {
	start, _, _, ok := contentRange(value)
	return start, ok
}

func contentRange(value string) (start, end, total int64, ok bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, false
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "bytes "), "/", 2)
	if len(parts) != 2 {
		return 0, 0, 0, false
	}
	bounds := strings.SplitN(parts[0], "-", 2)
	if len(bounds) != 2 {
		return 0, 0, 0, false
	}
	start, startErr := strconv.ParseInt(bounds[0], 10, 64)
	end, endErr := strconv.ParseInt(bounds[1], 10, 64)
	total, totalErr := strconv.ParseInt(parts[1], 10, 64)
	if startErr != nil || endErr != nil || totalErr != nil || start < 0 || end < start || total <= end {
		return 0, 0, 0, false
	}
	return start, end, total, true
}

func isHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func normalizeBaseURL(provider, value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s base URL must be an HTTP(S) URL without credentials, query, or fragment", provider)
	}
	return value, nil
}

func normalizeRequest(request Request) (Request, error) {
	request.Provider = strings.ToLower(strings.TrimSpace(request.Provider))
	if request.Provider == "" {
		request.Provider = ProviderHuggingFace
	}
	if request.Provider != ProviderHuggingFace && request.Provider != ProviderModelScope {
		return Request{}, errors.New("provider must be huggingface or modelscope")
	}
	if hasUnsafeDownloadRune(request.RepoID, true) {
		return Request{}, errors.New("repo_id contains whitespace or control characters")
	}
	request.RepoID = strings.TrimSpace(request.RepoID)
	if request.RepoID == "" || strings.Count(request.RepoID, "/") != 1 || strings.Contains(request.RepoID, "//") || strings.ContainsAny(request.RepoID, "\\\x00") || strings.ContainsRune(request.RepoID, ' ') {
		return Request{}, errors.New("repo_id must be a namespace/model identifier")
	}
	for _, part := range strings.Split(request.RepoID, "/") {
		if part == "." || part == ".." || strings.TrimSpace(part) == "" {
			return Request{}, errors.New("repo_id contains an invalid path segment")
		}
	}
	if hasUnsafeDownloadRune(request.Revision, true) {
		return Request{}, errors.New("revision contains an invalid character")
	}
	request.Revision = strings.TrimSpace(request.Revision)
	if request.Revision == "" {
		if request.Provider == ProviderModelScope {
			request.Revision = "master"
		} else {
			request.Revision = "main"
		}
	}
	if strings.ContainsRune(request.Revision, ' ') || strings.Contains(request.Revision, "\\") {
		return Request{}, errors.New("revision contains an invalid character")
	}
	request.SourceID = strings.TrimSpace(request.SourceID)
	var err error
	request.Include, err = normalizePatterns(request.Include)
	if err != nil {
		return Request{}, fmt.Errorf("include: %w", err)
	}
	request.Exclude, err = normalizePatterns(request.Exclude)
	if err != nil {
		return Request{}, fmt.Errorf("exclude: %w", err)
	}
	return request, nil
}

func normalizePatterns(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if hasUnsafeDownloadRune(value, true) {
			return nil, fmt.Errorf("pattern %q contains whitespace or control characters", value)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(value) > 512 {
			return nil, errors.New("pattern is too long")
		}
		if _, err := pathpkg.Match(value, ""); err != nil {
			return nil, fmt.Errorf("invalid glob %q: %w", value, err)
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

// hasUnsafeDownloadRune rejects characters that are visually empty or can
// alter log/path interpretation. Ordinary ASCII spaces remain allowed for
// glob patterns and repository filenames; callers that validate identifiers
// separately reject those spaces after trimming legacy padding.
func hasUnsafeDownloadRune(value string, allowASCIIWhitespace bool) bool {
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
		if unicode.IsSpace(r) && (!allowASCIIWhitespace || r != ' ') {
			return true
		}
	}
	return false
}

func newID(prefix string) string {
	random := make([]byte, 8)
	if _, err := cryptorand.Read(random); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", prefix, time.Now().UnixNano())))
		return prefix + "-" + hex.EncodeToString(sum[:8])
	}
	return prefix + "-" + hex.EncodeToString(random)
}
