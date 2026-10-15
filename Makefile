.PHONY: check ci web build test

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
