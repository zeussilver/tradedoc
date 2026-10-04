variable "region" {
  description = "AWS region to deploy into."
  type        = string
  default     = "ap-southeast-2" # Sydney, closest to Melbourne
}

variable "name" {
  description = "Name used for the Lambda function, table, log group and as a prefix elsewhere."
  type        = string
  default     = "tradedoc"

  validation {
    # It is part of the S3 bucket name, which must be lowercase and at most 63 characters.
    condition     = can(regex("^[a-z][a-z0-9-]{1,23}$", var.name))
    error_message = "name must be 2-24 lowercase letters, digits and hyphens, starting with a letter."
  }
}

variable "claude_model" {
  description = "Claude model ID used for extraction and candidate codes."
  type        = string
  default     = "claude-opus-5-5"
}

variable "claude_effort" {
  description = "Claude effort level (low, medium, high)."
  type        = string
  default     = "medium"
}

variable "model_calls_per_day" {
  description = "Model operations (extract or suggest) per day across all instances; each makes up to 2 Claude requests."
  type        = number
  # Counted in DynamoDB, so every Lambda instance shares it. 25 is enough
  # for several full demo walk-throughs while keeping a worst-case day's
  # bill small. The hard ceiling on cost is a spend limit on the Anthropic
  # workspace that owns the key (set in the Anthropic Console, not here).
  default = 25

  validation {
    condition     = var.model_calls_per_day >= 1 && floor(var.model_calls_per_day) == var.model_calls_per_day
    error_message = "model_calls_per_day must be a whole number of at least 1 (the app refuses to start otherwise)."
  }
}

variable "reserved_concurrency" {
  description = "Maximum concurrent Lambda instances, or null for no reservation."
  type        = number
  # null = don't reserve. Reserving concurrency is a good brake on cost and
  # abuse, but new AWS accounts often have an account limit of only 10
  # concurrent executions, and Lambda must keep 10 unreserved, so such an
  # account cannot reserve any. Set e.g. 5 once the account limit is raised.
  default  = null
  nullable = true
}
