package daemon

import (
	"context"
	"github.com/jinyitao123/weave/internal/base/execution"
	"net/http"
	"strconv"
)

type taskProofKey struct{}
type taskProof struct {
	Subject execution.Subject
	Epoch   int64
}

func withTaskProof(ctx context.Context, subject execution.Subject, epoch int64) context.Context {
	return context.WithValue(ctx, taskProofKey{}, taskProof{subject, epoch})
}
func writeTaskProof(ctx context.Context, header http.Header) {
	if proof, ok := ctx.Value(taskProofKey{}).(taskProof); ok {
		header.Set("X-Weave-Task-Epoch", strconv.FormatInt(proof.Epoch, 10))
		header.Set("X-Weave-Task-Subject", proof.Subject.Digest())
	}
}
