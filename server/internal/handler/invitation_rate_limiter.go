package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/redis/go-redis/v9"
)

const (
	invitationActorLimiterKeyPrefix     = "mul:invitation:actor:"
	invitationWorkspaceLimiterKeyPrefix = "mul:invitation:workspace:"
	invitationRecipientLimiterKeyPrefix = "mul:invitation:recipient:"
)

// InvitationRateLimits configures the independent invitation admission gates.
type InvitationRateLimits struct {
	Actor     SlidingWindowRateLimit
	Workspace SlidingWindowRateLimit
	Recipient SlidingWindowRateLimit
}

// DefaultInvitationRateLimits returns conservative email-abuse budgets.
func DefaultInvitationRateLimits() InvitationRateLimits {
	return InvitationRateLimits{
		Actor:     SlidingWindowRateLimit{Limit: 10, Window: 10 * time.Minute},
		Workspace: SlidingWindowRateLimit{Limit: 50, Window: 24 * time.Hour},
		Recipient: SlidingWindowRateLimit{Limit: 3, Window: 24 * time.Hour},
	}
}

// InvitationRateLimiters holds the independently keyed gate implementations.
type InvitationRateLimiters struct {
	Actor     SlidingWindowRateLimiter
	Workspace SlidingWindowRateLimiter
	Recipient SlidingWindowRateLimiter
}

// NewMemoryInvitationRateLimiters builds single-process gates for local and
// Redis-less deployments.
func NewMemoryInvitationRateLimiters(limits InvitationRateLimits) InvitationRateLimiters {
	return InvitationRateLimiters{
		Actor:     newMemorySlidingWindowRateLimiter(limits.Actor),
		Workspace: newMemorySlidingWindowRateLimiter(limits.Workspace),
		Recipient: newMemorySlidingWindowRateLimiter(limits.Recipient),
	}
}

// NewRedisInvitationRateLimiters builds replica-shared gates with isolated key
// namespaces for each dimension.
func NewRedisInvitationRateLimiters(rdb *redis.Client, limits InvitationRateLimits) InvitationRateLimiters {
	return InvitationRateLimiters{
		Actor:     newRedisSlidingWindowRateLimiter(rdb, limits.Actor, invitationActorLimiterKeyPrefix),
		Workspace: newRedisSlidingWindowRateLimiter(rdb, limits.Workspace, invitationWorkspaceLimiterKeyPrefix),
		Recipient: newRedisSlidingWindowRateLimiter(rdb, limits.Recipient, invitationRecipientLimiterKeyPrefix),
	}
}

type invitationRateLimitGate struct {
	name    string
	key     string
	limiter SlidingWindowRateLimiter
}

func invitationRecipientKey(email string) string {
	normalized := strings.ToLower(strings.TrimSpace(email))
	digest := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(digest[:])
}

func (h *Handler) admitInvitation(w http.ResponseWriter, r *http.Request, actorID, workspaceID, email string) bool {
	gates := []invitationRateLimitGate{
		{name: "actor", key: actorID, limiter: h.InvitationRateLimiters.Actor},
		{name: "workspace", key: workspaceID, limiter: h.InvitationRateLimiters.Workspace},
		{name: "recipient", key: invitationRecipientKey(email), limiter: h.InvitationRateLimiters.Recipient},
	}

	var backendErr error
	var backendErrorGates []string
	var retryAfter time.Duration
	for _, gate := range gates {
		allowed, err := slidingWindowLimiterAllow(r.Context(), gate.limiter, gate.key)
		if err != nil {
			if backendErr == nil {
				backendErr = err
			}
			backendErrorGates = append(backendErrorGates, gate.name)
			continue
		}
		if allowed {
			continue
		}

		h.Metrics.RecordEmailRateLimited("workspace_invitation", gate.name)
		if retry := webhookLimiterRetryAfter(r.Context(), gate.limiter, gate.key); retry > retryAfter {
			retryAfter = retry
		}
	}

	if backendErr != nil {
		slog.Error("invitation rate limiter unavailable", append(logger.RequestAttrs(r), "gates", strings.Join(backendErrorGates, ","), "error", backendErr)...)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "5")
		writeErrorCode(w, http.StatusServiceUnavailable, "invitation_rate_limiter_unavailable", "invitation service temporarily unavailable")
		return false
	}
	if retryAfter <= 0 {
		return true
	}

	retryAfterSeconds := int64((retryAfter + time.Second - 1) / time.Second)
	if retryAfterSeconds < 1 {
		retryAfterSeconds = 1
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds, 10))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":               "too many invitation requests",
		"code":                "invitation_rate_limited",
		"retry_after_seconds": retryAfterSeconds,
	})
	return false
}
