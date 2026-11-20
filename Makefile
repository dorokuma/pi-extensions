# pi-extensions 仓库根 Makefile：唯一检查入口 make check。
# zero-node 迁移后（08f8c71 删除顶层 package.json/pnpm/tsconfig），顶层无
# node 构建面；install.sh 仍负责把 hosted/federated 扩展幂等同步到部署目录。
# install-check 校验部署脚本的语法与回归测试，不触发真实安装；
# go-check 递归 go/ 下全组件的 fmt/sdk/vet/build/test。

.PHONY: all check install-check go-check help

all: check

help:
	@echo "Available targets:"
	@echo "  check         - Run install-check + go-check (repository gate)"
	@echo "  install-check - bash -n syntax check + tests/install.test.sh"
	@echo "  go-check      - $(MAKE) -C go check (fmt/sdk/vet/build/test)"
	@echo "  help          - Show this help message"

check: install-check go-check
	@echo "OK: repository check passed"

install-check:
	@echo "==> bash -n install.sh tests/install.test.sh"
	@bash -n install.sh || exit $$?
	@bash -n tests/install.test.sh || exit $$?
	@echo "==> bash tests/install.test.sh"
	@bash tests/install.test.sh || exit $$?

go-check:
	@echo "==> $(MAKE) -C go check"
	@$(MAKE) -C go check || exit $$?
