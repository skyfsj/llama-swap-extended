package mcpruntime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type pendingCancellation struct {
	ctx  context.Context
	sent chan struct{}
	once sync.Once
}
type cancellationTransport struct {
	mcp.Transport
	connection *cancellationConnection
}

func (t *cancellationTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	connection, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	t.connection = &cancellationConnection{Connection: connection, pending: map[string]*pendingCancellation{}}
	return t.connection, nil
}

type cancellationConnection struct {
	mcp.Connection
	mu      sync.Mutex
	pending map[string]*pendingCancellation
}

func cancellationID(id any) string { encoded, _ := json.Marshal(id); return string(encoded) }
func (c *cancellationConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	request, ok := message.(*jsonrpc.Request)
	var pending *pendingCancellation
	if ok {
		c.mu.Lock()
		if request.ID.IsValid() {
			c.pending[cancellationID(request.ID.Raw())] = &pendingCancellation{ctx: ctx, sent: make(chan struct{})}
		} else if request.Method == "notifications/cancelled" {
			var params struct {
				RequestID any `json:"requestId"`
			}
			if json.Unmarshal(request.Params, &params) == nil {
				pending = c.pending[cancellationID(params.RequestID)]
			}
		}
		c.mu.Unlock()
	}
	err := c.Connection.Write(ctx, message)
	if pending != nil {
		pending.once.Do(func() { close(pending.sent) })
		c.mu.Lock()
		var params struct {
			RequestID any `json:"requestId"`
		}
		_ = json.Unmarshal(request.Params, &params)
		delete(c.pending, cancellationID(params.RequestID))
		c.mu.Unlock()
	}
	return err
}

func (c *cancellationConnection) completeOperation(ctx context.Context) {
	token := ctx.Value(operationTokenKey{})
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, pending := range c.pending {
		if pending.ctx.Value(operationTokenKey{}) == token {
			pending.once.Do(func() { close(pending.sent) })
			delete(c.pending, id)
		}
	}
}
func (c *cancellationConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	message, err := c.Connection.Read(ctx)
	if response, ok := message.(*jsonrpc.Response); ok {
		c.mu.Lock()
		if pending := c.pending[cancellationID(response.ID.Raw())]; pending != nil {
			pending.once.Do(func() { close(pending.sent) })
		}
		delete(c.pending, cancellationID(response.ID.Raw()))
		c.mu.Unlock()
	}
	return message, err
}
func (c *cancellationConnection) wait() {
	c.mu.Lock()
	waiting := []chan struct{}{}
	for _, p := range c.pending {
		if p.ctx.Err() != nil {
			waiting = append(waiting, p.sent)
		}
	}
	c.mu.Unlock()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for _, sent := range waiting {
		select {
		case <-sent:
		case <-timer.C:
			return
		}
	}
}

type Session struct {
	*mcp.ClientSession
	cancellation     *cancellationTransport
	httpCancellation *cancellationConnection
	lifetime         context.Context
	manager          *Manager
	release          func()
	closeTransport   context.CancelFunc
	closeOnce        sync.Once
	closeErr         error
}

func (s *Session) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.close() })
	return s.closeErr
}
func (s *Session) close() error {
	if s.httpCancellation != nil {
		s.httpCancellation.wait()
	}
	if s.cancellation != nil && s.cancellation.connection != nil {
		s.cancellation.connection.wait()
	}
	if s.release != nil {
		s.release()
		return nil
	}
	if s.closeTransport != nil {
		defer s.closeTransport()
	}
	return s.ClientSession.Close()
}

type cancellationHTTPBody struct {
	io.ReadCloser
	finish func()
}

func (b *cancellationHTTPBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.finish()
	}
	return n, err
}
func (b *cancellationHTTPBody) Close() error { err := b.ReadCloser.Close(); b.finish(); return err }
func (c *cancellationConnection) httpRequest(r *http.Request) func(*http.Response, error) {
	if r.GetBody == nil {
		return func(*http.Response, error) {}
	}
	body, err := r.GetBody()
	if err != nil {
		return func(*http.Response, error) {}
	}
	data, err := io.ReadAll(io.LimitReader(body, (16<<20)+1))
	body.Close()
	if err != nil {
		return func(*http.Response, error) {}
	}
	message, err := jsonrpc.DecodeMessage(data)
	if err != nil {
		return func(*http.Response, error) {}
	}
	request, ok := message.(*jsonrpc.Request)
	if !ok {
		return func(*http.Response, error) {}
	}
	if request.Method == "notifications/cancelled" {
		var params struct {
			RequestID any `json:"requestId"`
		}
		_ = json.Unmarshal(request.Params, &params)
		return func(*http.Response, error) {
			c.mu.Lock()
			pending := c.pending[cancellationID(params.RequestID)]
			c.mu.Unlock()
			if pending != nil {
				pending.once.Do(func() { close(pending.sent) })
				c.mu.Lock()
				delete(c.pending, cancellationID(params.RequestID))
				c.mu.Unlock()
			}
		}
	}
	if !request.ID.IsValid() {
		return func(*http.Response, error) {}
	}
	id := cancellationID(request.ID.Raw())
	pending := &pendingCancellation{ctx: r.Context(), sent: make(chan struct{})}
	c.mu.Lock()
	c.pending[id] = pending
	c.mu.Unlock()
	finish := func() {
		if r.Context().Err() != nil {
			return
		}
		pending.once.Do(func() { close(pending.sent) })
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}
	return func(response *http.Response, err error) {
		if err != nil {
			finish()
			return
		}
		response.Body = &cancellationHTTPBody{ReadCloser: response.Body, finish: finish}
	}
}
