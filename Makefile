.PHONY: check ci web web-check build test release-linux

# web: 构建前端产物到 internal/httpx/web_dist（产物提交入库；
# CI 会校验产物与源码一致，改前端后必须执行 make web 再提交）
web:
	cd web && npm ci && npm run build

# web-check: 校验已提交的嵌入产物是否与源码构建结果一致（防漂移）
web-check: web
	@if [ -n "$$(git status --porcelain internal/httpx/web_dist)" ]; then \
		echo "ERROR: web_dist 与前端源码不一致，请提交重新构建后的产物"; \
		git status --porcelain internal/httpx/web_dist; \
		exit 1; \
	fi
	@echo "web_dist is up to date"

build:
	go build ./...

test:
	go test ./... -count=1

# check: 常规验证（go 测试 + 前端类型检查 + 产物漂移校验）
check: test web-check
	cd web && npx tsc -b

# ci: 完整交付验证
ci: web check
	go vet ./...

# release-linux: 本地静态交叉编译全部国产化目标
# （与 .github/workflows/release.yml 同参数；依赖纯 Go 栈，CGO_ENABLED=0 即可）
TARGETS := amd64 arm64 loong64 riscv64
release-linux:
	mkdir -p dist
	@for arch in $(TARGETS); do \
		echo "== linux/$$arch =="; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch \
		go build -trimpath -ldflags "-s -w -X main.version=$$(git describe --tags --always 2>/dev/null || echo dev)" \
		-o dist/agenthub-linux-$$arch ./cmd/agenthub || exit 1; \
	done
	@cd dist && shasum -a 256 agenthub-* > checksums.txt && ls -la

# docker-image: 本地装配方言镜像构建（单架构，默认当前平台）
# 依赖 dist/agenthub-linux-<arch> 与 certs/（都由 release-linux 产出）
release-image:
	mkdir -p certs
	docker create --name tz-tmp golang:1.26 true >/dev/null
	docker cp tz-tmp:/etc/ssl/certs/ca-certificates.crt certs/ca-certificates.crt
	docker cp tz-tmp:/usr/local/go/lib/time/zoneinfo.zip certs/zoneinfo.zip
	docker rm -f tz-tmp >/dev/null
	mkdir -p bin && cp dist/agenthub-linux-$(ARCH) bin/agenthub-linux-$(ARCH)
	docker build --build-arg BIN_NAME=bin/agenthub-linux-$(ARCH) -t agenthub-local .
