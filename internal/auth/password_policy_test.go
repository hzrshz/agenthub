package auth

import (
	"strings"
	"testing"
)

// 口令策略基线（等保二级 GB/T 22239-2019 8.1.4.6）。
//
// 这类规则最容易被"为了通过测试"或"为了方便运维"而放松，因此每条规则
// 都用正反两个方向锁定：合规口令必须通过，典型弱口令必须被拒。
func TestPasswordPolicyAcceptsStrongPasswords(t *testing.T) {
	valid := []struct {
		password string
		note     string
	}{
		{"K7mQx2wrTp9zVn", "大小写+数字"},
		{"Corr7horseKp4z", "含常见词但加了符号"},
		{"xK2mQ9zVnTp7wR4s", "高熵随机串"},
		{"Tr0ub4dor&3xyz", "含符号"},
		{"Zhang3san#2026x", "姓名拼音+符号"},
		{"Root7strongKp3", "角色词 + 随机增强（词根占比低，属合规）"},
		{"Adm1n7xKp9zQm", "admin 变体 + 随机增强"},
	}
	for _, tc := range valid {
		t.Run(tc.note, func(t *testing.T) {
			if err := ValidatePassword("someone@example.com", tc.password); err != nil {
				t.Errorf("password %q must be accepted: %v", tc.password, err)
			}
		})
	}
}

func TestPasswordPolicyRejectsWeakPasswords(t *testing.T) {
	cases := []struct {
		password string
		reason   string
	}{
		// 长度不足
		{"Ab3$xy", "太短"},
		{"Ab3$xyz9", "不足 12 位"},

		// 字符类别不足（等保要求至少三类）
		{"abcdefghijkl", "纯小写"},
		{"ABCDEFGHIJKL", "纯大写"},
		{"123456789012", "纯数字"},
		{"密码密码密码密码", "纯中文"},
		{"abcdefghij1234", "小写+数字仅两类"},

		// 弱口令字典
		{"password1234", "常见弱口令"},
		{"P@ssw0rd1234", "符号替换变体"},
		{"admin12345678", "角色词+数字"},
		{"Root12345678", "root 变体"},
		{"administrator12", "administrator"},
		{"agenthub12345", "项目名变体"},
		{"Admin@2026", "admin@年份"},

		// 模式弱口令
		{"Abcd1234Efgh!", "包含 abcdef 连续序列"},
		{"Xy9$9876543Qw", "包含降序序列"},
		{"Qwert12345Z$", "键盘序"},
		{"Asdfghjkl2Z!", "键盘序 asdf"},
		{"Ab1111112345c", "连续重复字符"},
		{"abababababab", "整串短块重复"},
		{"Xy7abcabcabc1", "含短块重复"},
		{"Qw9xyzxyzxyz2", "3 位块重复"},

		// 与用户名相关
		{"Zhangsan12345", "用户名直接作口令"},
		{"alice@example.com-Aa1!", "含邮箱本地部分"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			if err := ValidatePassword("alice@example.com", tc.password); err == nil {
				t.Errorf("password %q (%s) must be rejected", tc.password, tc.reason)
			}
		})
	}
}

// 用户名本身作为口令必须被拒（等保明确要求）。
func TestPasswordPolicyRejectsUsernameVariants(t *testing.T) {
	for _, username := range []string{"alice@example.com", "root@example.com"} {
		cases := []string{
			username,
			strings.ToUpper(username),
			strings.ToLower(username),
		}
		for _, password := range cases {
			if err := ValidatePassword(username, password); err == nil {
				t.Errorf("password %q must be rejected when it equals username %q", password, username)
			}
		}
	}
}

// 口令里包含用户名的一部分也要拒绝——"alice@example.com" + 固定后缀
// 是典型的"看起来很复杂"的实际弱口令。
func TestPasswordPolicyRejectsUsernameContained(t *testing.T) {
	// 只拦"用户名作主体、后面挂少量后缀"这一种——
	// 要求整体仍主要由用户名构成，才算"用户名口令"。
	for _, password := range []string{"alice2026", "alice12345", "ALICE@2026", "alice@example"} {
		if err := ValidatePassword("alice@example.com", password); err == nil {
			t.Errorf("password %q contains the username and must be rejected", password)
		}
	}
}

// 超长口令要拒绝：口令是登录未认证输入，超长会让 argon2 计算异常缓慢，
// 是可被利用的 CPU 消耗点。
func TestPasswordPolicyRejectsOverlongPassword(t *testing.T) {
	overlong := strings.Repeat("aB3$", 400) // 1600 字符，远超上限
	if err := ValidatePassword("alice@example.com", overlong); err == nil {
		t.Fatal("overlong password must be rejected")
	}
}

// 口令策略的字符类别计数。
func TestCountCharacterClasses(t *testing.T) {
	cases := []struct {
		secret string
		want   int
		note   string
	}{
		{"abc", 1, "纯小写"},
		{"ABC", 1, "纯大写"},
		{"123", 1, "纯数字"},
		{"aA1", 3, "大小写+数字"},
		{"aA1!", 4, "四类齐全"},
		{"密码", 1, "纯中文算一类"},
		{"密码abc123", 3, "中文(符号类)+小写+数字"},
	}
	for _, tc := range cases {
		if got := countCharacterClasses(tc.secret); got != tc.want {
			t.Errorf("countCharacterClasses(%q) = %d, want %d (%s)", tc.secret, got, tc.want, tc.note)
		}
	}
}

// 连续序列检测只应针对 ASCII 顺序，不能误伤正常中文口令。
func TestSequentialRunOnlyForASCII(t *testing.T) {
	// minRun=4 指"4 个连续差"（5 个字符），因此用 abcde / 98765。
	if !hasSequentialRun("abcde", 4) {
		t.Error("abcde must be detected as sequential")
	}
	if !hasSequentialRun("98765", 4) {
		t.Error("98765 must be detected as descending sequential")
	}
	if hasSequentialRun("abcd", 4) {
		t.Error("abcd is only 3 diffs, below the minRun=4 threshold")
	}
	// 码点递增的中文不是"可预测序列"（熵不为零）。
	if hasSequentialRun("中文密码", 4) {
		t.Error("CJK text must not be treated as a sequential run")
	}
}

// 口令策略常量本身要与描述一致——前端提示直接引用它。
func TestPasswordPolicyConstantsAreConsistent(t *testing.T) {
	if passwordMinLength < 8 {
		t.Errorf("passwordMinLength = %d, 等保二级要求至少 8", passwordMinLength)
	}
	if passwordRequiredClasses < 3 {
		t.Errorf("passwordRequiredClasses = %d, 等保二级要求至少 3 类字符", passwordRequiredClasses)
	}
	if !strings.Contains(PasswordPolicyDesc, "12") {
		t.Errorf("PasswordPolicyDesc must state the 12-character minimum: %q", PasswordPolicyDesc)
	}
}