# Default PiG home: $PIG_HOME if set, else ~/.pig (production home after trial drop).
PIG_HOME_DIR ?= $(or $(PIG_HOME),$(HOME)/.pig)
PIG := ./bin/pig-test
PIG_ISOLATED_DIR ?= $(PIG_HOME_DIR)/agent
COMPONENTS := $(filter-out pigsdk,$(patsubst %/go.mod,%,$(wildcard */go.mod */cmd/*/go.mod)))

# PIGSDK_LOCAL_PATCHES: 本地相对 staged SDK 的故意偏差（防御性守卫），格式
# 为「相对路径:md5」。sdk-check 除这些文件及其声明的 md5 外，仍要求 go/pigsdk
# 与 staged SDK 逐文件逐字节一致；改防御内容须同步更新此处 md5，删除防御须移除条目。
# 声明的路径必须同时存在于 staged 与本地 md5 清单：staged 缺失即 FAIL，
# 避免上游删文件被过期豁免掩盖。
PIGSDK_LOCAL_PATCHES := \
	./context.go:e90606f4b873db36de583541439b0951 \
	./mcp_extension_options.go:f5d88a9db0ec05f31d10946b99fe202c

.PHONY: all build test vet sdk-check install-test examples-smoke examples-run help

all: help

help:
	@echo "Available targets:"
	@echo "  build           - Compile root module and all component modules"
	@echo "  test            - Run tests across root module and all component modules"
	@echo "  vet             - Run static analysis on root module and all component modules"
	@echo "  sdk-check       - Compare local pigsdk against staged PiG SDK md5sum (plus declared local patches)"
	@echo "  install-test    - Validate and install into isolated directory (PIG_ISOLATED_DIR)"
	@echo "  examples-smoke  - Load examples/hello in PiG and call hello_ping (JSON output; makes one model call)"
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
	for d in "$(PIG_HOME_DIR)"/state/pigsdk/sdk-*; do \
		[ -d "$$d" ] || continue; \
		case "$$d" in \
			*'*') continue ;; \
		esac; \
		[ -f "$$d/go.mod" ] || continue; \
		if [ -z "$$staged" ] || [ "$$d" -nt "$$staged" ]; then \
			staged="$$d"; \
		fi; \
	done; \
	if [ -z "$$staged" ] || [ ! -d "$$staged" ]; then \
		echo "SKIP: staged SDK not found under $(PIG_HOME_DIR)/state/pigsdk"; \
		exit 0; \
	fi; \
	echo "Checking pigsdk against staged SDK: $$staged"; \
	(cd "$$staged" && find . -type f ! -path "./README.md" -exec md5sum {} + | sort -k2) > "$$tmpdir/staged.md5"; \
	(cd pigsdk && find . -type f ! -path "./README.md" -exec md5sum {} + | sort -k2) > "$$tmpdir/local.md5"; \
	patch_paths=""; \
	for entry in $(PIGSDK_LOCAL_PATCHES); do \
		patch_paths="$$patch_paths $${entry%%:*}"; \
	done; \
	for file in staged local; do \
		awk -v paths="$$patch_paths" '{p=" " paths " "; if (index(p, " " $$2 " ")) next; print}' "$$tmpdir/$$file.md5" > "$$tmpdir/$$file.core" || { echo "FAILED: filtering $$file.md5" >&2; exit 1; }; \
	done; \
	if ! diff -u "$$tmpdir/staged.core" "$$tmpdir/local.core"; then \
		echo "FAILED: pigsdk differs from staged SDK ($$staged)" >&2; \
		exit 1; \
	fi; \
	for entry in $(PIGSDK_LOCAL_PATCHES); do \
		path=$${entry%%:*}; want=$${entry##*:}; \
		if ! awk -v p="$$path" '$$2 == p {found=1} END {exit !found}' "$$tmpdir/staged.md5"; then \
			echo "FAILED: declared patch $$path absent from staged SDK (stale PIGSDK_LOCAL_PATCHES entry; remove it or re-sync staged)" >&2; \
			exit 1; \
		fi; \
		line=$$(awk -v p="$$path" '$$2 == p {print; exit}' "$$tmpdir/local.md5"); \
		got=$${line%% *}; \
		if [ "$$got" != "$$want" ]; then \
			echo "FAILED: local patch $$path md5=$${got:-MISSING}, want $$want (re-sync staged or update PIGSDK_LOCAL_PATCHES)" >&2; \
			exit 1; \
		fi; \
		echo "PATCH OK: $$path ($$want)"; \
	done; \
	echo "OK: pigsdk matches staged SDK ($$staged) plus declared local patches"

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
