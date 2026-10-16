package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/uvwt/agenthub/internal/auth"
	"github.com/uvwt/agenthub/internal/config"
	"github.com/uvwt/agenthub/internal/core"
)

// adminCommand 实现 agenthub admin init|reset。
// 与 nexusdock 同一安全立场：管理员口令只经本地交互输入，
// 不提供环境变量/参数形态（避免进入 shell history 与 ps 输出）。
func adminCommand(ctx context.Context, cfg config.Config, sub string, args []string) error {
	if sub != "init" && sub != "reset" {
		return fmt.Errorf("unknown admin subcommand %q", sub)
	}
	db, err := openAdminDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	svc := auth.NewService(db)

	switch sub {
	case "init":
		initialized, err := svc.AdminStatus(ctx)
		if err != nil {
			return err
		}
		if initialized {
			return errors.New("administrator is already initialized; use 'agenthub admin reset' to change the password")
		}
		fmt.Println("初始化 AgentHub 管理员账号（口令输入不回显）")
		username, err := promptLine("管理员用户名（邮箱形态）: ")
		if err != nil {
			return err
		}
		password, err := promptPasswordTwice()
		if err != nil {
			return err
		}
		user, err := svc.InitializeAdmin(ctx, username, password)
		if err != nil {
			return err
		}
		fmt.Printf("管理员已创建：%s（id=%s）\n", user.Username, user.ID)
		return nil
	case "reset":
		initialized, err := svc.AdminStatus(ctx)
		if err != nil {
			return err
		}
		if !initialized {
			return errors.New("administrator is not initialized; use 'agenthub admin init' first")
		}
		fmt.Println("重置管理员口令（请确认服务已停止；重置后全部会话失效）")
		username, err := promptLine("管理员用户名: ")
		if err != nil {
			return err
		}
		password, err := promptPasswordTwice()
		if err != nil {
			return err
		}
		var userID string
		err = db.QueryRowContext(ctx, `SELECT id FROM users WHERE lower(username) = lower(?) AND role = 'admin'`, strings.TrimSpace(username)).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("administrator %q not found", username)
		}
		if err != nil {
			return fmt.Errorf("read administrator: %w", err)
		}
		if err := svc.ResetUserPassword(ctx, "cli", userID, password); err != nil {
			return err
		}
		fmt.Printf("管理员 %s 的口令已重置，下次登录需强制改密。\n", username)
		return nil
	}
	return nil
}

func openAdminDB(ctx context.Context, cfg config.Config) (*sql.DB, error) {
	db, err := core.OpenSQLite(ctx, cfg.ControlDBPath(), 2)
	if err != nil {
		return nil, err
	}
	if err := core.EnsureSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func promptLine(prompt string) (string, error) {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read input: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", errors.New("input is required")
	}
	return line, nil
}

// promptPasswordTwice 读两遍口令并确认一致；term.ReadPassword 关闭回显。
func promptPasswordTwice() (string, error) {
	first, err := promptPassword("口令（至少 12 字符）: ")
	if err != nil {
		return "", err
	}
	second, err := promptPassword("再次输入口令: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("passwords do not match")
	}
	return first, nil
}

func promptPassword(prompt string) (string, error) {
	fmt.Print(prompt)
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimSpace(string(raw))
	if password == "" {
		return "", errors.New("password is required")
	}
	return password, nil
}
