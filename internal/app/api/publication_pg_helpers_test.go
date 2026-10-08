package api

import (
	"context"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/kernel/publicationservice"
)

func openAPIProductPublication(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	builder *workflowcatalog.CandidateBuilder,
) (*teamconstruction.PublicationAuthority, *teamconstruction.ProductPublication) {
	t.Helper()
	authority := teamconstruction.NewPublicationAuthority(pool, builder)
	kernel := openAPIKernelPublication(t, ctx, pool, authority)
	return authority, teamconstruction.NewProductPublication(pool, kernel, authority.AuthorizeProduct)
}

func openAPIKernelPublication(t *testing.T, ctx context.Context, pool *pgxpool.Pool, authority *teamconstruction.PublicationAuthority) *publicationservice.Service {
	t.Helper()
	connection, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	query := connection.Query()
	query.Set("search_path", pool.Config().ConnConfig.RuntimeParams["search_path"])
	connection.RawQuery = query.Encode()
	kernel, err := publicationservice.Open(ctx, connection.String(), authority)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kernel.Close)
	return kernel
}

func allowAPITestCandidateAssociation(product *teamconstruction.ProductPublication) {
	product.SetCandidateAssociation(func(context.Context, pgx.Tx, teamconstruction.CandidateRequestRecord) error { return nil })
}
