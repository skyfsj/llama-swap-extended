package main

import "testing"

func TestSimpleResponder_StreamRequested(t *testing.T) {
	tests := []struct {
		name  string
		query string
		body  string
		want  bool
	}{
		{name: "query switch", query: "true", want: true},
		{name: "json stream", body: `{"stream":true}`, want: true},
		{name: "json non-stream", body: `{"stream":false}`, want: false},
		{name: "missing stream", body: `{}`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := streamRequested(tt.query, []byte(tt.body)); got != tt.want {
				t.Fatalf("streamRequested(%q, %s) = %v, want %v", tt.query, tt.body, got, tt.want)
			}
		})
	}
}
