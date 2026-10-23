PIG := ./bin/pig-test
PIG_ISOLATED_DIR ?= /root/.pig-test/agent

.PHONY: all build test vet install-test examples-smoke examples-run help

all: help

help:
	@echo "Available targets:"
	@echo "  build           - Compile all packages (go build ./...)"
	@echo "  test            - Run tests across all packages (go test ./...)"
	@echo "  vet             - Run static analysis (go vet ./...)"
	@echo "  install-test    - Validate and install into isolated directory (PIG_ISOLATED_DIR)"
	@echo "  examples-smoke  - Load examples/hello in isolated PiG and call hello_ping (JSON output; makes one model call)"
	@echo "  examples-run    - Same as examples-smoke, plain text output (makes one model call)"
	@echo "  help            - Show this help message"

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

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
