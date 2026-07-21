package gdl

import (
	"context"
	"errors"

	"github.com/monbooru/monloader/internal/queue"
)

// Probe runs a live per-site connectivity check: `gallery-dl -j --range 1-1`
// against the example/supplied URL with the configured credentials, mapping the
// result to ok / auth_required / failed. A zero exit alone is not enough:
// gallery-dl reports auth and extraction errors as a [-1, {error}] message while
// still exiting 0, so the output is parsed before reporting ok.
func (t *Tool) Probe(ctx context.Context, exampleURL string) (ProbeResult, error) {
	args := t.configArgs()
	args = append(args, "-j", "--range", "1-1", exampleURL)
	res, err := t.run(ctx, args...)
	if err != nil {
		return ProbeResult{Status: ProbeFailed, Detail: err.Error()}, nil
	}
	if res.exitCode != 0 {
		return probeFromError(classifyError(res.exitCode, res.stderr)), nil
	}
	if _, perr := parseResolve(res.stdout); perr != nil {
		var ge *queue.CodedError
		if errors.As(perr, &ge) {
			return probeFromError(ge), nil
		}
		return ProbeResult{Status: ProbeFailed, Detail: perr.Error()}, nil
	}
	return ProbeResult{Status: ProbeOK}, nil
}

// probeFromError maps a classified gallery-dl error to a probe status.
func probeFromError(e *queue.CodedError) ProbeResult {
	status := ProbeFailed
	switch e.Code {
	case queue.ErrCodeAuthRequired:
		status = ProbeAuthRequired
	case queue.ErrCodeBlocked:
		status = ProbeBlocked
	}
	return ProbeResult{Status: status, Detail: e.Msg}
}
