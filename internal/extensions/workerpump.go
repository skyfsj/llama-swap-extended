package extensions

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// pumpWorkerStdio is the host side of the worker protocol. It writes the
// request line, answers host-call lines through the registry, forwards live
// log records to onLog, and delivers the terminal response line. stdin is
// owned here and closed on return, which ends the worker if it keeps running
// after its response.
// logSlots caps concurrent log-consumer goroutines for one worker pump. Real
// logging bursts are small; a script logging in a loop would otherwise spawn a
// goroutine per line faster than they drain.
var logSlots = make(chan struct{}, 8)

// pumpWorkerStdioObserved is pumpWorkerStdio with an observer for a terminal
// transport error. The error channel alone is not enough: when the worker is
// blocked writing an oversized line, the run loop is waiting on the process and
// never selects on the channel, so the pump's diagnosis would be lost behind a
// deadline.
func pumpWorkerStdioObserved(ctx context.Context, stdin io.WriteCloser, stdout io.Reader, request *workerRequest, host HostCaller, onLog func(logLine), onTerminal func(error)) <-chan workerResponse {
	responseCh := make(chan workerResponse, 1)
	report := func(err error) {
		if onTerminal != nil {
			onTerminal(err)
		}
		responseCh <- workerResponse{Error: err.Error()}
	}
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
					// Bound the log fan-out: a script that logs in a tight
					// loop must not spawn unbounded goroutines doing file I/O.
					// Log lines are observational, so shedding them under
					// pressure is safe; the terminal response never shares this
					// path.
					select {
					case logSlots <- struct{}{}:
						go func(record logLine) {
							defer func() { <-logSlots }()
							onLog(record)
						}(record)
					default:
					}
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
			// A token longer than the buffer means the worker wrote a response
			// line past the protocol limit. It is blocked writing the rest of
			// that line, so waiting for the process would end at the hook's
			// deadline and report a timeout for what is really a size limit.
			if errors.Is(scanErr, bufio.ErrTooLong) || strings.Contains(scanErr.Error(), "token too long") {
				report(fmt.Errorf("%w: the hook returned a result larger than the %s protocol line",
					ErrHookOutputTooLarge, humanMebi(maxWorkerLine)))
				return
			}
			report(fmt.Errorf("extension worker stdout: %s", scanErr.Error()))
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
