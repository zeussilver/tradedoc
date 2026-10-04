// Package secrets reads secrets (the Anthropic API key) from AWS Secrets
// Manager. Keeping the key there, rather than in a Lambda environment
// variable or Terraform, means it never appears in the Lambda console,
// in Terraform state or in the repository.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// Get returns the current string value of a secret, with surrounding
// whitespace trimmed (a pasted key often ends with a newline).
// Errors name the secret ID (an ARN, not sensitive) but never the value;
// callers must not log the value either.
func Get(ctx context.Context, client *secretsmanager.Client, secretID string) (string, error) {
	out, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(secretID)})
	if err != nil {
		return "", fmt.Errorf("read secret %s: %w", secretID, err)
	}
	value := strings.TrimSpace(aws.ToString(out.SecretString))
	if value == "" {
		return "", errors.New("secret " + secretID + " is empty: set it with `make set-secret`")
	}
	return value, nil
}
