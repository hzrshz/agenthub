package config

import "testing"

// PublicURL 校验：https 任意放行；反代子路径部署允许带一段 base path
// （单段、无 ..、无 //）；http 仅限 localhost/回环/局域网 IP（测试场景）；
// query/fragment/URL 凭据拒绝；空值拒绝。
func TestValidatePublicURL(t *testing.T) {
	allowed := []string{
		"https://hub.example.com",
		"https://hub.example.com:8443",
		"https://hub.example.com/app",  // 反代子路径部署：/mcp 与回调都在该前缀下
		"https://hub.example.com/app/", // 尾部斜杠容忍（env 加载时会被 trim）
		"http://localhost",
		"http://localhost:8787",
		"http://127.0.0.1:8787",
		"http://192.168.1.10:8787", // 局域网 IP 测试
		"http://10.0.0.5",          // 内网
		"http://[::1]:8787",        // IPv6 回环
	}
	for _, value := range allowed {
		if err := ValidatePublicURL(value); err != nil {
			t.Fatalf("%q should be allowed, got %v", value, err)
		}
	}
	rejected := []string{
		"",                               // 必填
		"http://hub.example.com",         // 公网域名明文
		"https://hub.example.com/a/b",    // 多段子路径：回调地址拼接会产生歧义
		"https://hub.example.com/../app", // .. 路径穿越观感，拒绝
		"https://hub.example.com//app",   // 空段
		"https://hub.example.com/app?x=1",
		"https://hub.example.com/app#frag",
		"https://user:pw@hub.example.com/app", // URL 内嵌凭据
		"ftp://hub.example.com",
		"hub.example.com", // 无 scheme
	}
	for _, value := range rejected {
		if err := ValidatePublicURL(value); err == nil {
			t.Fatalf("%q should be rejected", value)
		}
	}
}

// 控制库后端配置：默认 sqlite 不要求任何 DB 项；mysql/postgres 必须同时
// 给出 DSN 与口令文件，且 DSN 不得内嵌口令（等保"秘密不进配置"红线）。
func TestValidateDBConfig(t *testing.T) {
	base := Config{DataDir: "/tmp/x", Listen: "127.0.0.1:8787", PublicURL: "https://hub.example.com"}

	// 默认（未设置 HUB_DB_*）→ sqlite，DSN/口令文件为空，合法。
	if err := base.Validate(); err != nil {
		t.Fatalf("default sqlite should pass: %v", err)
	}
	// 显式 sqlite 等价。
	sqliteCfg := base
	sqliteCfg.DBDriver = "sqlite"
	if err := sqliteCfg.Validate(); err != nil {
		t.Fatalf("explicit sqlite should pass: %v", err)
	}
	// 配了 mysql 专用项却不切驱动：防呆拒绝（否则运维会误以为已连 MySQL）。
	confused := base
	confused.DBDSN = "user@tcp(127.0.0.1:3306)/hub"
	if err := confused.Validate(); err == nil {
		t.Fatal("DSN without driver should be rejected")
	}

	// 完整合法的 mysql 配置。
	mysqlCfg := base
	mysqlCfg.DBDriver = "mysql"
	mysqlCfg.DBDSN = "hub@tcp(127.0.0.1:3306)/agenthub"
	mysqlCfg.DBPasswordFile = "/run/secrets/db-pass"
	if err := mysqlCfg.Validate(); err != nil {
		t.Fatalf("valid mysql config should pass: %v", err)
	}
	// PG 的 key=value 形态同样合法。
	pgCfg := base
	pgCfg.DBDriver = "postgres"
	pgCfg.DBDSN = "host=127.0.0.1 port=5432 user=hub dbname=agenthub"
	pgCfg.DBPasswordFile = "/run/secrets/db-pass"
	if err := pgCfg.Validate(); err != nil {
		t.Fatalf("valid postgres config should pass: %v", err)
	}

	// 缺 DSN。
	noDSN := mysqlCfg
	noDSN.DBDSN = ""
	if err := noDSN.Validate(); err == nil {
		t.Fatal("mysql without DSN should be rejected")
	}
	// 缺口令文件。
	noPW := mysqlCfg
	noPW.DBPasswordFile = ""
	if err := noPW.Validate(); err == nil {
		t.Fatal("mysql without password file should be rejected")
	}
	// mysql DSN 内嵌口令段 user:{{SECRET_p0a3evgu}}
	embedded := mysqlCfg
	embedded.DBDSN = "hub:some-secret@tcp(127.0.0.1:3306)/agenthub"
	if err := embedded.Validate(); err == nil {
		t.Fatal("mysql DSN with embedded password should be rejected")
	}
	// PG DSN 内嵌 password=。
	embeddedPG := pgCfg
	embeddedPG.DBDSN = "host=x user=h password=s dbname=d"
	if err := embeddedPG.Validate(); err == nil {
		t.Fatal("postgres DSN with password= should be rejected")
	}
	// URL userinfo 形态（mysql://user:pw@...）一律拒绝。
	urlEmbedded := mysqlCfg
	urlEmbedded.DBDSN = "mysql://user:pw@127.0.0.1:3306/agenthub"
	if err := urlEmbedded.Validate(); err == nil {
		t.Fatal("URL DSN with userinfo should be rejected")
	}
	// 未知驱动。
	weird := base
	weird.DBDriver = "oracle"
	weird.DBDSN = "x"
	weird.DBPasswordFile = "/p"
	if err := weird.Validate(); err == nil {
		t.Fatal("unknown driver should be rejected")
	}
}

// HUB_DB_MAX_OPEN_CONNS 继承：非法值拒绝，合法值生效，默认 10。
func TestLoadDBMaxOpenConns(t *testing.T) {
	t.Setenv("HUB_DATA_DIR", t.TempDir())
	t.Setenv("HUB_LISTEN", "127.0.0.1:8787")
	t.Setenv("HUB_PUBLIC_URL", "https://hub.example.com")
	t.Setenv("HUB_DB_MAX_OPEN_CONNS", "25")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load with DB_MAX_OPEN_CONNS: %v", err)
	}
	if cfg.DBMaxOpenConns != 25 {
		t.Fatalf("DBMaxOpenConns = %d, want 25", cfg.DBMaxOpenConns)
	}
	t.Setenv("HUB_DB_MAX_OPEN_CONNS", "abc")
	if _, err := Load(); err == nil {
		t.Fatal("invalid HUB_DB_MAX_OPEN_CONNS should fail Load")
	}
}
