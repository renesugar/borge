# SPDX-License-Identifier: Apache-2.0

SHELL       := /usr/bin/env bash
MODULE      := github.com/renesugar/borge
BIN         := bin/borge
VERSION     ?= dev
LDFLAGS     := -X $(MODULE)/internal/version.Version=$(VERSION)

# The pinned borg 2 reference interpreter; see tests/borg2/setup.sh.
BORG2       := tests/borg2/borg2

# Go's default per-package test timeout is 10 minutes, which the differential suites
# exceed: they drive a Python borg over a pipe across a whole corpus, and under -race
# several of them take longer than that on their own. The deadline still exists - a test
# that hangs must fail rather than run forever - it is just set to fit the work.
TIMEOUT     ?= -timeout 60m

.PHONY: all build test race cover bench fmt vet lint check spdx layering interop coverage \
        borg2 upstream-licenses msgpack-fixtures item-fixtures evidence clean help

all: check

## build: compile the borge binary into bin/
build:
	@mkdir -p bin
	go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/borge

## test: run the unit tests
test:
	go test $(TIMEOUT) ./...

## interop: run the stage 7 interoperability gate (needs 'make build' and 'make borg2')
# Separate from 'make test' because it drives two real binaries over real corpora and
# takes tens of minutes; 'make check' stays fast enough to run before every commit.
interop: build
	go test $(TIMEOUT) -v ./tests/interop/

## race: run the tests under the race detector
# PackWriter (stage 3) hands packs to a background writer while the caller keeps
# mutating the ChunkIndex. That invariant is exactly the kind that fails rarely and
# corrupts repositories when it does, so -race is not optional here.
race:
	go test -race $(TIMEOUT) ./...

## cover: run tests with coverage, write coverage.out and print the summary
cover:
	go test $(TIMEOUT) -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

## bench: run benchmarks
bench:
	go test -run '^$$' -bench . -benchmem ./...

## fmt: format all Go source
fmt:
	gofmt -w .

## vet: run go vet
vet:
	go vet ./...

## lint: run golangci-lint if it is installed
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "lint: golangci-lint not installed, skipping (go vet still runs via 'make vet')"; \
	fi

## spdx: check every Go file's license header (docs/LICENSING.md section 5)
spdx:
	./scripts/check-spdx.sh

## layering: check that imports point downward (docs/PORTING_PLAN.md section 1)
layering:
	./scripts/check-layering.sh

## check: the gate - formatting, vet, lint, license headers, layering, tests
check: fmtcheck vet lint spdx layering test
	@echo "check: all green"

.PHONY: fmtcheck
fmtcheck:
	@out=$$(gofmt -l . | grep -v '^vendor/' || true); \
	if [ -n "$$out" ]; then echo "gofmt: these files need formatting:"; echo "$$out"; exit 1; fi
	@echo "gofmt: ok"

## borg2: build the pinned borg 2 reference interpreter used by the interop tests
borg2:
	./tests/borg2/setup.sh

## upstream-licenses: record the borghash/borgstore licenses (plan task 0.8)
upstream-licenses:
	./scripts/check-upstream-licenses.sh

## item-fixtures: regenerate the item differential fixtures from the pinned borg
item-fixtures:
	@test -x .venv-borg2/bin/python || { echo "run 'make borg2' first"; exit 1; }
	PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 \
		.venv-borg2/bin/python internal/item/testdata/gen_fixtures.py \
		> internal/item/testdata/fixtures.txt
	@echo "regenerated internal/item/testdata/fixtures.txt"

## msgpack-fixtures: regenerate the msgpack differential fixtures from the pinned borg
# The fixtures are checked in so the package is testable without the venv; regenerate
# them only when the pin moves, and review the diff - a changed fixture means borg's
# encoding changed, which is a format change, not a test update.
msgpack-fixtures:
	@test -x .venv-borg2/bin/python || { echo "run 'make borg2' first"; exit 1; }
	PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 \
		.venv-borg2/bin/python internal/msgpackx/testdata/gen_fixtures.py \
		> internal/msgpackx/testdata/fixtures.txt
	@echo "regenerated internal/msgpackx/testdata/fixtures.txt"

## coverage: compare borg's subcommand list against borge's (the stage 8 gate)
coverage: build
	./tests/evidence/command-coverage.sh

## evidence: build a stage evidence bundle, e.g. make evidence STAGE=stage-0
evidence:
	@if [ -z "$(STAGE)" ]; then echo "usage: make evidence STAGE=stage-N"; exit 64; fi
	./tests/evidence/mkbundle.sh $(STAGE)

## clean: remove build and test output
clean:
	rm -rf bin coverage.out

## help: list the targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
