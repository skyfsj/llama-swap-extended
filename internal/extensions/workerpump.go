package extensions

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// pumpWorkerStdio is the host side of the worker protocol. It writes the
// request line, answers host-call lines through the registry, forwards live
// log records to onLog, and delivers the terminal response line. stdin is
// owned here and closed on return, which ends the worker if it keeps running
// after its response.
func pumpWorkerStdio(ctx context.Context, stdin io.WriteCloser, stdout io.Reader, request *workerRequest, host HostCaller, onLog func(logLine)) <-chan workerResponse {
	responseCh := make(chan workerResponse, 1)
	go func() {
		defer stdin.Close()
		writer := bufio.NewWriter(stdin)
		encoder := json.NewEncoder(writer)
		if encodeErr := encoder.Encode(request); encodeErr != nil {
			return
		}
		if flushErr := writer.Flush(); flushErr != nil {
			return
		}
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64<<10), maxWorkerLine+1)
		for scanner.Scan() {
			line := scanner.Bytes()
			var probe struct {
				Type string `json:"type"`
			}
			if probeErr := json.Unmarshal(line, &probe); probeErr != nil || probe.Type == "" {
				responseCh <- workerResponse{Error: "extension worker protocol: malformed line"}
				return
			}
			switch probe.Type {
			case "call":
				var call hostCallMessage
				if callErr := json.Unmarshal(line, &call); callErr != nil {
					responseCh <- workerResponse{Error: "extension worker protocol: malformed host call"}
					return
				}
				result := answerHostCall(ctx, host, call)
				if encodeErr := encoder.Encode(result); encodeErr != nil {
					return
				}
				if flushErr := writer.Flush(); flushErr != nil {
					return
				}
			case "log":
				// Structured log lines emitted live by the script; the
				// subscriber receives them through the onLog callback. The
				// callback may do file I/O, so it runs off the pump goroutine:
				// blocking here would stall the scanner while the worker keeps
				// writing, and a cmd.Wait() closing stdout mid-wait loses the
				// terminal response (seen as flaky "ended without a response").
				var record logLine
				if logErr := json.Unmarshal(line, &record); logErr != nil {
					responseCh <- workerResponse{Error: "extension worker protocol: malformed log line"}
					return
				}
				if onLog != nil {
					go onLog(record)
				}
			case "response":
				var response workerResponse
				if responseErr := json.Unmarshal(line, &response); responseErr != nil {
					responseCh <- workerResponse{Error: "extension worker protocol: malformed response"}
					return
				}
				responseCh <- response
				return
			default:
				responseCh <- workerResponse{Error: fmt.Sprintf("extension worker protocol: unknown message type %s", probe.Type)}
				return
			}
		}
		// Scanner ended without a terminal response (worker process died or
		// closed stdout early): report EOF so the caller surfaces a clear
		// error instead of hanging on the channel.
		if scanErr := scanner.Err(); scanErr != nil {
			responseCh <- workerResponse{Error: fmt.Sprintf("extension worker stdout: %s", scanErr.Error())}
			return
		}
		responseCh <- workerResponse{Error: "extension worker closed stdout without a response"}
	}()
	return responseCh
}

// answerHostCall runs one host function and wraps the outcome for the worker.
// A Go-level failure becomes ok:false so the script sees a rejection with the
// reason instead of crashing the protocol.
func answerHostCall(ctx context.Context, host HostCaller, call hostCallMessage) hostResultMessage {
	result := hostResultMessage{Type: "result", ID: call.ID}
	value, err := host.Exec(ctx, call.Fn, callWithExtension(call))
	if err != nil {
		result.Error = err.Error()
		return result
	}
	encoded, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		result.Error = fmt.Sprintf("host call result is invalid: %s", marshalErr.Error())
		return result
	}
	result.OK = true
	result.Value = encoded
	return result
}

// callWithExtension stamps the worker-written extension identity into object
// args so host validation can re-resolve the manifest. Non-object payloads
// (raw ctx.host.call users) pass through untouched.
func callWithExtension(call hostCallMessage) json.RawMessage {
	if call.Extension == "" || len(call.Args) == 0 {
		return call.Args
	}
	var payload map[string]any
	if err := json.Unmarshal(call.Args, &payload); err != nil {
		return call.Args
	}
	payload["extension"] = call.Extension
	if merged, err := json.Marshal(payload); err == nil {
		return merged
	}
	return call.Args
}
