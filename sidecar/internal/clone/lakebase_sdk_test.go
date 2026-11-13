package clone

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/databricks/databricks-sdk-go/apierr"
	sdktime "github.com/databricks/databricks-sdk-go/common/types/time"
	"github.com/databricks/databricks-sdk-go/experimental/api"
	"github.com/databricks/databricks-sdk-go/service/postgres"
)

// market-03: the SDK adapter maps the narrow branch API onto the pinned
// Databricks SDK's long-running operations.

type fakeSDK struct {
	branchReq   postgres.CreateBranchRequest
	endpointReq postgres.CreateEndpointRequest
	credReq     postgres.GenerateDatabaseCredentialRequest
	deleted     []string

	branch      *postgres.Branch
	branchErr   error // from the call
	branchWait  error // from Wait
	endpoint    *postgres.Endpoint
	endpointErr error
	cred        *postgres.DatabaseCredential
	credErr     error
	deleteErr   error
	deleteWait  error
}

type op[T any] struct {
	result *T
	err    error
}

func (o op[T]) Wait(context.Context, ...api.Option) (*T, error) { return o.result, o.err }
func (o op[T]) Name() string                                    { return "operations/fake" }
func (o op[T]) Done() (bool, error)                             { return true, nil }

type branchOp struct{ op[postgres.Branch] }

func (branchOp) Metadata() (*postgres.BranchOperationMetadata, error) { return nil, nil }

type endpointOp struct{ op[postgres.Endpoint] }

func (endpointOp) Metadata() (*postgres.EndpointOperationMetadata, error) { return nil, nil }

type deleteOp struct{ err error }

func (d deleteOp) Wait(context.Context, ...api.Option) error { return d.err }
func (deleteOp) Name() string                                { return "operations/delete" }
func (deleteOp) Done() (bool, error)                         { return true, nil }

type deleteBranchOp struct{ deleteOp }

func (deleteBranchOp) Metadata() (*postgres.BranchOperationMetadata, error) { return nil, nil }

type deleteEndpointOp struct{ deleteOp }

func (deleteEndpointOp) Metadata() (*postgres.EndpointOperationMetadata, error) {
	return nil, nil
}

func (f *fakeSDK) CreateBranch(_ context.Context,
	req postgres.CreateBranchRequest) (postgres.CreateBranchOperationInterface, error) {
	f.branchReq = req
	if f.branchErr != nil {
		return nil, f.branchErr
	}
	return branchOp{op[postgres.Branch]{f.branch, f.branchWait}}, nil
}

func (f *fakeSDK) CreateEndpoint(_ context.Context,
	req postgres.CreateEndpointRequest) (postgres.CreateEndpointOperationInterface, error) {
	f.endpointReq = req
	if f.endpointErr != nil {
		return nil, f.endpointErr
	}
	return endpointOp{op[postgres.Endpoint]{f.endpoint, nil}}, nil
}

func (f *fakeSDK) DeleteBranch(_ context.Context,
	req postgres.DeleteBranchRequest) (postgres.DeleteBranchOperationInterface, error) {
	f.deleted = append(f.deleted, req.Name)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return deleteBranchOp{deleteOp{f.deleteWait}}, nil
}

func (f *fakeSDK) DeleteEndpoint(_ context.Context,
	req postgres.DeleteEndpointRequest) (postgres.DeleteEndpointOperationInterface, error) {
	f.deleted = append(f.deleted, req.Name)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return deleteEndpointOp{deleteOp{f.deleteWait}}, nil
}

func (f *fakeSDK) GenerateDatabaseCredential(_ context.Context,
	req postgres.GenerateDatabaseCredentialRequest) (*postgres.DatabaseCredential, error) {
	f.credReq = req
	return f.cred, f.credErr
}

func TestLakebaseSDK_CreateBranchMapsTheRequest(t *testing.T) {
	expire := testNow().Add(time.Hour)
	source := testNow().Add(-time.Second)
	sdk := &fakeSDK{branch: &postgres.Branch{Name: "projects/shop/branches/sage-rh-0a",
		Uid: "br-1", Status: &postgres.BranchStatus{SourceBranchTime: sdktime.New(source)}}}
	got, err := NewLakebaseSDKAPI(sdk).CreateBranch(context.Background(),
		LakebaseBranchRequest{Parent: "projects/shop", BranchID: "sage-rh-0a",
			SourceBranch: "projects/shop/branches/production", ExpireTime: expire})
	if err != nil {
		t.Fatal(err)
	}
	req := sdk.branchReq
	if req.Parent != "projects/shop" || req.BranchId != "sage-rh-0a" ||
		req.Branch.Spec == nil || req.Branch.Spec.SourceBranch !=
		"projects/shop/branches/production" || req.Branch.Spec.ExpireTime == nil ||
		!req.Branch.Spec.ExpireTime.AsTime().Equal(expire) || req.ReplaceExisting ||
		req.Branch.Spec.NoExpiry || req.Branch.Spec.IsProtected {
		t.Fatalf("branch request = %+v spec %+v", req, req.Branch.Spec)
	}
	if got.Name != "projects/shop/branches/sage-rh-0a" || got.UID != "br-1" ||
		!got.SourceTime.Equal(source) {
		t.Fatalf("branch = %+v", got)
	}
}

