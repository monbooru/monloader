package gdl

import (
	"context"
	"errors"

	"github.com/monbooru/monloader/internal/outcome"
)

// A zero exit is not enough: gallery-dl reports auth and extraction
// errors in the output and still exits 0.
func (t *Tool) Probe(ctx context.Context, exampleURL string) (ProbeResult, error) {
	res, err := t.probe(ctx, exampleURL)
	res.Detail = t.redactText(res.Detail)
	return res, err
}

func (t *Tool) probe(ctx context.Context, exampleURL string) (ProbeResult, error) {
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
		var ge *outcome.CodedError
		if errors.As(perr, &ge) {
			return probeFromError(ge), nil
		}
		return ProbeResult{Status: ProbeFailed, Detail: perr.Error()}, nil
	}
	return ProbeResult{Status: ProbeOK}, nil
}

func probeFromError(e *outcome.CodedError) ProbeResult {
	status := ProbeFailed
	switch e.Code {
	case outcome.ErrCodeAuthRequired:
		status = ProbeAuthRequired
	case outcome.ErrCodeBlocked:
		status = ProbeBlocked
	}
	return ProbeResult{Status: status, Detail: e.Msg}
}
