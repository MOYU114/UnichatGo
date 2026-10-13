package execution

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"unichatgo/internal/models"
)

func testCall(provider, baseURL string) CallInput {
	return CallInput{
		Provider: provider, Model: "test-model", BaseURL: baseURL, Token: "private-test-token",
		Messages:        []models.Message{{Role: models.RoleSystem, Content: "Be precise."}, {Role: models.RoleUser, Content: "old question"}, {Role: models.RoleAssistant, Content: "old answer"}, {Role: models.RoleUser, Content: "current question"}},
		MaxOutputTokens: 123,
	}
}

func testAttachments(t *testing.T) []Attachment {
	t.Helper()
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return []Attachment{{ID: 1, Name: "note.txt", MIME: "text/plain; charset=utf-8", Data: []byte("document text")}, {ID: 2, Name: "pixel.png", MIME: "image/png", Data: imageBytes.Bytes()}}
}

func TestHTTPProviderAdapters(t *testing.T) {
	tests := []struct{ provider, path, response, authHeader, authValue string }{
		{"openai", "/v1/chat/completions", `{"choices":[{"finish_reason":"stop","message":{"content":"hello"}}]}`, "Authorization", "Bearer private-test-token"},
		{"claude", "/v1/messages", `{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"hello"}]}`, "x-api-key", "private-test-token"},
		{"gemini", "/v1beta/models/test-model:generateContent", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"secret thought","thought":true},{"text":"hello"}]}}]}`, "x-goog-api-key", "private-test-token"},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			var calls atomic.Int64
			attachments := testAttachments(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != tc.path || r.URL.RawQuery != "" {
					t.Errorf("request: %s %s", r.Method, r.URL)
				}
				if got := r.Header.Get(tc.authHeader); got != tc.authValue {
					t.Errorf("auth header missing")
				}
				if tc.provider == "claude" && r.Header.Get("anthropic-version") != "2023-06-01" {
					t.Error("missing Claude protocol version")
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				encoded, _ := json.Marshal(payload)
				for _, content := range []string{"Be precise.", "old question", "old answer", "current question", "document text", base64.StdEncoding.EncodeToString(attachments[1].Data)} {
					if !strings.Contains(string(encoded), content) {
						t.Errorf("missing content %q", content)
					}
				}
				if strings.Count(string(encoded), "current question") != 1 || strings.Count(string(encoded), "document text") != 1 {
					t.Error("current input or attachment duplicated")
				}
				if strings.Contains(string(encoded), "private-test-token") {
					t.Error("credential leaked in body")
				}
				switch tc.provider {
				case "openai":
					if payload["max_completion_tokens"] != float64(123) || payload["stream"] != false || payload["n"] != float64(1) {
						t.Error("incorrect OpenAI budget/stream settings")
					}
				case "claude":
					if payload["max_tokens"] != float64(123) || payload["stream"] != false || payload["system"] != "Be precise." {
						t.Error("incorrect Claude budget/system settings")
					}
				case "gemini":
					config := payload["generationConfig"].(map[string]any)
					if config["maxOutputTokens"] != float64(123) || config["candidateCount"] != float64(1) {
						t.Error("incorrect Gemini budget settings")
					}
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.response)
			}))
			defer server.Close()
			provider := NewHTTPProvider(time.Second)
			defer provider.Close()
			in := testCall(tc.provider, server.URL)
			in.Attachments = attachments
			got, err := provider.Generate(context.Background(), in)
			if err != nil || got != "hello" || calls.Load() != 1 {
				t.Fatalf("got %q, %v; calls=%d", got, err, calls.Load())
			}
		})
	}
}

func TestHTTPProviderDoesNotRetryErrorsOrRedirect(t *testing.T) {
	for _, status := range []int{301, 307, 400, 401, 408, 429, 500, 502, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/redirected")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":"private response body"}`)
			}))
			defer server.Close()
			provider := NewHTTPProvider(time.Second)
			defer provider.Close()
			_, err := provider.Generate(context.Background(), testCall("openai", server.URL))
			uncertain := status < 400 || status >= 500 || status == 408
			assertProviderError(t, err, uncertain)
			if calls.Load() != 1 {
				t.Fatalf("retried: %d calls", calls.Load())
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatalf("response/credential leaked: %v", err)
			}
		})
	}
}

