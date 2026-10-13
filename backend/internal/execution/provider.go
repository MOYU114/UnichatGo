package execution

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"unichatgo/internal/models"
)

const (
	DefaultMaxOutputTokens = 3000
	MaxProviderInputBytes  = 16 << 20
	MaxProviderReplyBytes  = 4 << 20
	MaxImageDimension      = 2048
	ImageTokenOverhead     = 32768
	ImageTileSize          = 512
	ImageTokensPerTile     = 8192
)

type Provider interface {
	Generate(context.Context, CallInput) (string, error)
}

type CallInput struct {
	Provider, Model, BaseURL, Token string
	Messages                        []models.Message
	Attachments                     []Attachment
	MaxOutputTokens                 int
}

// ProviderError never includes response bodies, endpoints or credentials.
type ProviderError struct {
	Code      string
	Uncertain bool
}

func (e *ProviderError) Error() string { return e.Code }

type HTTPProvider struct{ client *http.Client }

func NewHTTPProvider(timeout time.Duration) *HTTPProvider {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A fresh connection for each POST also rules out transport replay on a stale
	// reused connection. No SDK, redirect or application retry can add a call.
	transport.DisableKeepAlives = true
	return &HTTPProvider{client: &http.Client{
		Timeout: timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (p *HTTPProvider) Close() { p.client.CloseIdleConnections() }

func providerError(code string, uncertain bool) error {
	return &ProviderError{Code: code, Uncertain: uncertain}
}

// ValidateAttachments is also used before acceptance: unsupported documents must
// not be accepted and then silently omitted from the model input.
func ValidateAttachments(attachments []Attachment) error {
	total := 0
	for _, a := range attachments {
		total += len(a.Data)
		if total > MaxProviderInputBytes || len(a.Data) == 0 || len(a.Name) > 1024 {
			return providerError("attachment_invalid", false)
		}
		if a.Size > 0 && a.Size != int64(len(a.Data)) {
			return providerError("attachment_invalid", false)
		}
		kind, err := attachmentKind(a)
		if err != nil {
			return err
		}
		if kind == "text" && (!utf8.Valid(a.Data) || bytes.ContainsRune(a.Data, 0)) {
			return providerError("attachment_not_utf8", false)
		}
		if kind == "image" {
			if _, _, err := imageDimensions(a); err != nil {
				return err
			}
		}
	}
	return nil
}

// EstimateTokens is deliberately an over-reservation, not a measured tokenizer.
// Supported conventional adapters reserve one token per UTF-8 byte, framing,
// maximum output, and 32768 + 8192 per 512px tile for each static image. This
// policy leaves margin above documented OpenAI tile/patch charges without making
// a normal image exceed the default 200000 TPM. It is not a universal tokenizer
// bound: other providers/custom upstreams require a deployment accounting audit.
func EstimateTokens(in CallInput) (int64, error) {
	if err := ValidateCallInput(in); err != nil {
		return 0, err
	}
	output := in.MaxOutputTokens
	if output == 0 {
		output = DefaultMaxOutputTokens
	}
	tokens := int64(output + 1024)
	for _, m := range in.Messages {
		tokens += int64(len(m.Content) + 64)
	}
	for _, a := range in.Attachments {
		kind, _ := attachmentKind(a)
		if kind == "text" {
			tokens += int64(len(attachmentText(a)) + 64)
		} else {
			width, height, err := imageDimensions(a)
			if err != nil {
				return 0, err
			}
			tiles := int64((width+ImageTileSize-1)/ImageTileSize) * int64((height+ImageTileSize-1)/ImageTileSize)
			tokens += tiles*ImageTokensPerTile + ImageTokenOverhead
		}
	}
	return tokens, nil
}

func imageDimensions(a Attachment) (int, int, error) {
	invalid := providerError("attachment_invalid_image", false)
	media := attachmentMIME(a)
	if http.DetectContentType(a.Data) != media {
		return 0, 0, invalid
	}
	var width, height int
	if media == "image/webp" {
		var valid bool
		width, height, valid = webpDimensions(a.Data)
		if !valid {
			return 0, 0, invalid
		}
	} else {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(a.Data))
		if err != nil {
			return 0, 0, invalid
		}
		width, height = cfg.Width, cfg.Height
	}
	if width <= 0 || height <= 0 || width > MaxImageDimension || height > MaxImageDimension {
		return 0, 0, providerError("attachment_image_dimensions", false)
	}
	if media == "image/png" && !staticPNG(a.Data) {
		return 0, 0, invalid
	}
	if media == "image/gif" && !staticGIF(a.Data, width, height) {
		return 0, 0, invalid
	}
	return width, height, nil
}

func staticPNG(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	for pos := 8; pos+12 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		if size > len(data)-pos-12 {
			return false
		}
		kind := string(data[pos+4 : pos+8])
		if kind == "acTL" || kind == "fcTL" || kind == "fdAT" {
			return false
		}
		pos += 12 + size
		if kind == "IEND" {
			return size == 0 && pos == len(data)
		}
	}
	return false
}

func staticGIF(data []byte, width, height int) bool {
	if len(data) < 13 {
		return false
	}
	pos := 13
	if data[10]&0x80 != 0 {
		pos += 3 * (1 << ((data[10] & 7) + 1))
	}
	frames := 0
	skipBlocks := func() bool {
		for pos < len(data) {
			size := int(data[pos])
			pos++
			if size == 0 {
				return true
			}
			if size > len(data)-pos {
				return false
			}
			pos += size
		}
		return false
	}
	for pos < len(data) {
		tag := data[pos]
		pos++
		switch tag {
		case 0x3b:
			return frames == 1 && pos == len(data)
		case 0x21:
			if pos >= len(data) {
				return false
			}
			pos++
			if !skipBlocks() {
				return false
			}
		case 0x2c:
			frames++
			if frames > 1 || pos+9 > len(data) {
				return false
			}
			x := int(binary.LittleEndian.Uint16(data[pos : pos+2]))
			y := int(binary.LittleEndian.Uint16(data[pos+2 : pos+4]))
			w := int(binary.LittleEndian.Uint16(data[pos+4 : pos+6]))
			h := int(binary.LittleEndian.Uint16(data[pos+6 : pos+8]))
			if w <= 0 || h <= 0 || x+w > width || y+h > height {
				return false
			}
			flags := data[pos+8]
			pos += 9
			if flags&0x80 != 0 {
				pos += 3 * (1 << ((flags & 7) + 1))
			}
			if pos >= len(data) {
				return false
			}
			pos++
			if !skipBlocks() {
				return false
			}
		default:
			return false
		}
	}
	return false
}

func webpDimensions(data []byte) (int, int, bool) {
	if len(data) < 20 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return 0, 0, false
	}
	width, height, frames := 0, 0, 0
	u24 := func(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }
	for pos := 12; pos < len(data); {
		if pos+8 > len(data) {
			return 0, 0, false
		}
		kind := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		pos += 8
		if size > len(data)-pos {
			return 0, 0, false
		}
		chunk := data[pos : pos+size]
		w, h := 0, 0
		switch kind {
		case "ANIM", "ANMF":
			return 0, 0, false
		case "VP8X":
			if size != 10 || chunk[0]&2 != 0 || width != 0 {
				return 0, 0, false
			}
			width, height = u24(chunk[4:7])+1, u24(chunk[7:10])+1
		case "VP8 ":
			if size < 10 || chunk[0]&1 != 0 || !bytes.Equal(chunk[3:6], []byte{0x9d, 0x01, 0x2a}) {
				return 0, 0, false
			}
			w, h = int(binary.LittleEndian.Uint16(chunk[6:8])&0x3fff), int(binary.LittleEndian.Uint16(chunk[8:10])&0x3fff)
		case "VP8L":
			if size < 5 || chunk[0] != 0x2f || chunk[4]&0xe0 != 0 {
				return 0, 0, false
			}
			bits := binary.LittleEndian.Uint32(chunk[1:5])
			w, h = int(bits&0x3fff)+1, int((bits>>14)&0x3fff)+1
		}
		if w > 0 || h > 0 {
			frames++
			if frames > 1 || w == 0 || h == 0 {
				return 0, 0, false
			}
			if width == 0 {
				width, height = w, h
			} else if w != width || h != height {
				return 0, 0, false
			}
		}
		pos += size + size%2
		if pos > len(data) {
			return 0, 0, false
		}
	}
	return width, height, frames == 1
}

