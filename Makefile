# pi-extensions 仓库根 Makefile：唯一检查入口 make check。
# zero-node 迁移后（08f8c71 删除顶层 package.json/pnpm/tsconfig），顶层无
# node 构建面；install.sh 作为薄兼容入口委托给 Go 实现（go/cmd/pi-ext-install）。
# install-check 校验安装器单元测试与脚本语法；
# go-check 递归 go/ 下全组件的 fmt/sdk/vet/build/test。

.PHONY: all check install-check go-check help

all: check

help:
	@echo "Available targets:"
	@echo "  check         - Run install-check + go-check (repository gate)"
	@echo "  install-check - Run Go installer tests + bash -n shim check (and bash tests when jq is available)"
	@echo "  go-check      - $(MAKE) -C go check (fmt/sdk/vet/build/test)"
	@echo "  help          - Show this help message"

check: install-check go-check
	@echo "OK: repository check passed"

install-check:
	@echo "==> bash -n install.sh tests/install.test.sh"
	@bash -n install.sh || exit $$?
	@bash -n tests/install.test.sh || exit $$?
	@echo "==> go test ./cmd/pi-ext-install/..."
	@cd go && go test -race -count=1 ./cmd/pi-ext-install/... || exit $$?
	@if command -v jq >/dev/null 2>&1; then \
		echo "==> bash tests/install.test.sh (with prebuilt pi-ext-install)"; \
		tmpbin=$$(mktemp /tmp/pi-ext-install.XXXXXX); \
		trap 'rm -f "$$tmpbin"' EXIT; \
		(cd go && go build -o "$$tmpbin" ./cmd/pi-ext-install) || exit $$?; \
		PI_EXT_INSTALL_BIN="$$tmpbin" bash tests/install.test.sh || exit $$?; \
	fi

go-check:
	@echo "==> $(MAKE) -C go check"
	@$(MAKE) -C go check || exit $$?
