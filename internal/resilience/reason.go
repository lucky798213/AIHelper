package resilience

import (
	"context"
	"errors"
	"strings"
)

type FailoverReason string

const (
	ReasonRateLimit FailoverReason = "rate_limit"
	ReasonAuth      FailoverReason = "auth"
	ReasonTimeout   FailoverReason = "timeout"
	ReasonBilling   FailoverReason = "billing"
	ReasonOverflow  FailoverReason = "overflow"
	ReasonUnknown   FailoverReason = "unknown"
)

type httpStatusError interface {
	error
	HTTPStatusCode() int
}

func ClassifyFailure(err error) FailoverReason {
	if err == nil {
		return ReasonUnknown
	}

	var statusErr httpStatusError
	if errors.As(err, &statusErr) {
		switch statusErr.HTTPStatusCode() {
		case 401, 403:
			return ReasonAuth
		case 402:
			return ReasonBilling
		case 408, 504:
			return ReasonTimeout
		case 429:
			return ReasonRateLimit
		}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return ReasonTimeout
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "context length"),
		strings.Contains(msg, "context_length_exceeded"),
		strings.Contains(msg, "context window"),
		strings.Contains(msg, "maximum context"),
		strings.Contains(msg, "too many tokens"),
		strings.Contains(msg, "token overflow"),
		strings.Contains(msg, "overflow"):
		return ReasonOverflow
	case strings.Contains(msg, "rate limit"),
		strings.Contains(msg, "too many requests"),
		strings.Contains(msg, "429"):
		return ReasonRateLimit
	case strings.Contains(msg, "auth"),
		strings.Contains(msg, "unauthorized"),
		strings.Contains(msg, "forbidden"),
		strings.Contains(msg, "api key"),
		strings.Contains(msg, "invalid key"),
		strings.Contains(msg, "401"),
		strings.Contains(msg, "403"):
		return ReasonAuth
	case strings.Contains(msg, "billing"),
		strings.Contains(msg, "quota"),
		strings.Contains(msg, "insufficient_quota"),
		strings.Contains(msg, "402"):
		return ReasonBilling
	case strings.Contains(msg, "timeout"),
		strings.Contains(msg, "timed out"),
		strings.Contains(msg, "deadline"):
		return ReasonTimeout
	default:
		return ReasonUnknown
	}
}

func cooldownForReason(reason FailoverReason) int {
	switch reason {
	case ReasonAuth, ReasonBilling:
		return 300
	case ReasonRateLimit, ReasonUnknown:
		return 120
	case ReasonTimeout:
		return 60
	case ReasonOverflow:
		return 600
	default:
		return 120
	}
}
