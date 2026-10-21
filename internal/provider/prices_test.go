package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenRouterPricesToleratesOddFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("output_modalities") != "all" {
			t.Errorf("must ask for all modalities or speech models are missing: %s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"data":[
			{"id":"google/gemini-3.8-flash-tts","pricing":{"prompt":"0.0000005","completion":"0.000009"}},
			{"id":"a/llm","pricing":{"prompt":"0.0000001","completion":"0.0000004","audio":"0.0000003","input_cache_read":"0.00000001","overrides":[{"x":1}]}},
			{"id":"router/pseudo","pricing":{"prompt":"-1000000","completion":"-1000000"}}]}`))
	}))
	defer srv.Close()

	got, err := OpenRouterPrices(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got["google/gemini-3.8-flash-tts"] != (Price{Prompt: 5e-7, Completion: 9e-6}) {
		t.Errorf("tts: %+v", got["google/gemini-3.8-flash-tts"])
	}
	if got["a/llm"].Audio != 3e-7 || got["a/llm"].CacheRead != 1e-8 {
		t.Errorf("audio and cache-read prices must be kept: %+v", got["a/llm"])
	}
	if _, ok := got["router/pseudo"]; ok {
		t.Error("negative placeholder prices must be skipped")
	}
}
