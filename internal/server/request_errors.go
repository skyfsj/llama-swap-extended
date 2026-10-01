package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// decodeJSONBody decodes exactly one JSON value from a bounded control-plane
// request. A surprising number of handlers are also called directly by
// embedders and httptest clients, where Request.Body may be nil instead of
// http.NoBody; guarding that case here avoids a panic inside
// http.MaxBytesReader. The second decode rejects trailing JSON values rather
// than accepting an ambiguous body whose first value happened to be valid.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, destination any, maxBytes int64) error {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return io.EOF
	}
	if maxBytes <= 0 {
		return errors.New("JSON body limit must be positive")
	}
	limited := http.MaxBytesReader(w, r.Body, maxBytes)
	defer limited.Close()
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("request body must contain a single JSON value")
		}
		return err
	}
	return nil
}

// sendModelRewriteError keeps malformed rewrite inputs as client errors while
// preserving the dedicated 413 response for a request that exceeded the
// shared body limit. ReplaceRequestModel is used by selector/profile/form
// middleware, including /upstream paths that do not pass through the normal
// context parser.
func sendModelRewriteError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, swaputil.ErrRequestBodyTooLarge) {
		swaputil.SendError(w, r, err)
		return
	}
	swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
}
