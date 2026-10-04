# TradeDoc Check: common tasks. Run `make` (or `make help`) to list them.

TF := terraform -chdir=deploy/terraform

.DEFAULT_GOAL := help
.PHONY: help test run run-fake web samples lambda tf-init deploy set-secret destroy clean

help: ## List the targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-11s %s\n", $$1, $$2}'

test: ## go vet + all tests with the race detector
	go vet ./...
	go test -race ./...

run: ## Run locally (memory store, local uploads; Claude if ANTHROPIC_API_KEY is set, else model off)
	STORE=memory BLOB=local go run ./cmd/api

run-fake: ## Run locally with the fake model (fixtures in samples/expected, no API key needed)
	STORE=memory BLOB=local MODEL_PROVIDER=fake go run ./cmd/api

web: ## Build the React + Vite frontend (embedded into the Go binary)
	cd web && npm ci && npm run build

samples: ## Regenerate the synthetic sample PDFs
	python3 scripts/gen_samples.py

# Lambda's provided.al2023 runtime runs an executable named "bootstrap".
# CGO_ENABLED=0 gives a static binary; lambda.norpc drops the legacy RPC
# mode we don't use; -trimpath and -s -w make the binary smaller and keep
# local file paths out of it.
lambda: web ## Build dist/lambda.zip (linux/arm64, frontend embedded)
	mkdir -p dist
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -trimpath -ldflags "-s -w" -o dist/bootstrap ./cmd/api
	rm -f dist/lambda.zip
	zip -j dist/lambda.zip dist/bootstrap

tf-init: ## terraform init (first time only)
	$(TF) init

deploy: lambda ## Build, then create/update the AWS stack (terraform apply)
	$(TF) apply

# The key is read from your shell environment and piped to the AWS CLI on
# stdin (printf is a shell builtin, not a separate process), so the key never
# appears in this Makefile's output, in the process list or in Terraform state.
#
# --region comes from the secret's ARN (arn:aws:secretsmanager:<region>:...,
# the 4th ":"-separated field). The AWS CLI sends a request to its own
# configured region, not the one in an ARN, so without it a CLI set up for
# another region (or none) fails with ResourceNotFound or "You must specify
# a region". Terraform doesn't have this problem: its region is in versions.tf.
# cut -s prints nothing for a value with no ":" (plain cut would print the
# whole value), so a garbled ARN stops here instead of becoming the region.
set-secret: ## Copy ANTHROPIC_API_KEY from your shell into Secrets Manager (after first deploy; needs the AWS CLI v2)
	@command -v aws >/dev/null || { echo "The AWS CLI v2 is required: https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html" >&2; exit 1; }
	@test -n "$$ANTHROPIC_API_KEY" || { echo "ANTHROPIC_API_KEY is not set in this shell" >&2; exit 1; }
	@arn="$$($(TF) output -raw secret_arn)" || { echo "No secret ARN in Terraform state: run make deploy first" >&2; exit 1; }; \
	region="$$(echo "$$arn" | cut -s -d: -f4)"; \
	test -n "$$region" || { echo "Could not read a region from the secret ARN" >&2; exit 1; }; \
	printf '%s' "$$ANTHROPIC_API_KEY" | aws secretsmanager put-secret-value \
		--region "$$region" \
		--secret-id "$$arn" \
		--secret-string file:///dev/stdin > /dev/null
	@echo "Secret stored. Lambda reads it at cold start (a changed key is used once old instances are replaced)."
	@echo "Tip: give this key its own Anthropic workspace with a monthly spend limit. The app caps calls per day; only Anthropic can enforce a hard spending ceiling."

destroy: ## Delete the whole AWS stack, including uploaded files
	$(TF) destroy

clean: ## Remove build output
	rm -rf dist