func attachmentMIME(a Attachment) string {
	media, _, err := mime.ParseMediaType(a.MIME)
	if err != nil {
		return ""
	}
	return strings.ToLower(media)
}

func attachmentKind(a Attachment) (string, error) {
	media := attachmentMIME(a)
	switch media {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return "image", nil
	case "application/json", "application/xml", "application/csv":
		return "text", nil
	default:
		if strings.HasPrefix(media, "text/") {
			return "text", nil
		}
		return "", providerError("attachment_unsupported", false)
	}
}

func ValidateCallInput(in CallInput) error {
	if len(in.Model) == 0 || len(in.Model) > 255 || strings.TrimSpace(in.Model) != in.Model || strings.ContainsAny(in.Model, "\r\n/?#") {
		return providerError("provider_model_invalid", false)
	}
	if _, err := providerEndpoint(in); err != nil {
		return err
	}
	if in.MaxOutputTokens < 0 || in.MaxOutputTokens > 32768 {
		return providerError("provider_output_budget_invalid", false)
	}
	if len(in.Messages) == 0 || in.Messages[len(in.Messages)-1].Role != models.RoleUser {
		return providerError("provider_messages_invalid", false)
	}
	total := 0
	for _, m := range in.Messages {
		if m.Role != models.RoleUser && m.Role != models.RoleAssistant && m.Role != models.RoleSystem {
			return providerError("provider_messages_invalid", false)
		}
		if !utf8.ValidString(m.Content) {
			return providerError("provider_messages_invalid", false)
		}
		total += len(m.Content)
	}
	for _, a := range in.Attachments {
		total += len(a.Data)
	}
	if total > MaxProviderInputBytes {
		return providerError("provider_input_too_large", false)
	}
	return ValidateAttachments(in.Attachments)
}

