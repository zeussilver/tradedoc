output "function_url" {
  description = "Open this in a browser: it serves the web UI and the API."
  value       = aws_lambda_function_url.api.function_url
}

output "bucket_name" {
  description = "S3 bucket holding uploaded documents (under uploads/)."
  value       = aws_s3_bucket.uploads.id
}

output "table_name" {
  description = "DynamoDB table holding shipments, extractions, checks and the audit log."
  value       = aws_dynamodb_table.main.name
}

output "secret_arn" {
  description = "Secrets Manager secret for the Anthropic API key (ARN only, never the value)."
  value       = aws_secretsmanager_secret.anthropic_api_key.arn
}

output "next_step" {
  description = "What to do after the first apply."
  value       = "Store the API key: export ANTHROPIC_API_KEY in your shell, then run `make set-secret` (needs the AWS CLI v2). Until then every request, including the web UI, gets a 502: the function stops at cold start when it cannot read the key."
}
