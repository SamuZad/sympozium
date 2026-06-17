package v1alpha1

// EmbeddingConfig configures the embedding model used to vectorize memory
// content. It can be set cluster-wide via Helm values, or overridden per
// Ensemble (for the ensemble's shared pool) or per Agent (for that agent's
// private scope only).
//
// Note: changing the embedding model is destructive — existing vectors live in
// a different geometric space and become incomparable with new ones. The
// memory-server enforces dimension consistency at write time and rejects
// ensemble-scope writes whose model disagrees with the ensemble's pool.
type EmbeddingConfig struct {
	// Provider identifies the embedding API to call.
	// One of: openai, azure_openai, ollama.
	// +kubebuilder:validation:Enum=openai;azure_openai;ollama
	Provider string `json:"provider"`

	// Model is the provider-specific model identifier
	// (e.g. "text-embedding-3-small").
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`

	// Dimension is the output dimensionality of the model. Must match the
	// `vector(N)` column dimension configured for memory-server. When
	// unset, the cluster default dimension is assumed.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Dimension int `json:"dimension,omitempty"`

	// BaseURL points at an OpenAI-compatible endpoint (Ollama, vLLM, etc.).
	// +optional
	BaseURL string `json:"baseURL,omitempty"`

	// APIKeySecretRef references the Secret holding the embedding API key.
	// When unset, the cluster-default embedding secret is used.
	// +optional
	APIKeySecretRef *SecretKeyRef `json:"apiKeySecretRef,omitempty"`
}

// SecretKeyRef references a single key inside a Kubernetes Secret.
type SecretKeyRef struct {
	// Name of the Secret in the same namespace as the referencing resource.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key inside the Secret.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}
