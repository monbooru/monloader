package gdl

import (
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/outcome"
)

// gallery-dl's exit codes shift across releases, so this keys on stderr text.
func classifyError(exitCode int, stderr string) *outcome.CodedError {
	low := strings.ToLower(stderr)
	switch {
	case containsAny(low, "no suitable extractor", "unsupported url", "no extractor found"):
		return &outcome.CodedError{Code: outcome.ErrCodeUnsupportedURL, Msg: stderr}
	// A bot wall answers 403 but is no missing credential, so it is matched
	// by vendor ahead of the auth rule. A plain 403 is not auth either.
	case containsAny(low, "cloudflare", "challengeerror", "ddos-guard", "captcha"):
		return &outcome.CodedError{Code: outcome.ErrCodeBlocked, Msg: stderr}
	case containsAny(low, "missing authentication", "authentication", "authorization", "authrequired", "login required", "http 401", "401 unauthorized"):
		return &outcome.CodedError{Code: outcome.ErrCodeAuthRequired, Msg: stderr}
	case containsAny(low, "http 429", "429 too many requests", "rate limit", "too many requests"):
		return &outcome.CodedError{Code: outcome.ErrCodeRateLimited, Msg: stderr}
	// The downloader's own network failing, not a bad link; a refused or
	// dropped connection stays download_failed.
	case containsAny(low, "nameresolutionerror", "temporary failure in name resolution", "name or service not known", "network is unreachable", "no route to host"):
		return &outcome.CodedError{Code: outcome.ErrCodeNetworkUnreachable, Msg: stderr}
	default:
		msg := stderr
		if msg == "" {
			msg = "gallery-dl exited with a non-zero status"
		}
		return &outcome.CodedError{Code: outcome.ErrCodeDownloadFailed, Msg: msg}
	}
}

func containsAny(s string, subs ...string) bool {
	return slices.ContainsFunc(subs, func(sub string) bool { return strings.Contains(s, sub) })
}