func TestProviderCompletionMarkers(t *testing.T) {
	tests := []struct {
		name, provider, response string
		uncertain                bool
	}{
		{"openai missing finish", "openai", `{"choices":[{"message":{"content":"partial"}}]}`, true},
		{"openai length", "openai", `{"choices":[{"finish_reason":"length","message":{"content":"partial"}}]}`, false},
		{"openai refused", "openai", `{"choices":[{"finish_reason":"stop","message":{"content":null,"refusal":"no"}}]}`, false},
		{"openai tool", "openai", `{"choices":[{"finish_reason":"tool_calls","message":{"content":null,"tool_calls":[{}]}}]}`, false},
		{"openai empty", "openai", `{"choices":[{"finish_reason":"stop","message":{"content":""}}]}`, true},
		{"openai extra JSON", "openai", `{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}]} {}`, true},
		{"claude length", "claude", `{"type":"message","stop_reason":"max_tokens","content":[{"type":"text","text":"partial"}]}`, false},
		{"claude missing type", "claude", `{"stop_reason":"end_turn","content":[{"type":"text","text":"partial"}]}`, true},
		{"claude tool", "claude", `{"type":"message","stop_reason":"tool_use","content":[{"type":"tool_use","id":"x"}]}`, false},
		{"gemini length", "gemini", `{"candidates":[{"finishReason":"MAX_TOKENS","content":{"parts":[{"text":"partial"}]}}]}`, false},
		{"gemini refused", "gemini", `{"promptFeedback":{"blockReason":"SAFETY"}}`, false},
		{"gemini missing finish", "gemini", `{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`, true},
		{"gemini tool", "gemini", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"search"}}]}}]}`, false},
		{"invalid JSON", "openai", `{"choices":[`, true},
		{"SSE not JSON", "openai", "data: {\"choices\":[]}\n\ndata: [DONE]\n", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, err := parseProviderReply(tc.provider, []byte(tc.response))
			assertProviderError(t, err, tc.uncertain)
			if text != "" {
				t.Fatalf("partial output returned as success: %q", text)
			}
		})
	}
}

func TestHTTPProviderTruncationIsTerminalWithoutRetry(t *testing.T) {
	for _, tc := range []struct{ provider, response string }{
		{"openai", `{"choices":[{"finish_reason":"length","message":{"content":"partial"}}]}`},
		{"claude", `{"type":"message","stop_reason":"max_tokens","content":[{"type":"text","text":"partial"}]}`},
		{"gemini", `{"candidates":[{"finishReason":"MAX_TOKENS","content":{"parts":[{"text":"partial"}]}}]}`},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.response)
			}))
			defer server.Close()
			provider := NewHTTPProvider(time.Second)
			defer provider.Close()
			text, err := provider.Generate(context.Background(), testCall(tc.provider, server.URL))
			assertProviderError(t, err, false)
			if err.Error() != "provider_output_truncated" || text != "" || calls.Load() != 1 {
				t.Fatalf("want terminal truncation without output or retry, got %q, %v; calls=%d", text, err, calls.Load())
			}
		})
	}
}

func TestProviderTruncationRequiresCompleteTypedReply(t *testing.T) {
	for _, tc := range []struct{ name, provider, response string }{
		{"openai incomplete JSON", "openai", `{"choices":[{"finish_reason":"length","message":{"content":"partial"}}]`},
		{"openai invalid content type", "openai", `{"choices":[{"finish_reason":"length","message":{"content":42}}]}`},
		{"openai invalid marker type", "openai", `{"choices":[{"finish_reason":["length"],"message":{"content":"partial"}}]}`},
		{"claude incomplete JSON", "claude", `{"type":"message","stop_reason":"max_tokens","content":[{"type":"text","text":"partial"}]`},
		{"claude missing message type", "claude", `{"stop_reason":"max_tokens","content":[{"type":"text","text":"partial"}]}`},
		{"claude invalid marker type", "claude", `{"type":"message","stop_reason":["max_tokens"],"content":[{"type":"text","text":"partial"}]}`},
		{"gemini incomplete JSON", "gemini", `{"candidates":[{"finishReason":"MAX_TOKENS","content":{"parts":[{"text":"partial"}]}}]`},
		{"gemini invalid content type", "gemini", `{"candidates":[{"finishReason":"MAX_TOKENS","content":{"parts":[{"text":42}]}}]}`},
		{"gemini invalid marker type", "gemini", `{"candidates":[{"finishReason":["MAX_TOKENS"],"content":{"parts":[{"text":"partial"}]}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, err := parseProviderReply(tc.provider, []byte(tc.response))
			assertProviderError(t, err, true)
			if text != "" {
				t.Fatalf("partial output returned as success: %q", text)
			}
		})
	}
}

func TestHTTPProviderTruncationInterruptedTransferIsUncertain(t *testing.T) {
	for _, tc := range []struct{ provider, response string }{
		{"openai", `{"choices":[{"finish_reason":"length","message":{"content":"partial"}}]}`},
		{"claude", `{"type":"message","stop_reason":"max_tokens","content":[{"type":"text","text":"partial"}]}`},
		{"gemini", `{"candidates":[{"finishReason":"MAX_TOKENS","content":{"parts":[{"text":"partial"}]}}]}`},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				// Even a valid marker cannot authorize a terminal state if the body transfer failed.
				fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nContent-Type: application/json\r\n\r\n%s", len(tc.response)+10, tc.response)
			}))
			defer server.Close()
			provider := NewHTTPProvider(time.Second)
			defer provider.Close()
			text, err := provider.Generate(context.Background(), testCall(tc.provider, server.URL))
			assertProviderError(t, err, true)
			if text != "" || calls.Load() != 1 {
				t.Fatalf("interrupted output returned or retried: %q, %v; calls=%d", text, err, calls.Load())
			}
		})
	}
}

