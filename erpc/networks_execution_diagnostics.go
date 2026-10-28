package erpc

import (
	"context"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/execdiag"
)

// Forward wraps the network forward with an execution diagnostics scope, so
// every upstream invocation of the request lands in one ledger and notable
// requests emit their evidence on completion.
func (n *Network) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	ctx, d := execdiag.BeginRequest(ctx, req, n.projectId, n.networkId)
	resp, err := n.forward(ctx, req)
	d.Finish(ctx, resp, err)
	return resp, err
}
