// SPDX-License-Identifier: Apache-2.0

package doccheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The model this checker talks to.
//
// # Why a local server and not an API
//
// The input is this repository's source. Sending it to a service to be told whether its
// comments are accurate is a poor trade, and a checker that needs a key and a network is a
// checker nobody runs. llama.cpp's server speaks one endpoint over HTTP on localhost, so
// the whole dependency is a URL that may be absent - and when it is absent, doccheck says
// so and exits rather than failing anything.
//
// # Why the raw completion endpoint
//
// The chat endpoint applies its own template, which differs between builds and models. The
// prompt below is written out as ChatML because that is what the model was trained on, and
// writing it here means the same bytes reach the model on any llama.cpp build.

// Model answers a prompt.
type Model interface {
	// Ask returns the model's completion for one system and user message. grammar, when
	// not empty, is GBNF constraining the answer.
	Ask(ctx context.Context, system, user string, maxTokens int, grammar string) (string, error)
	// Name identifies the model in a report, so a triage list says what produced it.
	Name() string
}

// DefaultURL is llama.cpp's server on the port the project's notes use.
const DefaultURL = "http://127.0.0.1:8081"

// LlamaServer is a Model backed by llama.cpp's HTTP server.
type LlamaServer struct {
	URL    string
	Client *http.Client
	// model is filled in by Probe from what the server reports it has loaded.
	model string
}

// NewLlamaServer returns a client for a llama.cpp server.
func NewLlamaServer(url string) *LlamaServer {
	if url == "" {
		url = DefaultURL
	}
	return &LlamaServer{
		URL: strings.TrimRight(url, "/"),
		// Generous: a reading of a long unit on a small GPU takes tens of seconds, and
		// a timeout that fires mid-run turns an advisory report into a mystery.
		Client: &http.Client{Timeout: 5 * time.Minute},
	}
}

// Probe reports whether the server is up, and records which model it has loaded.
//
// The model's identity belongs in the report: a verdict from a 1.5B model and a verdict
// from a large one are different evidence, and a report that does not say which it had is
// not reproducible.
func (l *LlamaServer) Probe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.URL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := l.Client.Do(req)
	if err != nil {
		return fmt.Errorf("no model server at %s: %w", l.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("model server at %s answered %s", l.URL, resp.Status)
	}
	// One token, purely to read back which model answered.
	var out completionResponse
	if err := l.post(ctx, completionRequest{
		Prompt: "hello", NPredict: 1, Temperature: 0, Seed: 1,
	}, &out); err != nil {
		return err
	}
	l.model = out.Model
	return nil
}

func (l *LlamaServer) Name() string {
	if l.model == "" {
		return l.URL
	}
	return l.model
}

// chatML wraps a system and user message the way the qwen2 template does.
func chatML(system, user string) string {
	return "<|im_start|>system\n" + system + "<|im_end|>\n" +
		"<|im_start|>user\n" + user + "<|im_end|>\n" +
		"<|im_start|>assistant\n"
}

type completionRequest struct {
	Prompt      string   `json:"prompt"`
	Temperature float64  `json:"temperature"`
	NPredict    int      `json:"n_predict"`
	Seed        int      `json:"seed"`
	Stop        []string `json:"stop,omitempty"`
	Grammar     string   `json:"grammar,omitempty"`
	CachePrompt bool     `json:"cache_prompt"`
}

type completionResponse struct {
	Content string `json:"content"`
	Model   string `json:"model"`
}

func (l *LlamaServer) post(ctx context.Context, in completionRequest, out *completionResponse) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.URL+"/completion",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("model server answered %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Ask sends one exchange.
//
// The temperature is zero and the seed fixed, so two runs over an unchanged tree give the
// same report. That is not the same as being right, but a checker whose output moves on
// its own cannot be reviewed at all.
func (l *LlamaServer) Ask(ctx context.Context, system, user string, maxTokens int, grammar string) (string, error) {
	var out completionResponse
	err := l.post(ctx, completionRequest{
		Prompt:      chatML(system, user),
		Temperature: 0,
		NPredict:    maxTokens,
		Seed:        1,
		Stop:        []string{"<|im_end|>"},
		Grammar:     grammar,
		CachePrompt: true,
	}, &out)
	if err != nil {
		return "", err
	}
	if l.model == "" {
		l.model = out.Model
	}
	return strings.TrimSpace(out.Content), nil
}
