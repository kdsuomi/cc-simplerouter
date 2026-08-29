package simplerouter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenRouterEndpointsThroughputP50(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models/vendor/model/endpoints":
			io.WriteString(w, `{"data":{"endpoints":[
				{
					"provider_name": "Provider A",
					"tag": "vendor/model-a",
					"quantization": "fp8",
					"context_length": 128000,
					"pricing": {"prompt": "0.000001", "completion": "0.000002"},
					"throughput_last_30m": {"p50": 42.5, "p75": 30.1}
				},
				{
					"provider_name": "Provider B",
					"tag": "vendor/model-b",
					"quantization": "int4",
					"context_length": 64000,
					"pricing": {"prompt": "0.000001", "completion": "0.000001"},
					"throughput_last_30m": {"p50": null}
				}
			]}}`)
		case "/endpoints/zdr":
			io.WriteString(w, `{"data":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	endpoints, err := newOpenRouterClient(nil, srv.URL).endpoints(context.Background(), "test-key", "vendor/model")
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoints) != 2 {
		t.Fatalf("endpoints = %+v", endpoints)
	}
	if endpoints[0].ThroughputP50 != 42.5 {
		t.Errorf("throughput p50 = %v, want 42.5", endpoints[0].ThroughputP50)
	}
	if endpoints[1].ThroughputP50 != 0 {
		t.Errorf("missing throughput p50 = %v, want 0", endpoints[1].ThroughputP50)
	}
	if got := formatThroughput(endpoints[0].ThroughputP50); got != "43 tps" {
		t.Errorf("formatThroughput = %q, want %q", got, "43 tps")
	}
	if got := formatThroughput(endpoints[1].ThroughputP50); got != "-" {
		t.Errorf("formatThroughput missing = %q, want %q", got, "-")
	}
}

func TestOpenRouterEndpointPrivacy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models/vendor/model/endpoints":
			io.WriteString(w, `{"data":{"endpoints":[
				{"provider_name":"Provider A","tag":"provider/zdr","quantization":"fp8"},
				{"provider_name":"Provider B","tag":"provider/standard","quantization":"fp8"}
			]}}`)
		case "/endpoints/zdr":
			io.WriteString(w, `{"data":[{"model_id":"vendor/model","tag":"provider/zdr"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	endpoints, err := newOpenRouterClient(srv.Client(), srv.URL).endpoints(context.Background(), "test-key", "vendor/model")
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoints) != 2 {
		t.Fatalf("endpoints = %+v", endpoints)
	}
	if endpoints[0].Privacy != "zdr" {
		t.Errorf("ZDR endpoint privacy = %q, want zdr", endpoints[0].Privacy)
	}
	if endpoints[1].Privacy != "non-zdr" {
		t.Errorf("standard endpoint privacy = %q, want non-zdr", endpoints[1].Privacy)
	}
}
