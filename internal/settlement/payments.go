// Package settlement charges the winner of every closed auction exactly
// once (invariant 5, docs/decisions/024).
package settlement

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

var (
	// ErrDeclined: the provider definitively refused; no charge exists.
	ErrDeclined = errors.New("payment declined")
	// ErrRejected: the provider refused the request itself (400/422), a
	// bug on our side. Retrying cannot help.
	ErrRejected = errors.New("payment request rejected")
	// ErrUnknownOutcome: no definite answer (5xx, timeout, connection
	// error). A charge may or may not exist; only a retry with the same
	// idempotency key finds out.
	ErrUnknownOutcome = errors.New("payment outcome unknown")
)

// Payer charges a customer under an idempotency key.
type Payer interface {
	Charge(ctx context.Context, key string, amount, customer int64) (chargeID string, err error)
}

// PaymentClient is the HTTP client for the payment provider.
type PaymentClient struct {
	http    *http.Client
	baseURL string
	timeout time.Duration
}

// NewPaymentClient returns a client for the provider at baseURL. timeout
// bounds each attempt; it should be well under the provider's hangs, so a
// hang becomes a quick retry instead of a stalled settlement.
func NewPaymentClient(baseURL string, timeout time.Duration) *PaymentClient {
	return &PaymentClient{http: &http.Client{}, baseURL: baseURL, timeout: timeout}
}

// Charge makes one attempt. Every error wraps ErrDeclined, ErrRejected or
// ErrUnknownOutcome.
func (c *PaymentClient) Charge(ctx context.Context, key string, amount, customer int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	body, err := json.Marshal(map[string]int64{"amount": amount, "customer_id": customer})
	if err != nil {
		return "", fmt.Errorf("%w: encode: %w", ErrRejected, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/charges", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%w: build request: %w", ErrRejected, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)

	resp, err := c.http.Do(req)
	if err != nil {
		// Includes our own timeout: the request may have been processed.
		return "", fmt.Errorf("%w: %w", ErrUnknownOutcome, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// It said yes, but if we cannot read which charge, ask again: the
		// same key returns the same charge.
		var out struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", fmt.Errorf("%w: unreadable success response: %w", ErrUnknownOutcome, err)
		}
		if out.ID == "" {
			return "", fmt.Errorf("%w: success response without a charge id", ErrUnknownOutcome)
		}
		return out.ID, nil
	case http.StatusPaymentRequired:
		return "", ErrDeclined
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "", fmt.Errorf("%w: status %d", ErrRejected, resp.StatusCode)
	default:
		return "", fmt.Errorf("%w: status %d", ErrUnknownOutcome, resp.StatusCode)
	}
}
