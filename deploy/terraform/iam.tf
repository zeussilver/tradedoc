# The Lambda function's role. Least privilege: every statement names exact
# actions on exact resource ARNs, and nothing uses a "*" resource. If the
# code is ever compromised, it can only touch this app's own log group,
# uploads, table and secret.

data "aws_iam_policy_document" "lambda_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "lambda" {
  name               = "${var.name}-lambda"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
}

data "aws_iam_policy_document" "lambda" {
  # Write logs to our own log group only. No logs:CreateLogGroup: Terraform
  # creates the group (with a retention period) before the function runs.
  statement {
    sid       = "WriteOwnLogs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.lambda.arn}:*"]
  }

  # Read, write and delete uploaded files, under uploads/ only.
  # PutObject is also what makes browser uploads work: a presigned URL acts
  # with the permissions of whoever signed it, and the URLs are signed with
  # this role's credentials. Without it every browser upload gets 403.
  statement {
    sid       = "UploadsObjects"
    actions   = ["s3:PutObject", "s3:GetObject", "s3:DeleteObject"]
    resources = ["${aws_s3_bucket.uploads.arn}/${local.upload_prefix}*"]
  }

  # Without ListBucket, S3 answers 403 (not 404) for a file that doesn't
  # exist, so the API couldn't tell "never uploaded" from "access denied".
  # The bucket holds nothing but these uploads, which the role can already read.
  statement {
    sid       = "UploadsList"
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.uploads.arn]
  }

  # Exactly the item-level operations the store uses. There is no separate
  # permission for TransactWriteItems: IAM checks each step of a transaction
  # as the matching single-item action. The store's transactions only hold
  # conditional Put and Update steps (e.g. a review plus its "line already
  # accepted" marker), so PutItem and UpdateItem cover them. A ConditionCheck
  # step would need dynamodb:ConditionCheckItem, a Delete step DeleteItem;
  # neither is used. No Scan, no DeleteItem, no table administration.
  statement {
    sid = "Table"
    actions = [
      "dynamodb:GetItem",
      "dynamodb:PutItem",
      "dynamodb:UpdateItem",
      "dynamodb:Query",
    ]
    resources = [aws_dynamodb_table.main.arn]
  }

  # Read the API key, and only this one secret.
  statement {
    sid       = "ReadApiKey"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.anthropic_api_key.arn]
  }
}

resource "aws_iam_role_policy" "lambda" {
  name   = "${var.name}-lambda"
  role   = aws_iam_role.lambda.id
  policy = data.aws_iam_policy_document.lambda.json
}
