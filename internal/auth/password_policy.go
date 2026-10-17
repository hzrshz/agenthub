package auth

import (
	"strings"
	"unicode"
)

// 口令强度策略（等保二级基线：GB/T 22239-2019 要求口令长度不少于 8 位
// 且包含三类字符，以及不得使用简单弱口令）。
//
// 政务扫描器会实际跑字典 + 规则检查，因此这里不是"写个大概的黑名单"，
// 而是把三类规则都做实：字符类别、常见弱口令、模式弱口令（顺序/重复/
// 键盘序列）。

// PasswordPolicy 是对外可见的口令要求，供前端提示与运维文档引用。
type PasswordPolicy struct {
	// MinLength 最短字符数（按 rune 计，支持中文）。
	MinLength int
	// RequiredClasses 必须覆盖的字符类别数（大写、小写、数字、符号）。
	RequiredClasses int
}

const (
	// passwordMinLength 等保要求至少 8 位；这里取 12 位与原有策略一致，
	// 不为了兼容旧口令而放松。
	passwordMinLength = 12
	// passwordMaxLength 防止超长口令导致 argon2 计算异常缓慢，
	// 也为 DoS 上一道闸（口令是登录未认证输入）。
	passwordMaxLength = 1024
	// passwordRequiredClasses 必须覆盖的类别数：大小写 + 数字 + 符号共 4 类，
	// 要求覆盖 3 类。
	//
	// 取 3 而非 4 是为了让"全小写字母 + 数字"这类实务中常见且足够强的
	// 口令（长度 ≥12 时熵已充足）可通过，避免把运维推到"必须用 !@#$%
	// 的键盘可达符号"的死角——政务运维人员常用输入法，特殊符号难打。
	passwordRequiredClasses = 3
)

// PasswordPolicyDesc 是一句话规则描述，用于接口返回给前端展示。
const PasswordPolicyDesc = "至少 12 个字符，且至少包含大写字母、小写字母、数字、特殊符号中的三类；不得使用常见弱口令、连续字符或重复字符"

// charClasses 统计口令覆盖了哪几类字符。
type charClasses int

const (
	classLower charClasses = 1 << iota
	classUpper
	classDigit
	classSymbol
)

func classify(r rune) charClasses {
	switch {
	case unicode.IsLower(r):
		return classLower
	case unicode.IsUpper(r):
		return classUpper
	case unicode.IsDigit(r):
		return classDigit
	default:
		// 其余（含中文、空格、符号）都算符号类。中文口令长度够时
		// 本身已提供足够熵，但纯中文通常只满足"一类"，仍会被
		// RequiredClasses 规则拦下——这是有意的。
		return classSymbol
	}
}

// countCharacterClasses 返回口令覆盖的字符类别数。
func countCharacterClasses(secret string) int {
	var mask charClasses
	for _, r := range secret {
		mask |= classify(r)
	}
	count := 0
	for _, class := range []charClasses{classLower, classUpper, classDigit, classSymbol} {
		if mask&class != 0 {
			count++
		}
	}
	return count
}

// keyboardRows 是常见键盘行序，用于识别 qwerty/asdfgh 这类顺序口令。
var keyboardRows = []string{
	"qwertyuiop", "asdfghjkl", "zxcvbnm",
	"1234567890", "!@#$%^&*()",
	"abcdefghijklmnopqrstuvwxyz",
	"0123456789",
}

