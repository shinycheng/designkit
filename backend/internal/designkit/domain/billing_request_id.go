package domain

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ============================================================================
// 计费 request_id
// ============================================================================
//
// 这是整个模块**最容易出人命**的地方，两个方向都能出事：
//
// 【方向一：一单 N 张只扣 1 张的钱】
// 上游算计费幂等键时优先取 context 里的 ClientRequestID
// （internal/service/gateway_usage_billing.go），而 RequestLogger 是全局中间件，
// 每个入站 HTTP 请求注一个 id。我们的合成 context 继承了入站请求的全部 ctx value，
// 于是一次 POST /jobs 展开的 N 张图会共用同一个 id ——
// 幂等表 usage_billing_dedup 的唯一键是 (request_id, api_key_id)，
// 第 2 张起 applied=false，**静默不扣钱**。
// 更糟的是 multipart 的计费指纹只取 StickySessionSeed，seed 里不含图片字节，
// 所以「图1×词1」和「图2×词1」的指纹完全相同，靠指纹也兜不住。
// → 每一张图都必须有自己独有的 id，并在合成请求的 context 上**无条件覆写**
//   （不是「没有才设」）ctxkey.ClientRequestID 和 ctxkey.RequestID。
//
// 【方向二：钱扣了、账单没有，还没有任何告警】
// usage_logs.request_id 是 VARCHAR(64)（001_init.sql）。
// 扣费和写账单是**两个独立事务、扣费在前**。id 超长的后果不是被截断，
// 是 PostgreSQL 抛 22001 让整条 INSERT 失败 —— 钱已经扣了，账单写不进去；
// 而兜底逻辑只是换个 context 再插一次，同样的原因会再失败一次，
// 然后只打一行日志：**没有告警、没有重试、没有补偿队列**。
// 上游还会自己在前面拼 "client:" 前缀（7 字节），所以我们能用的上限是 57，不是 64。
// → 单测必须断言 len("client:" + id) <= 64，并遍历 seq / attempt 的边界值。
//
// 【格式，只有一种】
//
//	我们生成：      dki:<job_uid>:<seq 4位>:<attempt 2位>     长度 38
//	实际落 usage_logs：client:dki:<job_uid>:<seq>:<attempt>   长度 45
//
// 【规则，写死】
//   - 同一个 (seq, attempt) **无论重投几次都用同一个 id** —— 幂等，不重复扣钱。
//     网络抖动导致我们不确定上游到底扣没扣时，重投同一个 id 是安全的。
//   - **attempt + 1 就换新 id** —— 该扣就得扣。重试一张 = 重新出一张图 =
//     上游真的又跑了一次 = 必须产生一条新的 usage_logs。
//     沿用旧 id 的后果是：图重出了、成本在上游产生了，我们**一分钱不扣**。
//   - designkit_job_items.billing_request_id 存的是**带 client: 前缀的完整值**
//     （StoredBillingRequestID 的返回值），否则拿它去 usage_logs 反查一条也查不到，
//     结算时会一直等不到账单、无限退避重试。

const (
	// BillingRequestIDPrefix designkit image 的固定前缀。
	// 只有这一个前缀（早期草案里的 dkh:/dkc:/dkr: 已随方案调整整体删除）。
	BillingRequestIDPrefix = "dki"

	// UpstreamClientRequestIDPrefix 上游在落 usage_logs.request_id 之前
	// 自己拼上去的前缀。我们**不要**自己拼进发给上游的值里，
	// 只在往 designkit_job_items.billing_request_id 落库时用。
	UpstreamClientRequestIDPrefix = "client:"

	// UsageLogRequestIDMaxLen 上游 usage_logs.request_id 列的宽度（VARCHAR(64)）。
	// 超一个字符就是 PostgreSQL 22001，后果见文件头。
	UsageLogRequestIDMaxLen = 64

	// BillingRequestIDLen 我们生成的 id 的固定长度：
	// len("dki") + 1 + 26 + 1 + 4 + 1 + 2 = 38
	BillingRequestIDLen = 38

	// StoredBillingRequestIDLen 落库 / 落 usage_logs 的完整长度：7 + 38 = 45。
	// 距离 64 还有 19 字符余量，够上游以后再加前缀。
	StoredBillingRequestIDLen = 45

	// ULIDLength ULID 的字符数。job uid 列是 VARCHAR(32)，但值恒为 26 位。
	ULIDLength = 26

	// BillingRequestIDMaxSeq seq 用 4 位补零，上限 9999。
	// 单次批量上限在 designkit_settings.max_batch_items（默认 50），离这个天花板很远。
	BillingRequestIDMaxSeq = 9999

	// BillingRequestIDMaxAttempt attempt 用 2 位补零，上限 99。
	// job_items.max_attempts 默认 3。
	BillingRequestIDMaxAttempt = 99
)

