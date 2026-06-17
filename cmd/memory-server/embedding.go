package main

import (
	"context"
	"fmt"
	"net/http"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
)

// Embedder turns a piece of text into a vector. Implementations are
// expected to enforce the configured dimension server-side and return an
// error if the provider produced a different one.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Model() string
	Dimension() int
}

// NewEmbedder constructs the embedder selected by cfg.Provider. Unknown
// providers return an error so the operator notices at boot rather than on
// the first write.
func NewEmbedder(cfg EmbeddingProviderConfig) (Embedder, error) {
	switch cfg.Provider {
	case "", "openai", "azure_openai", "ollama":
		return newOpenAICompatible(cfg), nil
	default:
		return nil, fmt.Errorf("unknown embedding provider %q (supported: openai, azure_openai, ollama)", cfg.Provider)
	}
}

// openAICompatible covers any provider that speaks the OpenAI embeddings
// HTTP contract: the OpenAI API, Azure OpenAI (when used via a base URL),
// Ollama, vLLM, and similar OpenAI-compatible servers.
type openAICompatible struct {
	client    openai.Client
	model     string
	dimension int
}

func newOpenAICompatible(cfg EmbeddingProviderConfig) *openAICompatible {
	opts := []option.RequestOption{
		option.WithHTTPClient(http.DefaultClient),
	}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &openAICompatible{
		client:    openai.NewClient(opts...),
		model:     cfg.Model,
		dimension: cfg.Dimension,
	}
}

func (o *openAICompatible) Model() string  { return o.model }
func (o *openAICompatible) Dimension() int { return o.dimension }

func (o *openAICompatible) Embed(ctx context.Context, text string) ([]float32, error) {
	params := openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel(o.model),
		Input: openai.EmbeddingNewParamsInputUnion{OfString: param.NewOpt(text)},
	}
	// Some models (text-embedding-3-*) accept a `dimensions` override.
	// Sending it is a no-op on others that ignore it.
	if o.dimension > 0 {
		params.Dimensions = param.NewOpt(int64(o.dimension))
	}
	resp, err := o.client.Embeddings.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("embeddings.new: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("embeddings.new: empty response")
	}
	emb := resp.Data[0].Embedding
	if len(emb) != o.dimension {
		return nil, fmt.Errorf("embedding dimension mismatch: configured %d, got %d from %s", o.dimension, len(emb), o.model)
	}
	out := make([]float32, len(emb))
	for i, v := range emb {
		out[i] = float32(v)
	}
	return out, nil
}
