package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"

	"sevi/internal/httpx"
)

// ChatPart is a piece of the user message: text, or audio (mp3 or wav).
type ChatPart struct {
	Text        string
	Audio       []byte
	AudioFormat string // "mp3" | "wav"
}

// ChatRequest is one system prompt plus one user message made of parts. If
// Schema is set the model must answer with JSON matching it.
type ChatRequest struct {
	System     string
	Parts      []ChatPart
	SchemaName string
	Schema     map[string]any
	// OnRetry, if set, is told when a failed call is about to be retried, so the
	// caller can show the person that it is waiting rather than stuck.
	OnRetry func(attempt int, wait time.Duration, err error)
}

// ChatUsage is what a call consumed, in tokens. AudioTokens and CachedTokens
// are parts of PromptTokens: audio is billed at the audio rate, and cached
// tokens (a repeated prompt prefix the provider remembered) at the cache rate.
type ChatUsage struct {
	PromptTokens     int
	AudioTokens      int
	CachedTokens     int
	CompletionTokens int
}

type ChatResult struct {
	Text         string
	Usage        ChatUsage
	GenerationID string
}

type LLM interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResult, error)
}

// OpenAICompatLLM talks to any OpenAI-compatible chat endpoint (OpenRouter,
// OpenAI, Gemini's compat endpoint, Ollama...) using the official SDK.
type OpenAICompatLLM struct {
	client  openai.Client
	model   string
	baseURL string
}

func NewOpenAICompatLLM(baseURL, key, model string, retries int) *OpenAICompatLLM {
	return &OpenAICompatLLM{
		client:  openai.NewClient(option.WithBaseURL(baseURL), option.WithAPIKey(key), option.WithMaxRetries(retries)),
		model:   model,
		baseURL: baseURL,
	}
}

func (l *OpenAICompatLLM) Chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
	var parts []openai.ChatCompletionContentPartUnionParam
	for _, p := range req.Parts {
		switch {
		case len(p.Audio) > 0:
			parts = append(parts, openai.InputAudioContentPart(openai.ChatCompletionContentPartInputAudioInputAudioParam{
				Data: base64.StdEncoding.EncodeToString(p.Audio), Format: p.AudioFormat,
			}))
		case p.Text != "":
			parts = append(parts, openai.TextContentPart(p.Text))
		}
	}
	params := openai.ChatCompletionNewParams{
		Model: l.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(req.System),
			openai.UserMessage(parts),
		},
	}
	if req.Schema != nil {
		params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name: req.SchemaName, Schema: req.Schema, Strict: openai.Bool(true),
				},
			},
		}
	}
	res, err := l.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return ChatResult{}, l.asStatusError(err)
	}
	if len(res.Choices) == 0 || strings.TrimSpace(res.Choices[0].Message.Content) == "" {
		return ChatResult{}, fmt.Errorf("model %s returned an empty answer", l.model)
	}
	return ChatResult{
		Text:         res.Choices[0].Message.Content,
		GenerationID: res.ID,
		Usage: ChatUsage{
			PromptTokens:     int(res.Usage.PromptTokens),
			AudioTokens:      int(res.Usage.PromptTokensDetails.AudioTokens),
			CachedTokens:     int(res.Usage.PromptTokensDetails.CachedTokens),
			CompletionTokens: int(res.Usage.CompletionTokens),
		},
	}, nil
}

// asStatusError converts the SDK's API error into the same httpx.StatusError
// the rest of sevi uses, so failures are classified in one place.
func (l *OpenAICompatLLM) asStatusError(err error) error {
	var ae *openai.Error
	if errors.As(err, &ae) {
		body := ae.RawJSON()
		if len(body) > 300 {
			body = body[:300]
		}
		return &httpx.StatusError{Host: hostOf(l.baseURL), Code: ae.StatusCode, Status: fmt.Sprintf("%d", ae.StatusCode), Body: body}
	}
	return err
}

func hostOf(base string) string {
	base = strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	if i := strings.IndexByte(base, '/'); i >= 0 {
		base = base[:i]
	}
	return base
}