// ulidAlphabet 是 Crockford Base32 字符集（去掉了容易看混的 I、L、O、U）。
const ulidAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ErrInvalidBillingRequestID 解析或校验 request_id 失败。
var ErrInvalidBillingRequestID = errors.New("designkit: invalid billing request id")

// BillingRequestID 生成发给上游的计费 request_id。
//
//	BillingRequestID("01J8ZK7Q9X2M4N6P8R0T2V4W6Y", 3, 1)
//	  → "dki:01J8ZK7Q9X2M4N6P8R0T2V4W6Y:0003:01"
//
// jobUID 必须是 26 位 ULID，seq 从 1 开始，attempt 从 1 开始
// （attempt 跟 job_items.attempt_count 对齐 —— 那一列在**领取那一刻**就 +1，
// 所以第一次出图时它已经是 1）。
//
// 参数越界不会在这里报错（调用方用 ValidateBillingRequestIDParts 先校验）；
// 越界时格式化出来的 id 会变长，但即使 seq 到七位数、attempt 到三位数，
// 加上 client: 前缀也才 49 字符，仍在 64 以内 —— 这是刻意留的余量，
// 宁可 id 难看也绝不能让那条 INSERT 因为 22001 失败。
func BillingRequestID(jobUID string, seq, attempt int) string {
	return fmt.Sprintf("%s:%s:%04d:%02d", BillingRequestIDPrefix, jobUID, seq, attempt)
}

// StoredBillingRequestID 返回**最终落进 usage_logs.request_id 的那个值**，
// 也正是要存进 designkit_job_items.billing_request_id 的值：
//
//	"client:" + BillingRequestID(...)
//
// 结算时按这一列去 SUM(usage_logs.actual_cost)，两边必须一模一样，
// 少了 client: 前缀就一条也查不到。
func StoredBillingRequestID(jobUID string, seq, attempt int) string {
	return UpstreamClientRequestIDPrefix + BillingRequestID(jobUID, seq, attempt)
}

// ValidateBillingRequestIDParts 在生成 id 之前校验三个入参。
// 建 job_items 和每次重投之前都该调一次 —— 越界不会立刻炸，
// 但会让 request_id 变长、让「同一 (seq,attempt) 同一 id」的幂等假设更难验证。
func ValidateBillingRequestIDParts(jobUID string, seq, attempt int) error {
	if !IsValidULID(jobUID) {
		return fmt.Errorf("%w: job uid 必须是 %d 位 ULID，实际 %q", ErrInvalidBillingRequestID, ULIDLength, jobUID)
	}
	if seq < 1 || seq > BillingRequestIDMaxSeq {
		return fmt.Errorf("%w: seq 必须在 1..%d，实际 %d", ErrInvalidBillingRequestID, BillingRequestIDMaxSeq, seq)
	}
	if attempt < 1 || attempt > BillingRequestIDMaxAttempt {
		return fmt.Errorf("%w: attempt 必须在 1..%d，实际 %d", ErrInvalidBillingRequestID, BillingRequestIDMaxAttempt, attempt)
	}
	return nil
}

// ParseBillingRequestID 把 id 拆回 (jobUID, seq, attempt)。
// 带不带 client: 前缀都能解析 —— 从数据库读出来的是带前缀的。
//
// 用途：结算对账时反查、排障时从 usage_logs 找回是哪个批次的第几张。
func ParseBillingRequestID(id string) (jobUID string, seq int, attempt int, err error) {
	raw := strings.TrimPrefix(strings.TrimSpace(id), UpstreamClientRequestIDPrefix)

	parts := strings.Split(raw, ":")
	if len(parts) != 4 || parts[0] != BillingRequestIDPrefix {
		return "", 0, 0, fmt.Errorf("%w: %q", ErrInvalidBillingRequestID, id)
	}
	if !IsValidULID(parts[1]) {
		return "", 0, 0, fmt.Errorf("%w: job uid 段不是 ULID：%q", ErrInvalidBillingRequestID, id)
	}
	seq, err = strconv.Atoi(parts[2])
	if err != nil || seq < 1 {
		return "", 0, 0, fmt.Errorf("%w: seq 段解析失败：%q", ErrInvalidBillingRequestID, id)
	}
	attempt, err = strconv.Atoi(parts[3])
	if err != nil || attempt < 1 {
		return "", 0, 0, fmt.Errorf("%w: attempt 段解析失败：%q", ErrInvalidBillingRequestID, id)
	}
	return parts[1], seq, attempt, nil
}

// IsValidULID 判断是不是 26 位大写 Crockford Base32 字符串。
// 只校验长度和字符集，不校验时间戳部分是否合理（那不是我们的职责）。
func IsValidULID(s string) bool {
	if len(s) != ULIDLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(ulidAlphabet, rune(s[i])) {
			return false
		}
	}
	return true
}