func assertProviderError(t *testing.T, err error, uncertain bool) {
	t.Helper()
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr.Uncertain != uncertain {
		t.Fatalf("want ProviderError uncertain=%v, got %#v", uncertain, err)
	}
}

func TestHTTPProviderInterruptedResponseIsUncertain(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 1000\r\nContent-Type: application/json\r\n\r\n{\"choices\":[")
	}))
	defer server.Close()
	provider := NewHTTPProvider(time.Second)
	defer provider.Close()
	_, err := provider.Generate(context.Background(), testCall("openai", server.URL))
	assertProviderError(t, err, true)
	if calls.Load() != 1 {
		t.Fatalf("retried interrupted response: %d", calls.Load())
	}
}

func TestHTTPProviderCancellationDoesNotRetry(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
	}))
	defer server.Close()
	defer close(release)
	provider := NewHTTPProvider(time.Second)
	defer provider.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := provider.Generate(ctx, testCall("openai", server.URL)); result <- err }()
	<-entered
	cancel()
	assertProviderError(t, <-result, true)
	if calls.Load() != 1 {
		t.Fatalf("retried cancelled request: %d", calls.Load())
	}
}

func TestHTTPProviderResponseBound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", MaxProviderReplyBytes+1))
	}))
	defer server.Close()
	provider := NewHTTPProvider(time.Second)
	defer provider.Close()
	_, err := provider.Generate(context.Background(), testCall("openai", server.URL))
	assertProviderError(t, err, true)
	if err.Error() != "provider_response_too_large" {
		t.Fatal(err)
	}
}

