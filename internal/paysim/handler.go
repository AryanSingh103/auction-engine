package paysim

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Faults decides, per request, how the simulator misbehaves.
type Faults struct {
	// FailureRate is the share of requests that fail. Half of them fail
	// before charging (503, nothing happened); the other half charge and
	// then answer 500, the "lost response" a client cannot tell apart from
	// the first kind, which is why it must retry with the same key.
	FailureRate float64
	// HangRate is the share of requests that hang for Hang after
	// charging, so the client times out on a charge that went through.
	HangRate float64
	Hang     time.Duration
	// Roll returns a uniform number in [0, 1). Tests replace it to choose
	// the fault.
	Roll func() float64
}

// DeclinedSuffix: a charge whose amount in cents ends in 13 is declined
// (402), deterministically. It is the permanent failure that settlement
// must not retry, available on demand to tests.
const DeclinedSuffix = 13

type chargeRequest struct {
	Amount     int64 `json:"amount"`
	CustomerID int64 `json:"customer_id"`
}

type chargeResponse struct {
	Charge
	Replayed bool `json:"replayed"`
}

// NewHandler serves POST /charges. The Idempotency-Key header is required.
func NewHandler(store *Store, faults Faults, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /charges", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		var req chargeRequest
		if key == "" || json.NewDecoder(r.Body).Decode(&req) != nil || req.Amount <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key and a positive amount are required"})
			return
		}

		roll := faults.Roll()
		if roll < faults.FailureRate/2 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "injected: unavailable"})
			return
		}
		if req.Amount%100 == DeclinedSuffix {
			writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "card declined"})
			return
		}

		c, replayed, err := store.Charge(r.Context(), key, req.Amount, req.CustomerID)
		switch {
		case errors.Is(err, ErrKeyReused):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		case err != nil:
			logger.Error("charge failed", slog.Any("error", err))
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
			return
		}

		if roll < faults.FailureRate {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "injected: response lost after charging"})
			return
		}
		if roll < faults.FailureRate+faults.HangRate {
			select {
			case <-time.After(faults.Hang):
			case <-r.Context().Done():
				return
			}
		}
		writeJSON(w, http.StatusOK, chargeResponse{Charge: c, Replayed: replayed})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
