// Command celln-review-provider is a deterministic TLS model fixture for the
// isolated framework walkthrough. It is not a real LLM or production provider.
package main

import (
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", ":8443", "TLS address")
	cert := flag.String("cert", "", "public TLS certificate")
	key := flag.String("key", "", "private TLS key file")
	tokenFile := flag.String("token-file", "", "private fixture credential file")
	flag.Parse()
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		log.Fatal("cannot read fixture credential")
	}
	expected := "Bearer " + strings.TrimSpace(string(token))
	if len(expected) < 24 {
		log.Fatal("fixture credential is too short")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(expected)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 262145))
		if err != nil || len(raw) > 262144 {
			http.Error(w, "bounded request required", 400)
			return
		}
		var request struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(raw, &request) != nil || len(request.Messages) == 0 {
			http.Error(w, "invalid request", 400)
			return
		}
		var user, result string
		for _, message := range request.Messages {
			var text string
			if json.Unmarshal(message.Content, &text) != nil {
				continue
			}
			if message.Role == "user" {
				user = text
				result = ""
			}
			if message.Role == "tool" {
				result = text
			}
		}
		var message any
		finish := "stop"
		if result != "" {
			message = map[string]any{"role": "assistant", "content": result}
		} else {
			if user == "" || len(user) > 2048 {
				http.Error(w, "bounded user text required", 400)
				return
			}
			args, _ := json.Marshal(map[string]string{"text": user})
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "uppercase-review-1", "type": "function", "function": map[string]any{"name": "uppercase", "arguments": string(args)}}}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "review-fixture", "object": "chat.completion", "model": request.Model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 8, "total_tokens": 9}})
	})
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, MaxHeaderBytes: 16384, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	log.Print("deterministic review fixture listening; not a real LLM")
	if server.ListenAndServeTLS(*cert, *key) != nil {
		log.Fatal("fixture TLS server stopped")
	}
}
