PIG := ./bin/pig-test
PIG_ISOLATED_DIR ?= /root/.pig-test/agent
COMPONENTS := $(filter-out pigsdk,$(patsubst %/go.mod,%,$(wildcard */go.mod)))

.PHONY: all build test vet sdk-check install-test examples-smoke examples-run help

all: help

help:
	@echo "Available targets:"
	@echo "  build           - Compile root module and all component modules"
	@echo "  test            - Run tests across root module and all component modules"
	@echo "  vet             - Run static analysis on root module and all component modules"
	@echo "  sdk-check       - Compare local pigsdk against staged PiG SDK md5sum"
	@echo "  install-test    - Validate and install into isolated directory (PIG_ISOLATED_DIR)"
	@echo "  examples-smoke  - Load examples/hello in isolated PiG and call hello_ping (JSON output; makes one model call)"
	@echo "  examples-run    - Same as examples-smoke, plain text output (makes one model call)"
	@echo "  help            - Show this help message"

build:
	@echo "==> [root module] go build ./..."
	@go build ./... || { echo "FAILED: ." >&2; exit 1; }
	@for dir in $(COMPONENTS); do \
		echo "==> [component: $$dir] go build ./..."; \
		(cd "$$dir" && go build ./...) || { echo "FAILED: $$dir" >&2; exit 1; }; \
	done

test:
	@echo "==> [root module] go test ./..."
	@go test ./... || { echo "FAILED: ." >&2; exit 1; }
	@for dir in $(COMPONENTS); do \
		echo "==> [component: $$dir] go test ./..."; \
		(cd "$$dir" && go test ./...) || { echo "FAILED: $$dir" >&2; exit 1; }; \
	done

vet:
	@echo "==> [root module] go vet ./..."
	@go vet ./... || { echo "FAILED: ." >&2; exit 1; }
	@for dir in $(COMPONENTS); do \
		echo "==> [component: $$dir] go vet ./..."; \
		(cd "$$dir" && go vet ./...) || { echo "FAILED: $$dir" >&2; exit 1; }; \
	done

sdk-check:
	@tmpdir=$$(mktemp -d /tmp/sdk-check.XXXXXX); \
	trap 'rm -rf "$$tmpdir"' 0 1 2 15; \
	staged=""; \
	for d in /root/.pig-test/state/pigsdk/sdk-*; do \
		case "$$d" in \
			*/sdk-py-*|*/sdk-rs-*|*'*') continue ;; \
			*) [ -d "$$d" ] && staged="$$d" ;; \
		esac; \
	done; \
	if [ -z "$$staged" ] || [ ! -d "$$staged" ]; then \
		echo "SKIP: staged SDK not found"; \
		exit 0; \
	fi; \
	echo "Checking pigsdk against staged SDK: $$staged"; \
	(cd "$$staged" && find . -type f ! -path "./README.md" -exec md5sum {} + | sort -k2) > "$$tmpdir/staged.md5"; \
	(cd pigsdk && find . -type f ! -path "./README.md" -exec md5sum {} + | sort -k2) > "$$tmpdir/local.md5"; \
	if ! diff -u "$$tmpdir/staged.md5" "$$tmpdir/local.md5"; then \
		echo "FAILED: pigsdk differs from staged SDK ($$staged)" >&2; \
		exit 1; \
	fi; \
	echo "OK: pigsdk matches staged SDK ($$staged)"

install-test:
	@if [ "$$(realpath -m "$(PIG_ISOLATED_DIR)")" = "$$(realpath -m "/root/.pi/agent")" ] || [ "$(PIG_ISOLATED_DIR)" = "/root/.pi/agent" ]; then \
		echo "CRITICAL: Refusing to operate on production /root/.pi/agent!" >&2; \
		exit 1; \
	fi
	@if [ ! -d "$(PIG_ISOLATED_DIR)" ]; then \
		echo "Notice: Isolated directory $(PIG_ISOLATED_DIR) does not exist. Creating it..."; \
		mkdir -p "$(PIG_ISOLATED_DIR)"; \
	else \
		echo "Isolated directory verified: $(PIG_ISOLATED_DIR)"; \
	fi
	go build ./...
	@echo "install-test validation succeeded for isolated directory: $(PIG_ISOLATED_DIR)"

examples-smoke:
	$(PIG) -e ./examples/hello --mode json -p "调用 hello_ping 工具并返回其结果"

examples-run:
	$(PIG) -e ./examples/hello -p "调用 hello_ping 工具并返回其结果"