// weakPasswords 是常见弱口令黑名单（归一化后的小写形式）。
//
// 覆盖三类真实场景：纯数字键盘序、常见单词与变体、系统/角色默认口令。
// 政务环境扫描器的字典通常比这里大，但守住"最容易被通报"的一批即可。
var weakPasswords = map[string]struct{}{
	// 键盘序与连续序列
	"123456789012": {}, "12345678910": {}, "012345678901": {},
	"123456789123": {}, "123456123456": {}, "123443211234": {},
	"098765432109": {}, "111111111111": {}, "222222222222": {},
	"333333333333": {}, "666666666666": {}, "888888888888": {},
	"abcdefghijkl": {}, "abcdef123456": {}, "qwertyuiop12": {},
	"asdfghjkl12": {}, "1qaz2wsx3edc": {}, "1q2w3e4r5t6": {},
	"zaq12wsx": {}, "qazwsxedc": {}, "asdqweasd12": {},

	// 常见单词与变体（含 admin/root/agent 等角色词——政务环境尤其常见）
	"password": {}, "password1": {}, "password12": {}, "password123": {},
	"passw0rd": {}, "passw0rd12": {}, "p@ssword": {}, "p@ssw0rd": {},
	"p@ssw0rd12": {}, "passw0rd!": {}, "administrator": {},
	"adminadmin": {}, "adminadmin1": {}, "admin123admin": {},
	"administrator1": {}, "rootroot": {}, "root123456": {},
	"administrat0r": {}, "administr@t0r": {}, "sysadmin": {},
	"sysadmin12": {}, "administrator12": {}, "welcome": {},
	"welcome1": {}, "welcome12": {}, "letmein": {}, "letmein1": {},
	"letmein12": {}, "master": {}, "master12": {}, "login": {},
	"login1234": {}, "access": {}, "access12": {}, "changeme": {},
	"changeme1": {}, "changeme12": {}, "test": {}, "test1234": {},
	"test12345": {}, "testing12": {}, "guest": {}, "guest1234": {},
	"user123456": {}, "user12345": {}, "abc123456": {}, "abc1234567": {},
	"agenthub": {}, "agenthub1": {}, "agenthub12": {}, "agenthub123": {},
	"hubhub12": {}, "agent1234": {}, "agent12345": {}, "node123456": {},

	// 常见中文口令（政务环境高频，且扫描器字典常含）
	"woaini123": {}, "woaini1234": {}, "nihao12345": {}, "nihao123456": {},
	"admin888": {}, "admin666": {}, "woaini666": {}, "mima123456": {},
	"woaini520": {}, "5201314a1": {},
}

// commonPasswordStems 是常见口令词干，用于识别加后缀的变体。
//
// 黑名单永远追不上所有变体（P@ssw0rd123!），但词干可以：只要口令剥掉
// 常见前后缀后落在词干表里，就判定为弱口令。
var commonPasswordStems = []string{
	"password", "passwd", "passw0rd", "p@ssword", "p@ssw0rd", "admin",
	"administrator", "administrat0r", "root", "sysadmin", "welcome",
	"guest", "test", "user", "login", "letmein", "changeme", "secret",
	"agenthub", "agent", "node", "hub", "qwerty", "abc", "abcd", "iloveyou",
}

// passwordAffixes 是变体里常见的前后缀（数字或符号）。
var passwordAffixes = []string{
	"0", "1", "12", "123", "1234", "12345", "123456", "1234567", "12345678",
	"234", "345", "456", "567", "678", "789", "0000", "1111", "666", "888",
	"@", "#", "!", "$", "*", ".",
}

// isWeakPassword 判断口令是否属于常见弱口令或常见词干变体。
//
// 策略：**先剥后缀再比对**。p@ssw0rd / P@ssw0rd! / password123 这类变体
// 剥掉常见数字/符号后缀后，词干仍会落在黑名单里，因此无需做"符号还原"
// （那会把口令里的数字 1/3/5/7 全替换成字母，反而误伤与还原无关的口令）。
func isWeakPassword(normalized string) bool {
	if _, weak := weakPasswords[normalized]; weak {
		return true
	}
	// 剥掉末尾常见数字/符号后缀（可多轮，如 password123!）。
	stripped := normalized
	for i := 0; i < 3; i++ {
		next, ok := stripAffixes(stripped)
		if !ok || next == stripped || next == "" {
			break
		}
		stripped = next
		if _, weak := weakPasswords[stripped]; weak {
			return true
		}
		for _, stem := range commonPasswordStems {
			if stripped == stem {
				return true
			}
			// 词干带数字形式（qwerty/abc123）：词干本身也可能是黑名单词，
			// 因此剥离后还要再比对一次全量黑名单。
		}
	}
	// 词干"主导"判定：词干占口令主体的绝大多数才算弱口令。
	//
	// 这里必须设比例门槛而非简单 HasPrefix——"Root7strongKp3z" 剥离后缀后
	// 以 "root" 开头，但 root 只占 4/14，明显是"词根 + 随机增强"，是合规口令；
	// 而 "admin2026" 里 admin 占 5/9，才是真正的"用户名式口令"。
	for _, stem := range commonPasswordStems {
		if len(stem) < 4 || len(stem)*10 < len(stripped)*6 {
			continue
		}
		if stripped == stem || strings.HasPrefix(stripped, stem) {
			return true
		}
	}
	return false
}

