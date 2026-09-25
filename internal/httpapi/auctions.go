package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/AryanSingh103/auction-engine/internal/auction"
)

// maxBidBody caps the request body. A bid is one small JSON object; the cap
// stops a client from making the server buffer an arbitrarily large body.
const maxBidBody = 1 << 10

// maxIdempotencyKey matches the CHECK constraint on bids.idempotency_key.
const maxIdempotencyKey = 255

type auctionResponse struct {
	ID              int64          `json:"id"`
	ItemID          int64          `json:"item_id"`
	StartAt         time.Time      `json:"start_at"`
	EndAt           time.Time      `json:"end_at"`
	StartingPrice   int64          `json:"starting_price"`
	MinIncrement    int64          `json:"min_increment"`
	Status          auction.Status `json:"status"`
	CurrentPrice    *int64         `json:"current_price"`
	CurrentLeaderID *int64         `json:"current_leader_id"`
	// CurrentBidID is the head of the bid chain: live clients compare it
	// with incoming bids' prev_bid_id to detect missed messages.
	CurrentBidID *int64 `json:"current_bid_id"`
	MinimumBid   int64  `json:"minimum_bid"`
}

func handleGetAuction(svc *auction.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "auctionID")
		if !ok {
			writeError(w, logger, http.StatusNotFound, "auction_not_found", "auction not found")
			return
		}
		a, err := svc.GetAuction(r.Context(), id)
		if err != nil {
			writeServiceError(w, r, logger, err)
			return
		}
		writeJSON(w, logger, http.StatusOK, toAuctionResponse(a))
	}
}

func toAuctionResponse(a auction.Auction) auctionResponse {
	resp := auctionResponse{
		ID: a.ID, ItemID: a.ItemID, StartAt: a.StartAt, EndAt: a.EndAt,
		StartingPrice: a.StartingPrice, MinIncrement: a.MinIncrement, Status: a.Status,
		MinimumBid: a.MinimumBid(),
	}
	if a.Head != nil {
		resp.CurrentPrice, resp.CurrentLeaderID, resp.CurrentBidID = &a.Head.Price, &a.Head.UserID, &a.Head.BidID
	}
	return resp
}

type placeBidBody struct {
	// Pointer so a missing field is distinguishable from 0.
	Amount *int64 `json:"amount"`
}

// handlePlaceBid serves POST /auctions/{auctionID}/bids.
//
// Headers: X-User-ID (the bidder; there is no real authentication, see
// docs/decisions/010) and Idempotency-Key (required; a retry with the same
// key returns the original bid). Body: {"amount": <cents>}.
func handlePlaceBid(svc *auction.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auctionID, ok := pathID(r, "auctionID")
		if !ok {
			writeError(w, logger, http.StatusNotFound, "auction_not_found", "auction not found")
			return
		}
		userID, err := strconv.ParseInt(r.Header.Get("X-User-ID"), 10, 64)
		if err != nil || userID <= 0 {
			writeError(w, logger, http.StatusUnauthorized, "missing_user", "X-User-ID header must be a positive integer user id")
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if key == "" || len(key) > maxIdempotencyKey {
			writeError(w, logger, http.StatusBadRequest, "invalid_idempotency_key",
				fmt.Sprintf("Idempotency-Key header is required, at most %d bytes", maxIdempotencyKey))
			return
		}
		amount, err := decodeBidBody(w, r)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}

		bid, replayed, err := svc.PlaceBid(r.Context(), auction.PlaceBidRequest{
			AuctionID: auctionID, UserID: userID, Amount: amount, IdempotencyKey: key,
		})
		if err != nil {
			writeServiceError(w, r, logger, err)
			return
		}
		if replayed {
			w.Header().Set("Idempotent-Replayed", "true")
			writeJSON(w, logger, http.StatusOK, bid)
			return
		}
		writeJSON(w, logger, http.StatusCreated, bid)
	}
}

func decodeBidBody(w http.ResponseWriter, r *http.Request) (int64, error) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBidBody))
	dec.DisallowUnknownFields()
	var body placeBidBody
	if err := dec.Decode(&body); err != nil {
		return 0, fmt.Errorf("body must be a JSON object {\"amount\": <cents>}: %w", err)
	}
	// Exactly one JSON value: reject trailing data such as a second object.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return 0, errors.New("body must contain a single JSON object")
	}
	if body.Amount == nil {
		return 0, errors.New("amount is required")
	}
	return *body.Amount, nil
}

// pathID parses a positive int64 URL parameter.
func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	return id, err == nil && id > 0
}

// serviceErrors maps domain errors to responses. Order matters only in that
// the first match wins; the sentinels are distinct.
var serviceErrors = []struct {
	err    error
	status int
	code   string
}{
	{auction.ErrAuctionNotFound, http.StatusNotFound, "auction_not_found"},
	{auction.ErrUnknownUser, http.StatusUnauthorized, "unknown_user"},
	{auction.ErrInvalidAmount, http.StatusBadRequest, "invalid_amount"},
	{auction.ErrIdempotencyConflict, http.StatusUnprocessableEntity, "idempotency_key_reused"},
	{auction.ErrAuctionNotOpen, http.StatusConflict, "auction_not_open"},
	{auction.ErrAuctionNotStarted, http.StatusConflict, "auction_not_started"},
	{auction.ErrAuctionEnded, http.StatusConflict, "auction_ended"},
	{auction.ErrSelfOutbid, http.StatusConflict, "self_outbid"},
	{auction.ErrBidTooLow, http.StatusConflict, "bid_too_low"},
	// Retry-safe: nothing was written, and the idempotency key covers it.
	{auction.ErrContention, http.StatusServiceUnavailable, "contention"},
}

func writeServiceError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	switch {
	case auction.IsExpectedGuardRejection(err):
		// The auction ended in the instant between the Go and database
		// clock reads. Correct outcome, just caught one layer later.
		logger.InfoContext(r.Context(), "bid crossed end_at between clock reads; rejected by database guard", slog.Any("error", err))
	case errors.Is(err, auction.ErrRejectedByDatabaseGuard):
		// The client still gets the right answer, but the Go rules missed
		// something the database caught: a bug worth seeing in the logs.
		logger.WarnContext(r.Context(), "bid rejected by database guard, not by Go validation", slog.Any("error", err))
	}
	for _, m := range serviceErrors {
		if !errors.Is(err, m.err) {
			continue
		}
		var body errorBody
		body.Error.Code = m.code
		body.Error.Message = err.Error()
		var tooLow *auction.BidTooLowError
		if errors.As(err, &tooLow) {
			body.Error.Minimum = &tooLow.Minimum
		}
		writeJSON(w, logger, m.status, body)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// The request deadline (REQUEST_TIMEOUT) passed, or the client went
		// away. Usually the transaction was rolled back, but if the deadline
		// hit while COMMIT was in flight the bid may be durable: the outcome
		// is unknown. A retry with the same Idempotency-Key resolves it
		// either way (replay of the committed bid, or a fresh attempt).
		logger.WarnContext(r.Context(), "request did not finish in time", slog.Any("error", err))
		writeError(w, logger, http.StatusServiceUnavailable, "timeout", "the request timed out; it is safe to retry with the same Idempotency-Key")
		return
	}
	logger.ErrorContext(r.Context(), "unexpected error", slog.Any("error", err))
	writeError(w, logger, http.StatusInternalServerError, "internal", "internal error")
}
