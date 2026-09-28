package azure

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// DefaultTokenSource authenticates with azidentity's default chain:
// service-principal environment variables (AZURE_TENANT_ID,
// AZURE_CLIENT_ID, AZURE_CLIENT_SECRET), workload identity, managed
// identity, then the Azure CLI login. Tokens are cached and refreshed by
// the credential.
func DefaultTokenSource() (TokenSource, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure credential: %w", err)
	}
	return func(ctx context.Context) (string, error) {
		token, err := credential.GetToken(ctx, policy.TokenRequestOptions{
			Scopes: []string{ManagementScope},
		})
		if err != nil {
			return "", err
		}
		return token.Token, nil
	}, nil
}
