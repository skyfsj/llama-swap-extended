package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServer_DecodeJSONBodyRejectsNilAndTrailingValues(t *testing.T) {
	var value map[string]string
	request := httptest.NewRequest(http.MethodPost, "/api/test", nil)
	if err := decodeJSONBody(httptest.NewRecorder(), request, &value, 1024); err != io.EOF {
		t.Fatalf("nil body error = %v, want io.EOF", err)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/test", strings.NewReader(`{"name":"one"} {"name":"two"}`))
	if err := decodeJSONBody(httptest.NewRecorder(), request, &value, 1024); err == nil || !strings.Contains(err.Error(), "single JSON value") {
		t.Fatalf("trailing JSON error = %v", err)
	}
}

func TestServer_DecodeJSONBodyHonorsLimit(t *testing.T) {
	payload := map[string]string{"name": strings.Repeat("x", 2048)}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/test", bytes.NewReader(body))
	var value map[string]string
	if err := decodeJSONBody(httptest.NewRecorder(), request, &value, 128); err == nil {
		t.Fatal("oversized JSON body was accepted")
	}
}
