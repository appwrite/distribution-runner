TAG ?= dev
REPOSITORY ?= appwrite/distribution
AGENT_PLATFORMS ?= darwin/arm64 darwin/amd64
GOTESTFLAGS ?=
TRIVY_CACHE_DIR ?= $(CURDIR)/.trivy/cache
TRIVY_TMP ?= $(CURDIR)/.trivy/tmp

HADOLINT_IMAGE := hadolint/hadolint:v2.15.1@sha256:32dac94127fd60b7b7e3fbfc65e1383b9b5e25c9bfd7b8536de7a539fe68a12d
ACTIONLINT_IMAGE := rhysd/actionlint:1.7.12@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667
BATS_IMAGE := bats/bats:1.14.0@sha256:5322b877351fda0cc435de8c6116de7d0a2ec79d7c680132a0ef329a633bc66f
TRIVY_IMAGE := aquasec/trivy:0.76.2@sha256:8f6bb947dbc528902e11f2f5be52f7b519f6a39d5d148f41166f2040499768fb
DEBIAN_IMAGE := debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

TOOLS_IMAGE := $(REPOSITORY)-tools:$(TAG)
TOOLS_TEST_IMAGE := $(REPOSITORY)-tools:test
TOOLS_BATS_IMAGE := $(REPOSITORY)-tools:bats
FIXTURE_IMAGE := $(REPOSITORY)-fixture:$(TAG)
ANDROID_IMAGE := $(REPOSITORY)-android:$(TAG)

export TOOLS_IMAGE TOOLS_TEST_IMAGE FIXTURE_IMAGE ANDROID_IMAGE

SCRIPTS = $(wildcard $(shell git ls-files --cached --others --exclude-standard -- '*.sh'))
BATS = $(wildcard tests/*.bats tests/helpers.bash)
DOCKERFILES = $(wildcard images/*/Dockerfile tests/Dockerfile)

TRIVY = docker run --rm \
	-v /var/run/docker.sock:/var/run/docker.sock \
	-v "$(TRIVY_CACHE_DIR):/root/.cache/trivy" \
	-v "$(TRIVY_TMP):/tmp" \
	-v "$(CURDIR):/repo:ro" -w /repo \
	$(TRIVY_IMAGE)

.DEFAULT_GOAL := help

.PHONY: help lint shellcheck hadolint actionlint tools tools-test fixture fixture-generate android agent \
	test-tools test-fixture test-android test-agent scan-agent check

help:
	@echo 'Usage: make <target> [TAG=dev]'
	@echo ''
	@echo '  lint              shellcheck, hadolint and actionlint'
	@echo '  check             lint, build tools and fixture, run tools, fixture and agent tests'
	@echo '  tools             build $(TOOLS_IMAGE)'
	@echo '  tools-test        build $(TOOLS_TEST_IMAGE) (test CA trusted, never published)'
	@echo '  fixture           build $(FIXTURE_IMAGE)'
	@echo '  fixture-generate  regenerate images/fixture/app-unsigned.aab in an amd64 container'
	@echo '  android           build $(ANDROID_IMAGE) (CI only)'
	@echo '  agent             build the agent for $(AGENT_PLATFORMS) into agent/bin'
	@echo '  test-tools        run tests/tools.bats inside the tools test image'
	@echo '  test-fixture      run tests/fixture.sh'
	@echo '  test-android      run tests/android.sh (CI only)'
	@echo '  test-agent        go vet and go test the agent'
	@echo '  scan-<image>      Trivy gate and report for tools, fixture or android'
	@echo '  scan-agent        Trivy gate for the agent binaries'

lint: shellcheck hadolint actionlint

shellcheck:
ifeq ($(strip $(SCRIPTS) $(BATS)),)
	@echo 'skip: no shell scripts'
else
	$(if $(SCRIPTS),shellcheck $(SCRIPTS))
	$(if $(BATS),shellcheck -s bash $(BATS))
endif

hadolint:
ifeq ($(DOCKERFILES),)
	@echo 'skip: no Dockerfiles'
else
	docker run --rm -v "$(CURDIR):/repo:ro" -w /repo $(HADOLINT_IMAGE) hadolint $(DOCKERFILES)
endif

actionlint:
	docker run --rm -v "$(CURDIR):/repo:ro" -w /repo $(ACTIONLINT_IMAGE)

tools:
ifeq ($(wildcard images/tools/Dockerfile),)
	@echo 'skip: images/tools/Dockerfile not found'
