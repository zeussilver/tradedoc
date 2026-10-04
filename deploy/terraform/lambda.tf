# The API and the web UI run as one Lambda function behind a Function URL.
# No API Gateway, no servers: nothing runs (or costs money) while idle.

locals {
  lambda_zip = "${path.module}/../../dist/lambda.zip" # built by `make lambda`
}

# Created by Terraform rather than by Lambda on first run, so it gets a
# retention period (Lambda's own default is "keep forever") and the role
# needs no permission to create log groups.
resource "aws_cloudwatch_log_group" "lambda" {
  name              = "/aws/lambda/${var.name}"
  retention_in_days = 14
}

resource "aws_lambda_function" "api" {
  function_name = var.name
  role          = aws_iam_role.lambda.arn

  # Go compiles to a native binary, so it runs on the OS-only runtime.
  # arm64 (Graviton) is cheaper per millisecond than x86_64.
  runtime       = "provided.al2023"
  architectures = ["arm64"]
  handler       = "bootstrap"

  # The hash makes Terraform redeploy when the code changes. fileexists()
  # lets `terraform validate` run before anything is built; the
  # precondition below stops a plan that has no zip to deploy.
  filename         = local.lambda_zip
  source_code_hash = fileexists(local.lambda_zip) ? filebase64sha256(local.lambda_zip) : null

  # Model calls take 30-90 s and the handler allows them 150 s.
  timeout     = 180
  memory_size = 512

  # null means "not reserved"; see the variable for why that's the default.
  reserved_concurrent_executions = var.reserved_concurrency

  logging_config {
    log_format = "JSON" # the app already logs JSON (log/slog), so fields stay searchable
    log_group  = aws_cloudwatch_log_group.lambda.name
  }

  # Configuration only. The API key itself is not here: environment
  # variables are visible to anyone who can read the function's settings,
  # so only the secret's ARN is passed and the key is fetched at cold start.
  environment {
    variables = {
      STORE                = "dynamodb"
      TABLE_NAME           = aws_dynamodb_table.main.name
      BLOB                 = "s3"
      BUCKET_NAME          = aws_s3_bucket.uploads.id
      UPLOAD_ORIGIN        = "https://${aws_s3_bucket.uploads.id}.s3.${var.region}.amazonaws.com" # presigned URL origin, for the page's CSP connect-src
      MODEL_PROVIDER       = "claude"
      ANTHROPIC_SECRET_ARN = aws_secretsmanager_secret.anthropic_api_key.arn
      CLAUDE_MODEL         = var.claude_model
      CLAUDE_EFFORT        = var.claude_effort
      MODEL_CALLS_PER_DAY  = tostring(var.model_calls_per_day)
    }
  }

  # The role's policy must exist before the first invocation, or the first
  # cold start can fail to read the secret.
  depends_on = [aws_iam_role_policy.lambda]

  lifecycle {
    precondition {
      condition     = fileexists(local.lambda_zip)
      error_message = "dist/lambda.zip not found: run `make lambda` (or `make deploy`) first."
    }
  }
}

# A public HTTPS endpoint for the function. Auth NONE because the app is a
# public demo with no login; abuse is limited in the app instead:
#   - the paid model routes take one unit from a per-IP daily quota and one
#     from a global daily quota (var.model_calls_per_day). Both counters live
#     in DynamoDB, so they hold across all Lambda instances;
#   - the in-memory per-IP token buckets are only a cheap first filter. Each
#     Lambda instance has its own, and concurrent requests start new
#     instances, so they do not limit a client across instances;
#   - shipment IDs are unguessable (80 random bits).
# RESPONSE_STREAM is the mode the Go adapter (lambdaurl) is written for.
# No CORS block: the UI is served from this same URL, so the browser never
# makes a cross-origin call to the API, and other sites can't read its responses.
resource "aws_lambda_function_url" "api" {
  function_name      = aws_lambda_function.api.function_name
  authorization_type = "NONE"
  invoke_mode        = "RESPONSE_STREAM"

  depends_on = [
    aws_lambda_permission.url_invoke_function_url,
    aws_lambda_permission.url_invoke_function,
  ]
}

# Since October 2025, a public Function URL needs a resource policy that
# allows BOTH lambda:InvokeFunctionUrl and lambda:InvokeFunction; with only
# one of them every request gets 403. The AWS provider also adds these two
# statements itself when it creates a NONE URL, but declaring them here
# (with the same statement IDs, created first) makes the public access
# visible in code and lets `terraform destroy` remove them.
resource "aws_lambda_permission" "url_invoke_function_url" {
  statement_id           = "FunctionURLAllowPublicAccess"
  action                 = "lambda:InvokeFunctionUrl"
  function_name          = aws_lambda_function.api.function_name
  principal              = "*"
  function_url_auth_type = "NONE" # only valid while the URL's auth type is NONE
}

resource "aws_lambda_permission" "url_invoke_function" {
  statement_id  = "FunctionURLAllowInvokeAction"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.api.function_name
  principal     = "*"
  # Only through the Function URL: this does not let anyone call the
  # function directly with the Lambda Invoke API.
  invoked_via_function_url = true
}
