package clone

import (
	"context"
	"errors"
	"fmt"

	"github.com/databricks/databricks-sdk-go/apierr"
	sdktime "github.com/databricks/databricks-sdk-go/common/types/time"
	"github.com/databricks/databricks-sdk-go/service/postgres"
)

// lakebaseSDK is the slice of the pinned SDK's workspace Postgres service
// the adapter calls; postgres.PostgresInterface satisfies it.
type lakebaseSDK interface {
	CreateBranch(context.Context, postgres.CreateBranchRequest) (
		postgres.CreateBranchOperationInterface, error)
	CreateEndpoint(context.Context, postgres.CreateEndpointRequest) (
		postgres.CreateEndpointOperationInterface, error)
	DeleteBranch(context.Context, postgres.DeleteBranchRequest) (
		postgres.DeleteBranchOperationInterface, error)
	DeleteEndpoint(context.Context, postgres.DeleteEndpointRequest) (
		postgres.DeleteEndpointOperationInterface, error)
	GenerateDatabaseCredential(context.Context, postgres.GenerateDatabaseCredentialRequest) (
		*postgres.DatabaseCredential, error)
}

// NewLakebaseSDKAPI adapts the Databricks SDK's Postgres service to
// LakebaseBranchAPI. Each create waits on its long-running operation until
// it completes or ctx ends.
func NewLakebaseSDKAPI(pg lakebaseSDK) LakebaseBranchAPI { return lakebaseSDKAPI{pg: pg} }

type lakebaseSDKAPI struct{ pg lakebaseSDK }

func (a lakebaseSDKAPI) CreateBranch(ctx context.Context,
	req LakebaseBranchRequest) (LakebaseBranch, error) {
	op, err := a.pg.CreateBranch(ctx, postgres.CreateBranchRequest{Parent: req.Parent,
		BranchId: req.BranchID, Branch: postgres.Branch{Spec: &postgres.BranchSpec{
			SourceBranch: req.SourceBranch, ExpireTime: sdktime.New(req.ExpireTime)}}})
	if err != nil {
		return LakebaseBranch{}, withContext(ctx, err)
	}
	b, err := op.Wait(ctx)
	if err != nil {
		return LakebaseBranch{}, withContext(ctx, fmt.Errorf("wait for the branch: %w", err))
	}
	if b == nil {
		return LakebaseBranch{}, errors.New("the create-branch operation returned no branch")
	}
	out := LakebaseBranch{Name: b.Name, UID: b.Uid}
	if b.Status != nil && b.Status.SourceBranchTime != nil {
		out.SourceTime = b.Status.SourceBranchTime.AsTime()
	} else if b.CreateTime != nil {
		out.SourceTime = b.CreateTime.AsTime()
	}
	return out, nil
}

func (a lakebaseSDKAPI) CreateEndpoint(ctx context.Context,
	req LakebaseEndpointRequest) (LakebaseEndpoint, error) {
	op, err := a.pg.CreateEndpoint(ctx, postgres.CreateEndpointRequest{Parent: req.Parent,
		EndpointId: req.EndpointID, Endpoint: postgres.Endpoint{Spec: &postgres.EndpointSpec{
			EndpointType:          postgres.EndpointTypeEndpointTypeReadWrite,
			AutoscalingLimitMaxCu: req.MaxCU}}})
	if err != nil {
		return LakebaseEndpoint{}, withContext(ctx, err)
	}
	e, err := op.Wait(ctx)
	if err != nil {
		return LakebaseEndpoint{}, withContext(ctx,
			fmt.Errorf("wait for the endpoint: %w", err))
	}
	if e == nil || e.Status == nil || e.Status.Hosts == nil || e.Status.Hosts.Host == "" {
		return LakebaseEndpoint{}, errors.New("the create-endpoint operation returned " +
			"no host")
	}
	return LakebaseEndpoint{Name: e.Name, UID: e.Uid, Host: e.Status.Hosts.Host}, nil
}

func (a lakebaseSDKAPI) DatabaseCredential(ctx context.Context, endpoint string) (string,
	error) {
	cred, err := a.pg.GenerateDatabaseCredential(ctx,
		postgres.GenerateDatabaseCredentialRequest{Endpoint: endpoint})
	if err != nil {
		return "", withContext(ctx, err)
	}
	if cred == nil {
		return "", errors.New("the credential call returned no credential")
	}
	return cred.Token, nil
}

func (a lakebaseSDKAPI) DeleteEndpoint(ctx context.Context, name string) error {
	op, err := a.pg.DeleteEndpoint(ctx, postgres.DeleteEndpointRequest{Name: name})
	if err == nil {
		err = op.Wait(ctx)
	}
	return deleteError(ctx, err)
}

func (a lakebaseSDKAPI) DeleteBranch(ctx context.Context, name string) error {
	op, err := a.pg.DeleteBranch(ctx, postgres.DeleteBranchRequest{Name: name})
	if err == nil {
		err = op.Wait(ctx)
	}
	return deleteError(ctx, err)
}

// deleteError maps the SDK's not-found to ErrLakebaseNotFound.
func deleteError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, apierr.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrLakebaseNotFound, err)
	}
	return withContext(ctx, err)
}

// withContext wraps ctx's error when ctx ended, so a wait the deadline cut
// short is recognisably a timeout whatever error the SDK returned.
func withContext(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
		return fmt.Errorf("%w (%w)", err, cerr)
	}
	return err
}