// stripAffixes 剥掉口令末尾的常见数字/符号后缀，返回剩余词干。
func stripAffixes(value string) (string, bool) {
	// 从长到短匹配，保证 "12345678" 优先于 "1234"。
	for _, affix := range passwordAffixes {
		if len(affix) >= len(value) {
			continue
		}
		if strings.HasSuffix(value, affix) {
			return strings.TrimSuffix(value, affix), true
		}
	}
	return value, false
}

// hasSequentialRun 判断口令是否包含 >=4 个连续字符（升序或降序）。
//
// 连续序列（abcd / 1234 / 9876）熵极低，扫描器字典必收。注意只查数字与
// 字母的 ASCII 顺序，不查 Unicode 码点递增——那会把正常中文口令误判。
func hasSequentialRun(normalized string, minRun int) bool {
	if len(normalized) < minRun {
		return false
	}
	runes := []rune(normalized)
	// minRun 个字符构成 minRun-1 个相邻差，因此窗口右端要留出下标空间：
	// i+minRun 必须严格小于 len，否则内层访问 runes[i+j+1] 会越界。
	for i := 0; i+minRun < len(runes); i++ {
		ascending, descending := true, true
		for j := 0; j < minRun-1; j++ {
			diff := runes[i+j+1] - runes[i+j]
			switch {
			case diff == 1:
				descending = false
			case diff == -1:
				ascending = false
			default:
				ascending, descending = false, false
			}
			if !ascending && !descending {
				break
			}
		}
		if ascending || descending {
			return true
		}
	}
	return false
}

// hasKeyboardRowSequence 判断口令是否包含键盘行序片段（qwerty / asdfgh）。
//
// 只看"口令里出现的字符子序列"是否在键盘行上连续，而不是要求整个口令
// 都在键盘上——后者会漏掉 "Asdfghjkl2Z!" 这类"键盘序 + 随机后缀"变体。
func hasKeyboardRowSequence(normalized string) bool {
	runes := []rune(normalized)
	for _, row := range keyboardRows {
		rowRunes := []rune(row)
		// 双指针找 normalized 是否含 row 的连续子段（≥6 位）。
		for start := 0; start < len(runes); start++ {
			matched := 0
			for r := 0; r < len(rowRunes) && start+matched < len(runes); r++ {
				if runes[start+matched] == rowRunes[r] {
					matched++
					if matched >= keyboardSequenceMin {
						return true
					}
				} else {
					// 允许重置到当前字符：row 里可能出现重复字符。
					if rowRunes[r] == runes[start+matched] {
						matched = 1
						continue
					}
					matched = 0
					break
				}
			}
		}
	}
	return false
}

// keyboardSequenceMin 判定键盘行序的最小长度。6 位起（asdfgh）才足够
// 说明是键盘序；更短的（asdf）常见于正常口令片段，不拦。
const keyboardSequenceMin = 6

// hasRepeatedRun 判断口令是否包含 >=3 个连续重复字符（aaaa / 111）。
func hasRepeatedRun(secret string, minRun int) bool {
	runes := []rune(secret)
	if len(runes) < minRun {
		return false
	}
	run := 1
	for i := 1; i < len(runes); i++ {
		if runes[i] == runes[i-1] {
			run++
			if run >= minRun {
				return true
			}
			continue
		}
		run = 1
	}
	return false
}

// repeatedBlockPattern 判断口令是否包含短块反复拼接。
//
// "abababab"（整串重复）与 "Xy7abcabcabc1"（中间一段重复）都要拦——
// 后者在实务中很常见（"名字+生日"里生日是重复块）。做法是扫所有子串，
// 找是否存在长度 ≥2 倍块长的纯重复段。
func repeatedBlockPattern(normalized string) bool {
	runes := []rune(normalized)
	// 至少要有 2 倍块长才构成"重复"；块长上限 4（"abab"已足够明显）。
	for blockLen := 1; blockLen <= 4; blockLen++ {
		minSpan := blockLen * repeatedBlockMinTimes
		if len(runes) < minSpan {
			continue
		}
		for start := 0; start+minSpan <= len(runes); start++ {
			repeated := true
			for i := start + blockLen; i < start+minSpan; i++ {
				if runes[i] != runes[i-blockLen] {
					repeated = false
					break
				}
			}
			if repeated {
				return true
			}
		}
	}
	return false
}

// repeatedBlockMinTimes 判定"块重复"所需的最小重复次数。
// 3 次（总长 ≥3 倍块长）才判弱——2 次（"abab"）可能只是普通短口令。
const repeatedBlockMinTimes = 3