else
	docker build -t $(TOOLS_IMAGE) images/tools
endif

tools-test:
ifeq ($(wildcard images/tools/Dockerfile),)
	@echo 'skip: images/tools/Dockerfile not found'
else
	docker build --target test -t $(TOOLS_TEST_IMAGE) images/tools
endif

fixture:
ifeq ($(wildcard images/fixture/Dockerfile),)
	@echo 'skip: images/fixture/Dockerfile not found'
else
	docker build --build-arg VERSION=$(TAG) -t $(FIXTURE_IMAGE) images/fixture
endif

fixture-generate:
ifeq ($(wildcard images/fixture/generate.sh),)
	@echo 'skip: images/fixture/generate.sh not found'
else
	docker run --rm --platform linux/amd64 -v "$(CURDIR)/images/fixture:/work" -w /work $(DEBIAN_IMAGE) ./generate.sh
endif

android:
ifeq ($(wildcard images/android/Dockerfile),)
	@echo 'skip: images/android/Dockerfile not found'
else ifneq ($(CI),true)
	$(error the android image builds only in CI, set CI=true to override)
else
	docker build --platform linux/amd64 --build-arg VERSION=$(TAG) -t $(ANDROID_IMAGE) images/android
endif

agent:
ifeq ($(wildcard agent/go.mod),)
	@echo 'skip: agent/go.mod not found'
else
	@for platform in $(AGENT_PLATFORMS); do \
		os=$${platform%/*}; \
		arch=$${platform#*/}; \
		echo "build: agent/bin/distribution-agent-$$os-$$arch"; \
		(cd agent && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '-s -w -X main.version=$(TAG)' -o "bin/distribution-agent-$$os-$$arch" .) || exit 1; \
	done
endif

test-tools: tools-test
ifeq ($(wildcard tests/tools.bats),)
	@echo 'skip: tests/tools.bats not found'
else
	docker build --build-arg TOOLS_IMAGE=$(TOOLS_TEST_IMAGE) --build-arg BATS_IMAGE=$(BATS_IMAGE) -t $(TOOLS_BATS_IMAGE) tests
	docker run --rm --network none -v "$(CURDIR)/tests:/tests:ro" -v "$(CURDIR)/images/fixture:/fixture:ro" $(TOOLS_BATS_IMAGE) /tests/tools.bats
endif

test-fixture: tools fixture
ifeq ($(wildcard tests/fixture.sh),)
	@echo 'skip: tests/fixture.sh not found'
else
	tests/fixture.sh
endif

test-android: android
ifeq ($(wildcard tests/android.sh),)
	@echo 'skip: tests/android.sh not found'
else ifneq ($(CI),true)
	$(error the android tests run only in CI, set CI=true to override)
else
	tests/android.sh
endif

test-agent:
ifeq ($(wildcard agent/go.mod),)
	@echo 'skip: agent/go.mod not found'
else
	cd agent && go vet ./... && go test $(GOTESTFLAGS) ./...
endif

scan-%: %
	@if [ ! -f images/$*/Dockerfile ]; then echo 'skip: images/$*/Dockerfile not found'; exit 0; fi; \
	mkdir -p "$(TRIVY_CACHE_DIR)" "$(TRIVY_TMP)" && \
	echo 'gate: $(REPOSITORY)-$*:$(TAG) os packages, fixable CRITICAL' && \
	$(TRIVY) image --scanners vuln --pkg-types os --severity CRITICAL --ignore-unfixed --exit-code 1 --no-progress --ignorefile .trivyignore $(REPOSITORY)-$*:$(TAG) && \
	echo 'report: $(REPOSITORY)-$*:$(TAG) libraries, CRITICAL and HIGH' && \
	$(TRIVY) image --scanners vuln --pkg-types library --severity CRITICAL,HIGH --exit-code 0 --format table --no-progress --ignorefile .trivyignore $(REPOSITORY)-$*:$(TAG)

scan-agent: agent
ifeq ($(wildcard agent/go.mod),)
	@echo 'skip: agent/go.mod not found'
else
	mkdir -p "$(TRIVY_CACHE_DIR)" "$(TRIVY_TMP)"
	$(TRIVY) rootfs --scanners vuln --severity CRITICAL,HIGH --ignore-unfixed --exit-code 1 --no-progress --ignorefile .trivyignore agent/bin
endif

check: lint tools tools-test fixture test-tools test-fixture test-agent
