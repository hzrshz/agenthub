.PHONY: check ci web web-check build test test-race release-linux

# web: 构建前端产物到 internal/httpx/web_dist（产物提交入库；
# CI 会校验产物与源码一致，改前端后必须执行 make web 再提交）
web:
	cd web && npm ci && npm run build

# web-check: 确认已提交的嵌入产物与"现在重新构建的结果"一致（防漂移）。
#
# 真正的漂移风险是：改了前端源码/依赖却没跑 make web 就提交，导致
# web_dist 与源码不同步。因此判据是——重新构建之后，产物相对**暂存区
# 或 HEAD**（取较新的那个，即"将要提交的状态"）有没有变化。
# 这比看 git status 准：它忽略"这次改动还没提交"这种正常中间态，
# 只在"重建结果 ≠ 提交内容"时判红。
web-check: web
	@REF=HEAD; \
	if ! git diff --quiet --cached; then REF=--cached; fi; \
	if git diff --quiet $$REF -- internal/httpx/web_dist; then \
		echo "web_dist is up to date"; \
	else \
		echo "ERROR: 重新构建后的 internal/httpx/web_dist 与已提交产物不一致。"; \
		echo "前端源码/依赖已变更但产物没重新构建或没提交，请执行 make web 并提交产物："; \
		git status --porcelain internal/httpx/web_dist; \
		git diff --stat $$REF -- internal/httpx/web_dist | head -20; \
		exit 1; \
	fi

build:
	go build ./...

test:
	go test ./... -count=1

# test-race: 并发热点（连接替换、inflight/rate map、fleet reconcileMu）的竞态检测。
test-race:
	go test ./... -race -count=1

# check: 常规验证（go 测试 + race + vet + 前端类型检查 + 产物漂移校验）
# vet 放在这里而不只放 ci：AGENTS.md 把 make check 定为"常规验证"，
# 只在 ci 跑 vet 意味着开发者按常规流程会漏掉它。
check: test test-race web-check
	go vet ./...
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