func providerEndpoint(in CallInput) (string, error) {
	base := strings.TrimRight(in.BaseURL, "/")
	if base == "" {
		switch in.Provider {
		case "openai":
			base = "https://api.openai.com/v1"
		case "claude":
			base = "https://api.anthropic.com"
		case "gemini":
			base = "https://generativelanguage.googleapis.com/v1beta"
		default:
			return "", providerError("provider_unsupported", false)
		}
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", providerError("provider_endpoint_invalid", false)
	}
	// BaseURL is trusted deployment configuration, frozen at acceptance, never a
	// request-supplied fetch URL. HTTP supports isolated mock/local deployments.
	switch in.Provider {
	case "openai":
		if u.Path == "" {
			base += "/v1"
		}
		return base + "/chat/completions", nil
	case "claude":
		if !strings.HasSuffix(u.Path, "/v1") {
			base += "/v1"
		}
		return base + "/messages", nil
	case "gemini":
		if u.Path == "" {
			base += "/v1beta"
		}
		return base + "/models/" + url.PathEscape(in.Model) + ":generateContent", nil
	default:
		return "", providerError("provider_unsupported", false)
	}
}

func (p *HTTPProvider) Generate(ctx context.Context, in CallInput) (string, error) {
	if err := ValidateCallInput(in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Token) == "" || strings.ContainsAny(in.Token, "\r\n") {
		return "", providerError("provider_credentials_invalid", false)
	}
	if in.MaxOutputTokens == 0 {
		in.MaxOutputTokens = DefaultMaxOutputTokens
	}
	body, err := json.Marshal(providerPayload(in))
	if err != nil || len(body) > 2*MaxProviderInputBytes {
		return "", providerError("provider_input_too_large", false)
	}
	endpoint, _ := providerEndpoint(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", providerError("provider_request_invalid", false)
	}
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	switch in.Provider {
	case "openai":
		req.Header.Set("Authorization", "Bearer "+in.Token)
	case "claude":
		req.Header.Set("x-api-key", in.Token)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "gemini":
		req.Header.Set("x-goog-api-key", in.Token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", providerError("provider_transport_unknown", true)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout {
		return "", providerError(fmt.Sprintf("provider_rejected_%d", resp.StatusCode), false)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", providerError(fmt.Sprintf("provider_http_unknown_%d", resp.StatusCode), true)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxProviderReplyBytes+1))
	if err != nil {
		return "", providerError("provider_response_interrupted", true)
	}
	if len(data) > MaxProviderReplyBytes {
		return "", providerError("provider_response_too_large", true)
	}
	return parseProviderReply(in.Provider, data)
}

func attachmentText(a Attachment) string {
	return "\n\nAttached document " + a.Name + ":\n" + string(a.Data)
}

func providerPayload(in CallInput) map[string]any {
	var messages []map[string]any
	var system []string
	for i, msg := range in.Messages {
		text := msg.Content
		var images []Attachment
		if i == len(in.Messages)-1 {
			for _, a := range in.Attachments {
				kind, _ := attachmentKind(a)
				if kind == "text" {
					text += attachmentText(a)
				} else {
					images = append(images, a)
				}
			}
		}
		if msg.Role == models.RoleSystem && in.Provider != "openai" {
			system = append(system, text)
			continue
		}
		role := string(msg.Role)
		switch in.Provider {
		case "openai":
			parts := []map[string]any{{"type": "text", "text": text}}
			for _, a := range images {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + attachmentMIME(a) + ";base64," + base64.StdEncoding.EncodeToString(a.Data)}})
			}
			messages = append(messages, map[string]any{"role": role, "content": parts})
		case "claude":
			parts := []map[string]any{{"type": "text", "text": text}}
			for _, a := range images {
				parts = append(parts, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": attachmentMIME(a), "data": base64.StdEncoding.EncodeToString(a.Data)}})
			}
			messages = append(messages, map[string]any{"role": role, "content": parts})
		case "gemini":
			if role == "assistant" {
				role = "model"
			}
			parts := []map[string]any{{"text": text}}
			for _, a := range images {
				parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": attachmentMIME(a), "data": base64.StdEncoding.EncodeToString(a.Data)}})
			}
			messages = append(messages, map[string]any{"role": role, "parts": parts})
		}
	}
	switch in.Provider {
	case "openai":
		return map[string]any{"model": in.Model, "messages": messages, "max_completion_tokens": in.MaxOutputTokens, "stream": false, "n": 1}
	case "claude":
		payload := map[string]any{"model": in.Model, "messages": messages, "max_tokens": in.MaxOutputTokens, "stream": false}
		if len(system) > 0 {
			payload["system"] = strings.Join(system, "\n\n")
		}
		return payload
	default:
		payload := map[string]any{"contents": messages, "generationConfig": map[string]any{"maxOutputTokens": in.MaxOutputTokens, "candidateCount": 1}}
		if len(system) > 0 {
			payload["systemInstruction"] = map[string]any{"parts": []map[string]any{{"text": strings.Join(system, "\n\n")}}}
		}
		return payload
	}
}

