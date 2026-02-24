# Run from the repo root: make -C docker [target]
# Or from within this directory: make [target]
# Build context is always the repository root.

IMAGE_NAME := sticky-recorder
REPO_ROOT  := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/..)
VERSION    ?= $(shell cat $(REPO_ROOT)/VERSION)
DIST_DIR   := dist

# Pin sticky-overseer to a specific tag/branch/commit (default: latest main)
STICKY_OVERSEER_REF  ?=
STICKY_OVERSEER_REPO ?=
_OVERSEER_REF_ARG    = $(if $(STICKY_OVERSEER_REF),--build-arg STICKY_OVERSEER_REF=$(STICKY_OVERSEER_REF),)
_OVERSEER_REPO_ARG   = $(if $(STICKY_OVERSEER_REPO),--build-arg STICKY_OVERSEER_REPO=$(STICKY_OVERSEER_REPO),)

.PHONY: build export load clean help version

help: ## Show this help message
	@echo "Usage: make -C docker [target] [VERSION=x.y.z] [STICKY_OVERSEER_REF=vX.Y.Z]"
	@echo ""
	@echo "Current version : $(VERSION)"
	@echo "Image tag       : $(IMAGE_NAME):$(VERSION)"
	@echo ""
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-10s %s\n", $$1, $$2}'

version: ## Show current version
	@echo $(VERSION)

build: ## Build the sticky-recorder image (bundles sticky-overseer)
	docker build \
		-f $(CURDIR)/Dockerfile \
		-t $(IMAGE_NAME):$(VERSION) \
		-t $(IMAGE_NAME):latest \
		$(_OVERSEER_REF_ARG) \
		$(_OVERSEER_REPO_ARG) \
		$(REPO_ROOT)
	@echo "Build complete: $(IMAGE_NAME):$(VERSION)"

export: build ## Build and export image to docker/dist/
	@mkdir -p $(DIST_DIR)
	docker save $(IMAGE_NAME):$(VERSION) $(IMAGE_NAME):latest | gzip > $(DIST_DIR)/$(IMAGE_NAME)-$(VERSION).tar.gz
	@echo "Exported $(DIST_DIR)/$(IMAGE_NAME)-$(VERSION).tar.gz  ($$(du -h $(DIST_DIR)/$(IMAGE_NAME)-$(VERSION).tar.gz | cut -f1))"

load: ## Load a previously exported tar. Usage: make load FILE=dist/sticky-recorder-*.tar
	@if [ -z "$(FILE)" ]; then \
		echo "Error: FILE is required. Usage: make load FILE=dist/sticky-recorder-*.tar"; \
		exit 1; \
	fi
	docker load -i $(FILE)
	@echo "Image loaded from $(FILE)"

clean: ## Remove sticky-recorder images and dist/
	-docker images --format '{{.Repository}}:{{.Tag}}' | grep '^sticky-recorder' | xargs -r docker rmi 2>/dev/null || true
	-rm -rf $(DIST_DIR)

.DEFAULT_GOAL := help
