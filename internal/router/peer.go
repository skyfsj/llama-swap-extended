package router

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type peerMember struct {
	peerID       string
	reverseProxy *httputil.ReverseProxy
	apiKey       string
}

type peerRoute struct {
	member  *peerMember
	modelID string
}

type Peer struct {
	mu     sync.RWMutex
	cfg    config.Config
	logger *logmon.Monitor
	peers  map[string]*peerRoute

	shutdownCtx  context.Context
	shutdownFn   context.CancelFunc
	shuttingDown atomic.Bool
	inflight     sync.WaitGroup
}

// Reconfigure atomically publishes a new peer route table. Requests that have
// already selected an old reverse proxy keep using it until completion; new
// requests see the new peer configuration. The Peer object, its shutdown
// context and the enclosing Server remain unchanged.
func (r *Peer) Reconfigure(cfg config.Config) error {
	next, err := NewPeer(cfg, r.logger)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.cfg = cfg
	r.peers = next.peers
	r.mu.Unlock()
	// The temporary Peer only owns the construction context. Its route proxies
	// are now intentionally owned by r; canceling that context does not cancel
	// requests because ServeHTTP derives from r.shutdownCtx.
	next.shutdownFn()
	return nil
}

func NewPeer(cfg config.Config, logger *logmon.Monitor) (*Peer, error) {
	if err := config.ValidatePeerNamespace(cfg); err != nil {
		return nil, err
	}

	peers := cfg.Peers
	modelMap := make(map[string]*peerRoute)
	bareRoutes := make(map[string][]*peerRoute)

	peerIDs := make([]string, 0, len(peers))
	for peerID := range peers {
		peerIDs = append(peerIDs, peerID)
	}
	sort.Strings(peerIDs)

	for _, peerID := range peerIDs {
		peer := peers[peerID]

		peerTransport := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   time.Duration(peer.Timeouts.Connect) * time.Second,
				KeepAlive: time.Duration(peer.Timeouts.KeepAlive) * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   time.Duration(peer.Timeouts.TLSHandshake) * time.Second,
			ResponseHeaderTimeout: time.Duration(peer.Timeouts.ResponseHeader) * time.Second,
			ExpectContinueTimeout: time.Duration(peer.Timeouts.ExpectContinue) * time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       time.Duration(peer.Timeouts.IdleConn) * time.Second,
		}

		reverseProxy := &httputil.ReverseProxy{
			Transport: peerTransport,
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(peer.ProxyURL)
				r.Out.Host = r.Out.URL.Host
			},
		}

		reverseProxy.ModifyResponse = func(resp *http.Response) error {
			if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
				resp.Header.Set("X-Accel-Buffering", "no")
			}
			return nil
		}

		reverseProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			// A cancelled request is not a peer failure, so keep it out of the
			// warning stream whether or not the sentinel applies below.
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				logger.Debugf("peer %s: request cancelled: %v", peerID, err)
			} else {
				logger.Warnf("peer %s: proxy error: %v", peerID, err)
			}

			// Only a client that actually hung up gets the recorded-only
			// sentinel (#1029). A request cancelled server-side still has a
			// client waiting for an answer.
			if swaputil.MarkClientClosed(w, r) || swaputil.ResponseStarted(w) {
				return
			}

			errMsg := fmt.Sprintf("peer proxy error: %v", err)
			if runtime.GOOS == "darwin" && strings.Contains(err.Error(), "connect: no route to host") {
				errMsg += " (hint: on macOS, check System Settings > Privacy & Security > Local Network permissions)"
			}
			swaputil.SendResponse(w, r, http.StatusBadGateway, errMsg)
		}

		pp := &peerMember{
			peerID:       peerID,
			reverseProxy: reverseProxy,
			apiKey:       peer.ApiKey,
		}

		seen := make(map[string]struct{})
		for _, modelID := range peer.Models {
			if _, duplicate := seen[modelID]; duplicate {
				continue
			}
			seen[modelID] = struct{}{}

			route := &peerRoute{member: pp, modelID: modelID}
			modelMap[config.PeerModelFQN(peerID, modelID)] = route
			bareRoutes[modelID] = append(bareRoutes[modelID], route)
		}
	}

	for modelID, routes := range bareRoutes {
		if len(routes) != 1 {
			continue
		}
		if _, reserved := modelMap[modelID]; reserved {
			continue
		}
		modelMap[modelID] = routes[0]
	}

	shutdownCtx, shutdownFn := context.WithCancel(context.Background())

	return &Peer{
		cfg:         cfg,
		logger:      logger,
		peers:       modelMap,
		shutdownCtx: shutdownCtx,
		shutdownFn:  shutdownFn,
	}, nil
}

func (r *Peer) Handles(model string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.peers[model]
	return ok
}

func (r *Peer) Shutdown(timeout time.Duration) error {
	if !r.shuttingDown.CompareAndSwap(false, true) {
		return fmt.Errorf("shutdown already in progress")
	}

	if timeout == 0 {
		r.shutdownFn()
		r.inflight.Wait()
		return nil
	}

	done := make(chan struct{})
	go func() {
		r.inflight.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		r.shutdownFn()
		r.inflight.Wait()
		return fmt.Errorf("peer shutdown timed out after %v", timeout)
	}
}

func (r *Peer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.shuttingDown.Load() {
		swaputil.SendError(w, req, fmt.Errorf("peer proxy is shutting down"))
		return
	}
	r.inflight.Add(1)
	defer r.inflight.Done()

	r.mu.RLock()
	conf := r.cfg
	r.mu.RUnlock()
	data, err := swaputil.FetchContext(req, conf)
	if err != nil {
		swaputil.SendError(w, req, err)
		return
	}

	r.mu.RLock()
	route, found := r.peers[data.ModelID]
	r.mu.RUnlock()
	if !found {
		r.logger.Warnf("peer model not found: %s", data.ModelID)
		swaputil.SendError(w, req, ErrNoPeerModelFound)
		return
	}
	pp := route.member

	r.logger.Debugf("peer: routing model %s to peer %s as %s", data.ModelID, pp.peerID, route.modelID)

	if data.Model != route.modelID {
		req, err = swaputil.ReplaceRequestModel(req, data.Model, route.modelID)
		if err != nil {
			swaputil.SendResponse(w, req, http.StatusBadRequest, err.Error())
			return
		}
	}

	if pp.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+pp.apiKey)
		req.Header.Set("x-api-key", pp.apiKey)
	}

	// Cancel the proxy request when the client disconnects or shutdown times out.
	// Deriving from the request covers the client half directly and keeps the
	// request's context values — notably the client context that tells a real
	// disconnect apart from a server-side cancel. AfterFunc links the unrelated
	// shutdown context in without a goroutine leak.
	ctx, cancel := context.WithCancel(req.Context())
	stopShutdown := context.AfterFunc(r.shutdownCtx, cancel)
	req = req.WithContext(ctx)

	pp.reverseProxy.ServeHTTP(w, req)

	stopShutdown()
	cancel()
}