func TestProviderValidationBeforeNetwork(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	provider := NewHTTPProvider(time.Second)
	defer provider.Close()
	tests := []struct {
		name   string
		mutate func(*CallInput)
	}{
		{"unknown provider", func(in *CallInput) { in.Provider = "other" }},
		{"url credentials", func(in *CallInput) { in.BaseURL = "https://user:password@example.com" }},
		{"url query", func(in *CallInput) { in.BaseURL += "?key=secret" }},
		{"model escape", func(in *CallInput) { in.Model = "../other" }},
		{"missing credential", func(in *CallInput) { in.Token = "" }},
		{"header injection", func(in *CallInput) { in.Token = "key\r\ninjected" }},
		{"output budget", func(in *CallInput) { in.MaxOutputTokens = 1000000 }},
		{"missing current input", func(in *CallInput) { in.Messages = nil }},
		{"unsupported role", func(in *CallInput) { in.Messages[0].Role = "tool" }},
		{"unsupported pdf", func(in *CallInput) {
			in.Attachments = []Attachment{{Name: "x.pdf", MIME: "application/pdf", Data: []byte("%PDF")}}
		}},
		{"invalid UTF8", func(in *CallInput) {
			in.Attachments = []Attachment{{Name: "x.txt", MIME: "text/plain", Data: []byte{0xff}}}
		}},
		{"fake image", func(in *CallInput) {
			in.Attachments = []Attachment{{Name: "x.png", MIME: "image/png", Data: []byte("not an image")}}
		}},
		{"wrong size", func(in *CallInput) {
			in.Attachments = []Attachment{{Name: "x.txt", MIME: "text/plain", Data: []byte("yes"), Size: 1}}
		}},
		{"input too large", func(in *CallInput) { in.Messages[0].Content = strings.Repeat("x", MaxProviderInputBytes+1) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := testCall("openai", server.URL)
			tc.mutate(&in)
			_, err := provider.Generate(context.Background(), in)
			assertProviderError(t, err, false)
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid request reached network: %d", calls.Load())
	}
}

func TestProviderEndpointVersionPrefixes(t *testing.T) {
	for _, tc := range []struct{ provider, base, want string }{
		{"openai", "https://example.test/v1/", "https://example.test/v1/chat/completions"},
		{"claude", "https://example.test/v1/", "https://example.test/v1/messages"},
		{"gemini", "https://example.test/v1beta/", "https://example.test/v1beta/models/test-model:generateContent"},
	} {
		got, err := providerEndpoint(testCall(tc.provider, tc.base))
		if err != nil || got != tc.want {
			t.Fatalf("%s: %q %v", tc.provider, got, err)
		}
	}
}

func TestImageValidationAndConservativeTokens(t *testing.T) {
	in := testCall("openai", "https://example.test")
	in.Attachments = testAttachments(t)
	tokens, err := EstimateTokens(in)
	if err != nil {
		t.Fatal(err)
	}
	if tokens < ImageTokenOverhead+1+int64(in.MaxOutputTokens)+int64(len("document text")) {
		t.Fatalf("image under-reserved: %d", tokens)
	}
	textOnly := in
	textOnly.Attachments = in.Attachments[:1]
	base, err := EstimateTokens(textOnly)
	if err != nil || tokens-base != ImageTokenOverhead+ImageTokensPerTile {
		t.Fatalf("image reservation %d: %v", tokens-base, err)
	}
	var oversized bytes.Buffer
	if err := png.Encode(&oversized, image.NewGray(image.Rect(0, 0, MaxImageDimension+1, 1))); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAttachments([]Attachment{{Name: "large.png", MIME: "image/png", Data: oversized.Bytes()}}); err == nil {
		t.Fatal("oversized image accepted")
	}
	frame := image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{color.Black, color.White})
	var animated bytes.Buffer
	if err := gif.EncodeAll(&animated, &gif.GIF{Image: []*image.Paletted{frame, frame}, Delay: []int{0, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAttachments([]Attachment{{Name: "animated.gif", MIME: "image/gif", Data: animated.Bytes()}}); err == nil {
		t.Fatal("animated GIF accepted")
	}
	var single bytes.Buffer
	if err := gif.Encode(&single, frame, nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAttachments([]Attachment{{Name: "static.gif", MIME: "image/gif", Data: single.Bytes()}}); err != nil {
		t.Fatalf("static GIF rejected: %v", err)
	}
	pngData := testAttachments(t)[1].Data
	// Insert a well-framed animation-control chunk before IEND; no image decode
	// should be needed merely to detect and reject an animation container.
	chunk := make([]byte, 20)
	binary.BigEndian.PutUint32(chunk[:4], 8)
	copy(chunk[4:8], "acTL")
	animatedPNG := append(append(append([]byte{}, pngData[:len(pngData)-12]...), chunk...), pngData[len(pngData)-12:]...)
	if err := ValidateAttachments([]Attachment{{Name: "animated.png", MIME: "image/png", Data: animatedPNG}}); err == nil {
		t.Fatal("animated PNG accepted")
	}
}

func TestWebPDimensionsAndAnimationRejection(t *testing.T) {
	chunk := func(kind string, payload []byte) []byte {
		out := make([]byte, 8+len(payload)+len(payload)%2)
		copy(out, kind)
		binary.LittleEndian.PutUint32(out[4:8], uint32(len(payload)))
		copy(out[8:], payload)
		return out
	}
	riff := func(chunks ...[]byte) []byte {
		out := []byte("RIFF\x00\x00\x00\x00WEBP")
		for _, c := range chunks {
			out = append(out, c...)
		}
		binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
		return out
	}
	static := riff(chunk("VP8L", []byte{0x2f, 0, 0, 0, 0}))
	w, h, ok := webpDimensions(static)
	if !ok || w != 1 || h != 1 {
		t.Fatalf("dimensions %d %d %v", w, h, ok)
	}
	if err := ValidateAttachments([]Attachment{{Name: "static.webp", MIME: "image/webp", Data: static}}); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{riff(chunk("ANIM", []byte{})), riff(chunk("VP8X", []byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0}), chunk("VP8L", []byte{0x2f, 0, 0, 0, 0})), append(static, 0), static[:len(static)-1]} {
		if _, _, ok := webpDimensions(invalid); ok {
			t.Fatal("invalid/animated WebP accepted")
		}
	}
}
