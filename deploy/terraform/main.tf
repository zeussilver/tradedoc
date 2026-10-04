# Storage: the uploads bucket, the DynamoDB table and the API key secret.
# The Lambda function and its permissions are in lambda.tf and iam.tf.

data "aws_caller_identity" "current" {}

locals {
  # Bucket names are global across all AWS accounts, so the account ID and
  # region make ours unique without a random suffix.
  bucket_name = "${var.name}-uploads-${data.aws_caller_identity.current.account_id}-${var.region}"

  # Files live under one prefix, so IAM and the lifecycle rule can target it.
  upload_prefix = "uploads/"
}

# ---- S3: uploaded documents ----

resource "aws_s3_bucket" "uploads" {
  bucket = local.bucket_name
  # Demo convenience: `terraform destroy` also deletes uploaded files.
  # A production bucket would keep this false so data can't be lost by accident.
  force_destroy = true
}

# Nothing in this bucket may ever be public: browsers only reach it through
# short-lived presigned URLs, which work with all four settings on.
resource "aws_s3_bucket_public_access_block" "uploads" {
  bucket                  = aws_s3_bucket.uploads.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# ACLs off: access is decided only by IAM and the bucket policy, so an
# uploader can't make an object public by sending an ACL header.
resource "aws_s3_bucket_ownership_controls" "uploads" {
  bucket = aws_s3_bucket.uploads.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

# Encrypt every object at rest with S3-managed keys (SSE-S3). No KMS key to
# manage or pay for, and presigned uploads need no extra headers.
resource "aws_s3_bucket_server_side_encryption_configuration" "uploads" {
  bucket = aws_s3_bucket.uploads.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# Refuse any request that isn't over HTTPS, even a correctly signed one.
data "aws_iam_policy_document" "uploads_tls_only" {
  statement {
    sid     = "DenyInsecureTransport"
    effect  = "Deny"
    actions = ["s3:*"]
    resources = [
      aws_s3_bucket.uploads.arn,
      "${aws_s3_bucket.uploads.arn}/*",
    ]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "uploads" {
  bucket = aws_s3_bucket.uploads.id
  policy = data.aws_iam_policy_document.uploads_tls_only.json
  # Apply after Block Public Access, so there is never a moment where a
  # policy exists without it.
  depends_on = [aws_s3_bucket_public_access_block.uploads]
}

# The browser PUTs files straight to S3, which is a cross-origin request
# from the app's page. Allow exactly that: PUT, from the app's own origin,
# with only the headers a presigned upload sends.
resource "aws_s3_bucket_cors_configuration" "uploads" {
  bucket = aws_s3_bucket.uploads.id
  cors_rule {
    allowed_methods = ["PUT"]
    # The Function URL ends with "/", a browser's Origin header doesn't.
    allowed_origins = [trimsuffix(aws_lambda_function_url.api.function_url, "/")]
    allowed_headers = ["content-type", "x-amz-*"]
    max_age_seconds = 600
  }
}

# Trade documents are only needed while a shipment is being checked, so
# they are deleted automatically after 30 days (data minimisation).
resource "aws_s3_bucket_lifecycle_configuration" "uploads" {
  bucket = aws_s3_bucket.uploads.id
  rule {
    id     = "expire-uploads"
    status = "Enabled"
    filter {
      prefix = local.upload_prefix
    }
    expiration {
      days = 30
    }
  }
}

# ---- DynamoDB: shipments, documents, extractions, checks, audit, quotas ----

resource "aws_dynamodb_table" "main" {
  name = var.name
  # On-demand: pay per request, nothing while idle, no capacity to tune.
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  range_key    = "sk"

  attribute {
    name = "pk"
    type = "S"
  }
  attribute {
    name = "sk"
    type = "S"
  }

  # Items that carry an expires_at (Unix seconds) are deleted by DynamoDB
  # some time after that moment, at no cost. Items without it are kept.
  ttl {
    attribute_name = "expires_at"
    enabled        = true
  }
  # Encryption at rest is always on in DynamoDB (AWS-owned key by default).
}

# ---- Secrets Manager: the Anthropic API key ----

# Only the secret's "container" is created here. There is deliberately no
# aws_secretsmanager_secret_version: its value would be stored in plain text
# in the Terraform state file. The value is set with `make set-secret`.
resource "aws_secretsmanager_secret" "anthropic_api_key" {
  name        = "${var.name}/anthropic-api-key"
  description = "Anthropic API key for TradeDoc Check. Value set with make set-secret."
  # Demo convenience: delete at once on destroy, so the same name can be
  # reused by the next deploy. Production would keep the recovery window.
  recovery_window_in_days = 0
}