func TestLakebaseSDK_CreateBranchErrors(t *testing.T) {
	cause := errors.New("invalid source")
	for name, sdk := range map[string]*fakeSDK{
		"call":      {branchErr: cause},
		"wait":      {branchWait: cause},
		"no branch": {},
	} {
		_, err := NewLakebaseSDKAPI(sdk).CreateBranch(context.Background(),
			LakebaseBranchRequest{Parent: "projects/shop", BranchID: "b"})
		if err == nil || (sdk.branch == nil && sdk.branchErr == nil &&
			sdk.branchWait == nil && !strings.Contains(err.Error(), "no branch")) {
			t.Errorf("%s: %v", name, err)
		}
		if (sdk.branchErr != nil || sdk.branchWait != nil) && !errors.Is(err, cause) {
			t.Errorf("%s: %v does not wrap the cause", name, err)
		}
	}
}

// A wait the caller's deadline cut short is reported as that deadline, so
// the receipt becomes create_uncertain.
func TestLakebaseSDK_WaitCutShortWrapsTheContextError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	sdk := &fakeSDK{branchWait: errors.New("timed out: operation still in progress")}
	_, err := NewLakebaseSDKAPI(sdk).CreateBranch(ctx, LakebaseBranchRequest{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cut-short wait = %v, want DeadlineExceeded", err)
	}
}

func TestLakebaseSDK_CreateEndpointMapsTheRequest(t *testing.T) {
	sdk := &fakeSDK{endpoint: &postgres.Endpoint{Name: "projects/shop/branches/b/endpoints/e",
		Uid: "ep-1", Status: &postgres.EndpointStatus{
			Hosts: &postgres.EndpointHosts{Host: "ep-1.lakebase.invalid"}}}}
	got, err := NewLakebaseSDKAPI(sdk).CreateEndpoint(context.Background(),
		LakebaseEndpointRequest{Parent: "projects/shop/branches/b", EndpointID: "e",
			MaxCU: 4})
	if err != nil {
		t.Fatal(err)
	}
	req := sdk.endpointReq
	if req.Parent != "projects/shop/branches/b" || req.EndpointId != "e" ||
		req.Endpoint.Spec == nil ||
		req.Endpoint.Spec.EndpointType != postgres.EndpointTypeEndpointTypeReadWrite ||
		req.Endpoint.Spec.AutoscalingLimitMaxCu != 4 {
		t.Fatalf("endpoint request = %+v spec %+v", req, req.Endpoint.Spec)
	}
	if got.Name != "projects/shop/branches/b/endpoints/e" || got.UID != "ep-1" ||
		got.Host != "ep-1.lakebase.invalid" {
		t.Fatalf("endpoint = %+v", got)
	}
	sdk.endpoint.Status = nil
	if _, err := NewLakebaseSDKAPI(sdk).CreateEndpoint(context.Background(),
		LakebaseEndpointRequest{}); err == nil || !strings.Contains(err.Error(), "no host") {
		t.Fatalf("endpoint without hosts = %v", err)
	}
}

func TestLakebaseSDK_Credential(t *testing.T) {
	sdk := &fakeSDK{cred: &postgres.DatabaseCredential{Token: testLakebaseToken}}
	token, err := NewLakebaseSDKAPI(sdk).DatabaseCredential(context.Background(), "ep")
	if err != nil || token != testLakebaseToken || sdk.credReq.Endpoint != "ep" {
		t.Fatalf("credential set = %v, %v (endpoint %q)", token != "", err, sdk.credReq.Endpoint)
	}
	sdk = &fakeSDK{credErr: errors.New("denied")}
	if _, err := NewLakebaseSDKAPI(sdk).DatabaseCredential(context.Background(),
		"ep"); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("credential error = %v", err)
	}
	if _, err := NewLakebaseSDKAPI(&fakeSDK{}).DatabaseCredential(context.Background(),
		"ep"); err == nil {
		t.Fatal("a nil credential was accepted")
	}
}

func TestLakebaseSDK_DeleteMapsNotFound(t *testing.T) {
	gone := fmt.Errorf("RESOURCE_DOES_NOT_EXIST: %w", apierr.ErrNotFound)
	for name, sdk := range map[string]*fakeSDK{
		"call": {deleteErr: gone}, "wait": {deleteWait: gone},
	} {
		a := NewLakebaseSDKAPI(sdk)
		if err := a.DeleteEndpoint(context.Background(), "e"); !errors.Is(err,
			ErrLakebaseNotFound) {
			t.Errorf("%s: endpoint delete = %v", name, err)
		}
		if err := a.DeleteBranch(context.Background(), "b"); !errors.Is(err,
			ErrLakebaseNotFound) {
			t.Errorf("%s: branch delete = %v", name, err)
		}
	}
	cause := errors.New("throttled")
	sdk := &fakeSDK{deleteWait: cause}
	if err := NewLakebaseSDKAPI(sdk).DeleteBranch(context.Background(),
		"projects/shop/branches/b"); !errors.Is(err, cause) || errors.Is(err,
		ErrLakebaseNotFound) {
		t.Fatalf("other delete error = %v", err)
	}
	ok := &fakeSDK{}
	if err := NewLakebaseSDKAPI(ok).DeleteBranch(context.Background(),
		"projects/shop/branches/b"); err != nil || ok.deleted[0] != "projects/shop/branches/b" {
		t.Fatalf("delete = %v, deleted %q", err, ok.deleted)
	}
}
