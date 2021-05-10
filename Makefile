# ====================================================================================
# Setup Project

PROJECT_NAME := nats-proxy
PROJECT_REPO := github.com/upbound/$(PROJECT_NAME)

-include build/makelib/common.mk

# ====================================================================================
# Setup Go
GO_LDFLAGS += -X $(GO_PROJECT)/pkg/version.Version=$(VERSION)
GO_SUBDIRS += pkg test
GO111MODULE = on
-include build/makelib/golang.mk


# ====================================================================================
# Targets

fallthrough: submodules
	@echo Initial setup complete. Running make again . . .
	@make

# Generate a coverage report for cobertura applying exclusions on
# - generated file
cobertura:
	@cat $(GO_TEST_OUTPUT)/coverage.txt | \
		grep -v zz_generated.deepcopy | \
		$(GOCOVER_COBERTURA) > $(GO_TEST_OUTPUT)/cobertura-coverage.xml

# Ensure a PR is ready for review.
reviewable: lint
	@go mod tidy

# Ensure branch is clean.
check-diff: reviewable
	@$(INFO) checking that branch is clean
	@git diff --quiet || $(FAIL)
	@$(OK) branch is clean

# Update the submodules, such as the common build scripts.
submodules:
	@git submodule sync
	@git submodule update --init --recursive

.PHONY: cobertura reviewable submodules fallthrough