func parseProviderReply(provider string, data []byte) (string, error) {
	if !utf8.Valid(data) {
		return "", providerError("provider_response_invalid", true)
	}
	var text string
	switch provider {
	case "openai":
		var reply struct {
			Choices []struct {
				Finish  string `json:"finish_reason"`
				Message struct {
					Content   string            `json:"content"`
					Refusal   string            `json:"refusal"`
					ToolCalls []json.RawMessage `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &reply) != nil || len(reply.Choices) != 1 {
			return "", providerError("provider_response_invalid", true)
		}
		choice := reply.Choices[0]
		if choice.Finish == "content_filter" || choice.Message.Refusal != "" {
			return "", providerError("provider_refused", false)
		}
		if len(choice.Message.ToolCalls) > 0 || choice.Finish == "tool_calls" || choice.Finish == "function_call" {
			return "", providerError("provider_tool_unsupported", false)
		}
		if choice.Finish == "length" {
			return "", providerError("provider_output_truncated", false)
		}
		if choice.Finish != "stop" {
			return "", providerError("provider_completion_unconfirmed", true)
		}
		text = choice.Message.Content
	case "claude":
		var reply struct {
			Type    string `json:"type"`
			Stop    string `json:"stop_reason"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(data, &reply) != nil || reply.Type != "message" {
			return "", providerError("provider_response_invalid", true)
		}
		if reply.Stop == "refusal" {
			return "", providerError("provider_refused", false)
		}
		if reply.Stop == "tool_use" {
			return "", providerError("provider_tool_unsupported", false)
		}
		if reply.Stop == "max_tokens" {
			return "", providerError("provider_output_truncated", false)
		}
		if reply.Stop != "end_turn" && reply.Stop != "stop_sequence" {
			return "", providerError("provider_completion_unconfirmed", true)
		}
		for _, part := range reply.Content {
			if part.Type == "text" {
				text += part.Text
			} else {
				return "", providerError("provider_content_unsupported", false)
			}
		}
	case "gemini":
		var reply struct {
			PromptFeedback struct {
				BlockReason string `json:"blockReason"`
			} `json:"promptFeedback"`
			Candidates []struct {
				Finish  string `json:"finishReason"`
				Content struct {
					Parts []struct {
						Text         string          `json:"text"`
						Thought      bool            `json:"thought"`
						FunctionCall json.RawMessage `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if json.Unmarshal(data, &reply) != nil {
			return "", providerError("provider_response_invalid", true)
		}
		if reply.PromptFeedback.BlockReason != "" {
			return "", providerError("provider_refused", false)
		}
		if len(reply.Candidates) != 1 {
			return "", providerError("provider_response_invalid", true)
		}
		candidate := reply.Candidates[0]
		switch candidate.Finish {
		case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
			return "", providerError("provider_refused", false)
		case "MAX_TOKENS":
			return "", providerError("provider_output_truncated", false)
		case "STOP":
		default:
			return "", providerError("provider_completion_unconfirmed", true)
		}
		for _, part := range candidate.Content.Parts {
			if len(part.FunctionCall) > 0 && string(part.FunctionCall) != "null" {
				return "", providerError("provider_tool_unsupported", false)
			}
			if !part.Thought {
				text += part.Text
			}
		}
	default:
		return "", providerError("provider_unsupported", false)
	}
	if strings.TrimSpace(text) == "" {
		return "", providerError("provider_empty_response", true)
	}
	return text, nil
}
