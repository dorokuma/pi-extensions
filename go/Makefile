PIG := ./bin/pig-test
PIG_ISOLATED_DIR ?= /root/.pig-test/agent

.PHONY: all build test vet install-test examples-build examples-run help

all: help

help:
	@echo "Available targets:"
	@echo "  build           - Compile all packages (go build ./...)"
	@echo "  test            - Run tests across all packages (go test ./...)"
	@echo "  vet             - Run static analysis (go vet ./...)"
	@echo "  install-test    - Validate and install into isolated directory (PIG_ISOLATED_DIR)"
	@echo "  examples-build  - Build/validate examples/hello via PiG runner compiler"
	@echo "  examples-run    - Execute examples/hello tool call via PiG"
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

examples-build:
	$(PIG) -e ./examples/hello --mode json -p "调用 hello_ping 工具并返回其结果"

examples-run:
	$(PIG) -e ./examples/hello -p "调用 hello_ping 工具并返回其结果"
