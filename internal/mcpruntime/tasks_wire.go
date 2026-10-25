package mcpruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const TasksExtension = "io.modelcontextprotocol/tasks"
const taskCallMethod = "io.llama-swap/tasks/call"

type Task struct {
	TaskID        string              `json:"taskId"`
	Status        string              `json:"status"`
	StatusMessage string              `json:"statusMessage,omitempty"`
	CreatedAt     time.Time           `json:"createdAt"`
	LastUpdatedAt time.Time           `json:"lastUpdatedAt"`
	TTL           *int64              `json:"ttlMs"`
	PollInterval  *int64              `json:"pollIntervalMs,omitempty"`
	InputRequests mcp.InputRequestMap `json:"inputRequests,omitempty"`
	Result        json.RawMessage     `json:"result,omitempty"`
	Error         *jsonrpc.Error      `json:"error,omitempty"`
}

type TaskParams struct {
	mcp.CallToolParams `json:"-"`
	TaskID             string               `json:"taskId"`
	Responses          mcp.InputResponseMap `json:"inputResponses,omitempty"`
}

func (p TaskParams) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Meta      mcp.Meta             `json:"_meta,omitempty"`
		TaskID    string               `json:"taskId"`
		Responses mcp.InputResponseMap `json:"inputResponses,omitempty"`
	}{p.Meta, p.TaskID, p.Responses})
}
func (p *TaskParams) UnmarshalJSON(data []byte) error {
	var wire struct {
		Meta      mcp.Meta             `json:"_meta"`
		TaskID    string               `json:"taskId"`
		Responses mcp.InputResponseMap `json:"inputResponses"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	p.Meta = wire.Meta
	p.TaskID = wire.TaskID
	p.Responses = wire.Responses
	return nil
}

type TaskReply struct {
	mcp.CallToolResult `json:"-"`
	Raw                json.RawMessage `json:"-"`
}

func (r *TaskReply) MarshalJSON() ([]byte, error) { return r.Raw, nil }
func (r *TaskReply) UnmarshalJSON(data []byte) error {
	if !json.Valid(data) || len(data) > 16<<20 {
		return errors.New("invalid MCP task response")
	}
	r.Raw = append(r.Raw[:0], data...)
	return nil
}
func (r *TaskReply) task(requireDetails bool) (Task, error) {
	var t Task
	var wire map[string]json.RawMessage
	if json.Unmarshal(r.Raw, &t) != nil || json.Unmarshal(r.Raw, &wire) != nil || t.TaskID == "" || len(t.TaskID) > 512 || t.CreatedAt.IsZero() || t.LastUpdatedAt.Before(t.CreatedAt) || len(wire["ttlMs"]) == 0 || t.TTL != nil && *t.TTL < 0 || t.PollInterval != nil && *t.PollInterval < 0 {
		return t, errors.New("invalid MCP task metadata")
	}
	var resultType string
	_ = json.Unmarshal(wire["resultType"], &resultType)
	if requireDetails && resultType != "complete" {
		return t, errors.New("invalid MCP task operation result type")
	}
	switch t.Status {
	case "working", "cancelled":
	case "input_required":
		if requireDetails && len(t.InputRequests) == 0 {
			return t, errors.New("MCP task omitted required input")
		}
	case "completed":
		if requireDetails && (len(t.Result) == 0 || bytes.Equal(t.Result, []byte("null"))) {
			return t, errors.New("MCP task omitted its result")
		}
	case "failed":
		if requireDetails && t.Error == nil {
			return t, errors.New("MCP task omitted its JSON-RPC error")
		}
	default:
		return t, errors.New("invalid MCP task status")
	}
	return t, nil
}
func (r *TaskReply) isTask() bool {
	var wire struct {
		Type string `json:"resultType"`
	}
	return json.Unmarshal(r.Raw, &wire) == nil && wire.Type == "task"
}

func (s *Session) CallToolReply(ctx context.Context, params *mcp.CallToolParams) (*TaskReply, error) {
	if params == nil {
		return nil, errors.New("MCP tool params are required")
	}
	copy := *params
	copy.Meta = ForwardMeta(params.Meta)
	if ctx.Value(taskToolKey{}) == nil {
		inventory := Inventory{}
		for tool, err := range s.Tools(ctx, nil) {
			if err != nil {
				return nil, err
			}
			if err := inventory.Add(tool); err != nil {
				return nil, err
			}
			if tool.Name == params.Name {
				ctx = withTaskTool(ctx, tool)
				break
			}
		}
	}
	return sessionCall(s, ctx, func(ctx context.Context) (*TaskReply, error) {
		return mcp.CallCustomMethod[*mcp.CallToolParams, *TaskReply](ctx, s.ClientSession, taskCallMethod, &copy)
	})
}
func (s *Session) Task(ctx context.Context, method string, params *TaskParams) (*TaskReply, error) {
	if method != "tasks/get" && method != "tasks/update" && method != "tasks/cancel" {
		return nil, errors.New("unsupported MCP task method")
	}
	copy := *params
	copy.Meta = ForwardMeta(params.Meta)
	return sessionCall(s, ctx, func(ctx context.Context) (*TaskReply, error) {
		return mcp.CallCustomMethod[*TaskParams, *TaskReply](ctx, s.ClientSession, method, &copy)
	})
}

func registerTaskMethods(client *mcp.Client) error {
	if err := mcp.AddSendingCustomMethod[*mcp.CallToolParams, *TaskReply](client, taskCallMethod); err != nil {
		return err
	}
	for _, method := range []string{"tasks/get", "tasks/update", "tasks/cancel"} {
		if err := mcp.AddSendingCustomMethod[*TaskParams, *TaskReply](client, method); err != nil {
			return err
		}
	}
	return nil
}

func rewriteTaskAlias(data []byte) ([]byte, error) {
	var wire map[string]json.RawMessage
	if json.Unmarshal(data, &wire) != nil {
		return data, nil
	}
	var method string
	_ = json.Unmarshal(wire["method"], &method)
	if method != taskCallMethod {
		return data, nil
	}
	wire["method"] = json.RawMessage(`"tools/call"`)
	return json.Marshal(wire)
}
func rewriteTaskHTTP(r *http.Request) error {
	if r.Method != http.MethodPost || r.Body == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, (16<<20)+1))
	r.Body.Close()
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return errors.New("MCP request exceeds capacity")
	}
	data, err = rewriteTaskAlias(data)
	if err != nil {
		return err
	}
	var message struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(data, &message) == nil && message.Method == "tools/call" && message.Params.Name != "" {
		r.Header.Set("Mcp-Name", message.Params.Name)
	}
	if message.Method == "tools/call" {
		var params mcp.CallToolParams
		if err := json.Unmarshal(data, &struct {
			Params *mcp.CallToolParams `json:"params"`
		}{Params: &params}); err != nil {
			return err
		}
		tool, _ := r.Context().Value(taskToolKey{}).(*mcp.Tool)
		headers, err := taskParameterHeaders(tool, params.Arguments)
		if err != nil {
			return err
		}
		for name, values := range headers {
			r.Header[name] = values
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	if r.Header.Get("MCP-Method") == taskCallMethod {
		r.Header.Set("MCP-Method", "tools/call")
	}
	return nil
}

type taskAliasTransport struct{ mcp.Transport }

func (t taskAliasTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return taskAliasConnection{c}, nil
}

type taskAliasConnection struct{ mcp.Connection }

func (c taskAliasConnection) Write(ctx context.Context, msg jsonrpc.Message) error {
	if req, ok := msg.(*jsonrpc.Request); ok && req.Method == taskCallMethod {
		copy := *req
		copy.Method = "tools/call"
		msg = &copy
	}
	return c.Connection.Write(ctx, msg)
}
