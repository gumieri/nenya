package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/nenya/config"
	"github.com/nenya/internal/util"
)

// systemOneRequest is the decision request body: the state (the
// adjudicated excerpt, capped) plus the contract-derived typed
// questions. Shape follows the validated System One contract (see
// docs/SYSTEM_ONE.md): questions is an id-keyed map, each entry a
// typed question with instructions and (for choice) criteria.
type systemOneRequest struct {
	Model     string                        `json:"model"`
	State     string                        `json:"state"`
	Questions map[string]systemOneQuestionW `json:"questions"`
}

type systemOneQuestionW struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

// systemOneResponse is the typed answer envelope, keyed by question
// id. Confidence is a pointer so an explicit 0 is distinguishable from
// an absent field (a zero-confidence answer must cascade, not be read
// as a decisive 1). Type/Noul/Score/Probabilities are decoded for
// contract fidelity but unused by the choice transport. Unknown
// choices and malformed envelopes are operational failures
// (fail-closed).
type systemOneResponse struct {
	Answers map[string]struct {
		Type          string             `json:"type"`
		Choice        string             `json:"choice"`
		Noul          float64            `json:"noul"`
		Score         float64            `json:"score"`
		Confidence    *float64           `json:"confidence"`
		Probabilities map[string]float64 `json:"probabilities"`
	} `json:"answers"`
}

// callEngineSystemOne sends the contract-derived question to a System
// One target and returns the typed choice. The state is capped at
// SystemOneStateCapBytes; the answer's choice must be one of the
// question's choices; a confidence below the question's threshold
// returns ErrSystemOneLowConfidence so the chain cascades to the next
// target. Same retry/timeout discipline as the chat transports.
func callEngineSystemOne(ctx context.Context, httpClient *http.Client, provider *config.Provider,
	engine config.EngineConfig, injectAPIKey func(string, http.Header) error, q *systemOneQuestion) (string, error) {
	if q == nil || len(q.Choices) == 0 {
		return "", errors.New("system one: empty question")
	}
	endpoint := provider.FormatURLs[config.FormatKeySystemOne]
	if endpoint == "" {
		endpoint = provider.URL
	}

	encoded, err := marshalSystemOneRequest(engine.Model, q)
	if err != nil {
		return "", err
	}

	maxRetries := provider.MaxRetryAttempts
	if maxRetries <= 0 {
		maxRetries = util.DefaultMaxRetryAttempts()
	}

	resp, err := util.DoWithRetryResp(ctx, maxRetries, func() (*http.Response, error) {
		return postSystemOne(ctx, httpClient, endpoint, engine.Provider, injectAPIKey, encoded)
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	choice, confidence, err := decodeSystemOneAnswer(resp, q)
	if err != nil {
		return "", err
	}
	if q.ConfidenceBelow > 0 && confidence < q.ConfidenceBelow {
		return "", fmt.Errorf("%w: confidence %.2f < %.2f", ErrSystemOneLowConfidence, confidence, q.ConfidenceBelow)
	}
	// Return the contract-shaped verdict object synthesized from the
	// already-validated typed choice: the Judge's single path re-reads
	// this through ParseVerdict (enum re-validated), so no prose is
	// ever parsed on the System One transport.
	verdict, err := json.Marshal(map[string]string{"verdict": choice})
	if err != nil {
		return "", fmt.Errorf("system one: encode verdict: %w", err)
	}
	return string(verdict), nil
}

// marshalSystemOneRequest builds and encodes the decision request body.
// The contract's verdict enum becomes the choice question's criteria
// map (each verdict labeled by its own name; the contract prompt in
// instructions carries the decision rules).
func marshalSystemOneRequest(model string, q *systemOneQuestion) ([]byte, error) {
	criteria := make(map[string]string, len(q.Choices))
	for _, c := range q.Choices {
		criteria[c] = c
	}
	encoded, err := json.Marshal(systemOneRequest{
		Model: model,
		State: capSystemOneState(q.state),
		Questions: map[string]systemOneQuestionW{
			q.ID: {
				Type:         "choice",
				Instructions: q.Prompt,
				Criteria:     criteria,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("system one: marshal payload: %w", err)
	}
	return encoded, nil
}

// postSystemOne performs one decision request attempt. 4xx responses
// other than 429 are permanent (not retried); network errors and 5xx
// retry.
func postSystemOne(ctx context.Context, httpClient *http.Client, endpoint, providerName string,
	injectAPIKey func(string, http.Header) error, encoded []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if authErr := injectAPIKey(providerName, req.Header); authErr != nil {
		return nil, fmt.Errorf("system one auth failed: %w", authErr)
	}
	r, err := httpClient.Do(req)
	if err != nil {
		if r != nil {
			_ = r.Body.Close()
		}
		return nil, err
	}
	if r.StatusCode < 400 {
		return r, nil
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, MaxErrorBodyBytes))
	_ = r.Body.Close()
	statusErr := fmt.Errorf("system one returned status %d: %s", r.StatusCode, string(body))
	if r.StatusCode != http.StatusTooManyRequests && r.StatusCode < 500 {
		return nil, &util.PermanentError{Err: statusErr}
	}
	return nil, statusErr
}

// decodeSystemOneAnswer reads the typed answer envelope and extracts
// the contract-validated choice.
func decodeSystemOneAnswer(resp *http.Response, q *systemOneQuestion) (string, float64, error) {
	var response systemOneResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, MaxOllamaResponseBytes)).Decode(&response); err != nil {
		return "", 0, fmt.Errorf("system one: decode response: %w", err)
	}
	return extractSystemOneAnswer(&response, q)
}

// extractSystemOneAnswer finds the question's answer and validates the
// choice against the contract enum (fail-closed).
func extractSystemOneAnswer(resp *systemOneResponse, q *systemOneQuestion) (choice string, confidence float64, err error) {
	a, ok := resp.Answers[q.ID]
	if !ok {
		return "", 0, errors.New("system one response missing answer for question " + q.ID)
	}
	for _, c := range q.Choices {
		if a.Choice == c {
			conf := 1.0
			if a.Confidence != nil {
				// Preserve an explicit 0 so it can cascade.
				conf = *a.Confidence
			}
			return a.Choice, conf, nil
		}
	}
	return "", 0, fmt.Errorf("system one answer choice %q outside contract enum", a.Choice)
}

// capSystemOneState caps the state at the 512-token window.
func capSystemOneState(state string) string {
	if len(state) <= SystemOneStateCapBytes {
		return state
	}
	keep := SystemOneStateCapBytes
	for keep > 0 && state[keep]&0xC0 == 0x80 {
		keep--
	}
	return state[:keep]
}
