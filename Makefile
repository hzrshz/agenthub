.PHONY: check ci web build test release-linux

# web: 构建前端产物到 internal/httpx/web_dist（提交嵌入产物前必须执行）
web:
	cd web && npm ci && npm run build

build:
	go build ./...

test:
	go test ./... -count=1

# check: 常规验证（go 测试 + 前端类型检查）
check: test
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
