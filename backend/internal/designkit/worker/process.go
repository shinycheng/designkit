package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// itemResult 是一张图跑完之后要写回库里的东西。
type itemResult struct {
	// images 本次产出的图（**可能不止一张**，n 参数的行为还没实测）。
	images []dkdomain.NewImage
	// storedBillingRequestID 带 client: 前缀的完整值，结算靠它回 usage_logs 对账。
	storedBillingRequestID string
}

// preparedImage 是准备好发给网关的输入图（预处理之后的）。
type preparedImage struct {
	data        []byte
	contentType string
	filename    string
	width       int
	height      int
}

// processItem 跑完一张图的全链路：预处理 → 出图 → 存图。
//
// **不写库**（除了 holding → running 那一下和预处理产物的缓存行），
// 写回统一由调用方在一个干净的 context 上做 —— 出图这个 context 随时可能超时，
// 而写回绝不能因为超时丢掉。
func (p *Pool) processItem(ctx context.Context, job *dkdomain.Job, item *dkdomain.JobItem, log *slog.Logger) (*itemResult, error) {
	// 第二道尝试次数守卫（第一道在 handleItem 领到 item 的那一刻）。
	//
	// 为什么值得重复：这个函数是**唯一会真的调网关花钱**的地方，漏判一次
	// 就是一张图的钱。领取 SQL 的僵尸回收分支刻意不看 attempt，所以这里
	// 不能假设「能进到这个函数的都在预算内」。
	if attemptBudgetExceeded(item.AttemptCount, item.MaxAttempts) {
		return nil, failFinal(dkdomain.ErrCodeMaxAttemptsExceeded).
			withUpstream(fmt.Sprintf("attempt %d exceeds max_attempts %d",
				item.AttemptCount, normalizeMaxAttempts(item.MaxAttempts)))
	}

	// 运营已经点过「停止排队」：这一张是抢在 stop 那条 UPDATE 之前被领出来的（很窄的竞态）。
	// 不出图。运营点停止的动机 100% 是「别再花钱」，让它继续跑等于当场违背按钮的承诺。
	if job.IsStopRequested() {
		return nil, failFinal(dkdomain.ErrCodeCanceled).
			withUpstream("job stop requested before this item started")
	}

	// 第一张被领走时把批次从 holding 推到 running。
	// 已经是 running 了会返回 ErrConflict（守卫没命中），那是正常的，不是错误。
	if job.Status == dkdomain.JobStatusHolding {
		err := p.deps.Repo.UpdateJobStatus(ctx, job.ID, dkdomain.JobStatusHolding, dkdomain.JobStatusRunning)
		if err != nil && !errors.Is(err, dkdomain.ErrConflict) {
			log.Warn("designkit 批次转 running 失败", slog.Any("error", err))
		}
	}

	// 计费 id 的三个入参必须先校验。越界不会当场炸，但会让 id 变长、
	// 让「同一 (seq,attempt) 用同一个 id」的幂等假设不成立。
	if err := dkdomain.ValidateBillingRequestIDParts(job.UID, item.Seq, item.AttemptCount); err != nil {
		return nil, failFinal(dkdomain.ErrCodeInternal).withCause(err)
	}

	input, err := p.resolveImage(ctx, job, item, log)
	if err != nil {
		return nil, err
	}

	model := strings.TrimSpace(job.Model)
	if model == "" {
		model = dkdomain.DefaultModel
	}

	res, err := p.deps.Gateway.Generate(ctx, dkdomain.GenerateRequest{
		UserID:   job.UserID,
		APIKeyID: job.APIKeyID,
		JobUID:   job.UID,
		Seq:      item.Seq,
		Attempt:  item.AttemptCount,
		Model:    model,
		// ⚠ 发给模型的不是原文，是**原文 + 一句比例要求**（见 withRatioInstruction）。
		// 落库的快照仍然是运营看到的那句原文 —— 两者刻意不同，
		// 不然历史记录里每条提示词后面都挂着一句系统加的话，运营会以为是自己写的。
		Prompt: withRatioInstruction(item.PromptText, job.Ratio),
		// ⚠ 上游**不听**这个 size（2026-08-14 实测：输入 554×554 的 1:1 图、
		// 请求 554x554，拿回来的是 1087×1447 的 3:4）。
		// 原因是这个值来自补边后图片的真实像素，而那几乎永远不是接口认的标准尺寸
		// （1024x1024 那一类），发一个它不认的值等于没发，模型就按自己的默认出 ——
		// 它的默认是竖版。
		//
		// 仍然发它，是因为它决定上游把请求归到 images-native 还是 images-basic 档
		// （openai_images.go:501 的 ExplicitSize），而那会改变账号筛选的结果。
		// 在只改一个变量的前提下，先靠提示词控比例（见上面的 Prompt）。
		Size:             fmt.Sprintf("%dx%d", input.width, input.height),
		Ratio:            job.Ratio,
		ImageData:        input.data,
		ImageFilename:    input.filename,
		ImageContentType: input.contentType,
		// **不带** client: 前缀。实现会把它无条件覆写进合成请求的 context，
		// 否则一单 N 张共用入站请求的 id，第 2 张起静默不扣钱。
		BillingRequestID: dkdomain.BillingRequestID(job.UID, item.Seq, item.AttemptCount),
		Timeout:          p.settings.ItemTimeout,
	})
	if err != nil {
		return nil, err
	}

	if len(res.Images) == 0 {
		// 上游 2xx 但一张图都没有。**不重试**：这种情况多半钱已经扣了，
		// 重试就是再扣一次还是拿不到图。让运营看到失败、去找管理员查上游。
		return nil, failFinal(dkdomain.ErrCodeUpstreamError).
			withUpstream(truncateForLog(string(res.RawResponse)))
	}

	stored := strings.TrimSpace(res.StoredBillingRequestID)
	if stored == "" {
		stored = dkdomain.StoredBillingRequestID(job.UID, item.Seq, item.AttemptCount)
	}

	images, err := p.storeImages(ctx, job, item, res.Images)
	if err != nil {
		return nil, err
	}

	return &itemResult{images: images, storedBillingRequestID: stored}, nil
}

// resolveImage 拿到「补好白边、可以直接发给网关」的图片字节。
//
// 顺序：先找已有的预处理产物（variant），没有才调 Python 服务现做一份。
// variant 的唯一键是 (asset_id, ratio, keep_transparency, max_dimension) ——
// 同一张原图在 3:4 和 16:9 任务里是**两个不同文件**，绝不能互相复用。
func (p *Pool) resolveImage(ctx context.Context, job *dkdomain.Job, item *dkdomain.JobItem, log *slog.Logger) (*preparedImage, error) {
	if item.AssetID == nil {
		// 出图走的是 images/edits，没有输入图根本发不出去。
		return nil, failFinal(dkdomain.ErrCodeInvalidRequest).
			withUpstream("job item has no asset")
	}
	assetID := *item.AssetID
	maxDimension := p.settings.MaxDimension

	// keep_transparency 按批取值（designkit_jobs.keep_transparency，9004 起）。
	// 9004 之前的历史批次回填 FALSE，跟它们当时走全局配置的实际行为一致。
	variant, err := p.deps.Repo.GetVariant(ctx, assetID, job.Ratio, job.KeepTransparency, maxDimension)
	// ErrNotFound 是正常路径（还没预处理过），其余错误才是真的出事了。
	if err != nil && !errors.Is(err, dkdomain.ErrNotFound) {
		return nil, failRetry(dkdomain.ErrCodeInternal).withCause(err)
	}

	if err == nil && variant != nil {
		if item.VariantID != nil && *item.VariantID != variant.ID {
			// 提交时记的产物跟现在查出来的不是同一行。唯一键决定了参数完全一致，
			// 所以内容等价，但这说明 variant 行被重建过 —— 留一行日志便于排查。
			log.Warn("designkit 预处理产物 id 与提交时记录的不一致，按参数重新解析",
				slog.Int64("item_variant_id", *item.VariantID),
				slog.Int64("resolved_variant_id", variant.ID))
		}
		data, contentType, getErr := p.deps.Store.Get(ctx, variant.ObjectKey)
		if getErr == nil {
			return &preparedImage{
				data:        data,
				contentType: firstNonEmpty(contentType, variant.ContentType, "image/png"),
				filename:    filenameFor(variant.ObjectKey, firstNonEmpty(contentType, variant.ContentType)),
				width:       intOrTarget(variant.Width, job.Ratio, maxDimension, true),
				height:      intOrTarget(variant.Height, job.Ratio, maxDimension, false),
			}, nil
		}
		if !errors.Is(getErr, dkdomain.ErrObjectNotFound) {
			return nil, failRetry(dkdomain.ErrCodeStorageError).withCause(getErr)
		}
		// 产物记录还在、文件没了（换过存储、手工清过盘）。重做一份即可。
		log.Warn("designkit 预处理产物文件不存在，重新预处理",
			slog.String("object_key", variant.ObjectKey))
	}

	return p.preprocess(ctx, job, assetID, maxDimension, log)
}

// preprocess 调 Python 服务补白边，并把产物存起来供下次复用。
//
// **fail-closed**：Python 侧任何异常都判这一张失败，**绝不拿原图顶上去**。
// 老代码是 fail-open 的，搬过来的后果是：补边失败 → 原图什么比例出什么比例 →
// 运营选 3:4 拿回 4:3，钱照扣，而日志里只有一行 warning。
func (p *Pool) preprocess(ctx context.Context, job *dkdomain.Job, assetID int64, maxDimension int, log *slog.Logger) (*preparedImage, error) {
	assets, err := p.deps.Repo.GetAssetsByIDs(ctx, job.UserID, []int64{assetID})
	if err != nil {
		return nil, failRetry(dkdomain.ErrCodeInternal).withCause(err)
	}
	if len(assets) == 0 || assets[0] == nil {
		return nil, failFinal(dkdomain.ErrCodeAssetNotFound).
			withUpstream(fmt.Sprintf("asset %d not found for user %d", assetID, job.UserID))
	}
	asset := assets[0]

	raw, contentType, err := p.deps.Store.Get(ctx, asset.ObjectKey)
	if err != nil {
		if errors.Is(err, dkdomain.ErrObjectNotFound) {
			// 原图文件没了，重试多少次都没用。
			return nil, failFinal(dkdomain.ErrCodeAssetNotFound).withCause(err)
		}
		return nil, failRetry(dkdomain.ErrCodeStorageError).withCause(err)
	}
	contentType = firstNonEmpty(contentType, asset.ContentType, "application/octet-stream")

	result, err := p.deps.Preprocessor.Preprocess(ctx, dkdomain.PreprocessRequest{
		Data:             raw,
		Filename:         filenameFor(asset.ObjectKey, contentType),
		ContentType:      contentType,
		Ratio:            job.Ratio,
		KeepTransparency: job.KeepTransparency,
		// ⚠ **每次都要显式带**，值取自 designkit_settings.max_dimension。
		// 不带的话，管理员把它从 2048 改成 4096 而 Python 那边的环境变量没跟着改，
		// 就会「界面说 4K、实际按 2K 出图」，而且不报错。
		MaxDimension: maxDimension,
	})
	if err != nil {
		// 同一张图重试还是会失败（预处理是纯函数），所以不重试。
		return nil, failFinal(dkdomain.ErrCodePreprocessFailed).withCause(err)
	}
	if result == nil || len(result.Data) == 0 {
		return nil, failFinal(dkdomain.ErrCodePreprocessFailed).
			withUpstream("preprocess returned empty body")
	}

	outType := firstNonEmpty(result.ContentType, "image/png")
	key := dkdomain.VariantObjectKey(p.now(), asset.UID, job.Ratio, job.KeepTransparency, maxDimension, extForContentType(outType))
	if err := p.putWithRetry(ctx, key, outType, result.Data); err != nil {
		return nil, failRetry(dkdomain.ErrCodeStorageError).withCause(err)
	}

	width, height := result.Width, result.Height
	if _, err := p.deps.Repo.UpsertVariant(ctx, dkdomain.UpsertVariantParams{
		AssetID:          asset.ID,
		Ratio:            job.Ratio,
		KeepTransparency: job.KeepTransparency,
		MaxDimension:     maxDimension,
		ObjectKey:        key,
		Width:            &width,
		Height:           &height,
		ContentType:      outType,
	}); err != nil {
		// 产物行只是缓存：写不进去下次重做一遍就是了，不该让这一张图失败。
		log.Warn("designkit 预处理产物入库失败，本次仍照常出图", slog.Any("error", err))
	}

	if width <= 0 || height <= 0 {
		width, height = targetSize(job.Ratio, maxDimension)
	}
	return &preparedImage{
		data:        result.Data,
		contentType: outType,
		filename:    filenameFor(key, outType),
		width:       width,
		height:      height,
	}, nil
}

// storeImages 把网关返回的每一张图落进对象存储，并组装成待入库的行。
//
// object_key 的格式是**定死**的：
// designkit/<YYYY>/<MM>/<DD>/<job_uid>-<seq>-<attempt>-<image_index>.png
// image_index 从 1 开始 —— 万一网关一次返回两张，第二张也得有地方放，
// 少一个维度就是「钱花了、图丢了」。
func (p *Pool) storeImages(ctx context.Context, job *dkdomain.Job, item *dkdomain.JobItem, generated []dkdomain.GeneratedImage) ([]dkdomain.NewImage, error) {
	createdAt := p.now()
	out := make([]dkdomain.NewImage, 0, len(generated))

	for i := range generated {
		img := generated[i]
		imageIndex := i + 1
		key := dkdomain.ImageObjectKey(createdAt, job.UID, item.Seq, item.AttemptCount, imageIndex)
		contentType := firstNonEmpty(img.ContentType, "image/png")

		if err := p.putWithRetry(ctx, key, contentType, img.Data); err != nil {
			// 图已经出了、钱已经花了，但存不进去。
			// **不重试整张**：重试 = 重新出图 = 再扣一次钱，而存储坏着多半还是存不进。
			// 判终态并把 key 打进日志，管理员能照着查存储。
			return nil, failFinal(dkdomain.ErrCodeStorageError).
				withCause(err).
				withUpstream("failed to persist generated image at " + key)
		}

		width, height := img.Width, img.Height
		tier := dkdomain.ClassifyBillingTierOrDefault(width, height)
		newImage := dkdomain.NewImage{
			UID:         p.newUID(),
			ImageIndex:  imageIndex,
			ObjectKey:   key,
			ContentType: contentType,
			ByteSize:    int64(len(img.Data)),
			BillingTier: &tier,
		}
		if width > 0 {
			w := width
			newImage.Width = &w
		}
		if height > 0 {
			h := height
			newImage.Height = &h
		}
		out = append(out, newImage)
	}
	return out, nil
}

// putWithRetry 存对象，失败重试两次。
//
// 只重试**存储**，不重试出图 —— 出图重试要花钱，存储重试不要。
func (p *Pool) putWithRetry(ctx context.Context, key, contentType string, data []byte) error {
	const attempts = 3
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := p.deps.Store.Put(ctx, key, contentType, data)
		if err == nil {
			return nil
		}
		lastErr = err
		if !sleepCtx(ctx, time.Duration(i+1)*200*time.Millisecond) {
			return lastErr
		}
	}
	return lastErr
}

// ----------------------------------------------------------------------------
// 小工具
// ----------------------------------------------------------------------------

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// extForContentType 由 MIME 类型推扩展名（只用于拼 object_key，不影响解码）。
func extForContentType(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/heic", "image/heif":
		return ".heic"
	case "image/png":
		return ".png"
	default:
		return ".png"
	}
}

// filenameFor 给 multipart 的文件字段造一个文件名。
// Python 那边只拿它认扩展名，所以取 object_key 的最后一段就够了。
func filenameFor(objectKey, contentType string) string {
	name := objectKey
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if name == "" {
		name = "image" + extForContentType(contentType)
	}
	return name
}

// targetSize 算「补白边到这个比例、最长边不超过 maxDimension」的目标像素。
// 解析不出来时退回一个正方形，绝不返回 0 —— size 传 "0x0" 上游会直接 400。
func targetSize(ratio dkdomain.Ratio, maxDimension int) (int, int) {
	if maxDimension <= 0 {
		maxDimension = dkdomain.DefaultMaxDimension
	}
	w, h, err := ratio.TargetSize(maxDimension)
	if err != nil || w <= 0 || h <= 0 {
		return maxDimension, maxDimension
	}
	return w, h
}

// intOrTarget 取产物记下来的实际像素；没有就按比例算一个。
// wantWidth=true 取宽，否则取高。
func intOrTarget(v *int, ratio dkdomain.Ratio, maxDimension int, wantWidth bool) int {
	if v != nil && *v > 0 {
		return *v
	}
	w, h := targetSize(ratio, maxDimension)
	if wantWidth {
		return w
	}
	return h
}

// truncateForLog 把上游原文压到能进 last_error_message 又不刷屏的长度。
func truncateForLog(s string) string {
	const maxLen = 2000
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…(truncated)"
}

// ---------------------------------------------------------------------------
// 比例：靠提示词说，不靠 size
// ---------------------------------------------------------------------------

// ratioInstructions 每个比例对应的一句中文要求。
//
// 为什么要有这个（2026-08-14 实测结论）：
//
//	设计定型里写着「给输入图补白边到目标比例，网关会跟着输入图的比例出图 ——
//	这是控制出图比例的唯一手段」。**这句话被实测推翻了**：
//	输入一张 554×554 的正方图、请求 1:1，拿回来的是 1087×1447（3:4）。
//
//	原因是控制比例的三条路我们只走通了半条：
//	  ① 补白边 —— 做了，但单靠它不管用（就是上面那次）；
//	  ② size 参数 —— 发的是补边后的真实像素（554x554），不是接口认的标准尺寸，
//	     等于没发；
//	  ③ 提示词里写版式 —— **完全没做**。
//
//	monica 的印象是「提示词优先级很高」。这一条正好也是三条里唯一
//	不依赖「OpenAI 支不支持 3:4 / 16:9 这类非标准比例」的 —— 模型是照着描述画，
//	不是从固定尺寸列表里挑。
//
// 措辞上刻意同时给「比例数字」和「一个人话的形状」：只说 "3:4" 时模型容易忽略，
// 加上「竖版」这种词之后指令性强得多。
var ratioInstructions = map[dkdomain.Ratio]string{
	"1:1":  "画面必须是正方形构图，宽高比严格 1:1。",
	"3:4":  "画面必须是竖版构图，宽高比严格 3:4（高比宽长）。",
	"4:3":  "画面必须是横版构图，宽高比严格 4:3（宽比高长）。",
	"16:9": "画面必须是宽幅横版构图，宽高比严格 16:9。",
	"9:16": "画面必须是竖长条构图，宽高比严格 9:16（手机全屏那种比例）。",
}

// withRatioInstruction 在提示词末尾追加一句比例要求。
//
// ⚠ **只作用于发给模型的那一份**，不改落库的快照：
// designkit_job_items.prompt_text 存的仍是运营写的原文。
// 两者混同的话，历史记录里每条词后面都挂着一句系统加的话，
// 运营会以为是自己写的，重试和复制时又会被追加第二遍。
//
// 比例不在表里（配置里加了新比例但这里没跟上）时原样返回 ——
// 宁可这一张比例不受控，也不要拼一句半通不通的话进去。
func withRatioInstruction(prompt string, ratio dkdomain.Ratio) string {
	instruction, ok := ratioInstructions[ratio]
	if !ok {
		return prompt
	}
	prompt = stripRatioClauses(strings.TrimSpace(prompt))
	if prompt == "" {
		return instruction
	}
	return prompt + "\n\n" + instruction
}

// ---------------------------------------------------------------------------
// 先清掉提示词里原有的比例说法，再追加
// ---------------------------------------------------------------------------

// ⚠ 为什么必须清（2026-08-14 monica 发现）：
//
//	比例是在**第三步**选的，而提示词在**第二步**就定了。灵感库里那些电商主图模板
//	本身满是「方形1:1构图」「square composition」这类话，AI 合成时会照抄进来。
//	运营接着在第三步选了 16:9，我们再追加一句「宽高比严格 16:9」——
//	于是同一条提示词里出现两个互相矛盾的比例，**谁赢纯粹碰运气**。
//
//	实例（她贴的原文，结尾）：
//	  「…商业产品摄影质感，高分辨率，方形1:1构图，无文字、无水印、无边框、无多余元素。」
//	  运营选 16:9 → 追加「画面必须是宽幅横版构图，宽高比严格 16:9。」
//
//	合成那一步已经明确要求模型不要写比例（prompt_suggest.go），但**不能只靠它**：
//	模型不一定听，而且运营也可能从灵感库直接拿一条带比例的词来用。
//	所以发出去之前再清一道。

// ratioTokenPattern 形如 1:1、16 : 9、3：4（全角冒号）的比例记号。
var ratioTokenPattern = regexp.MustCompile(`\d{1,2}\s*[:：]\s*\d{1,2}`)

// ratioShapePattern 描述画面形状的词。
var ratioShapePattern = regexp.MustCompile(`正方形|方形|竖版|横版|竖构图|横构图|竖幅|横幅|(?i:square|portrait|landscape)`)

// ratioContextPattern 「在谈画面本身」的词。
//
// ⚠ 必须跟形状词**同时出现**才算比例说法。只看形状词会误伤：
// 「正方形包装盒」「方形托盘」是在描述**商品**，清掉就把商品说错了。
var ratioContextPattern = regexp.MustCompile(`构图|画幅|比例|画面|(?i:composition|aspect|ratio|framing)`)

// stripRatioClauses 按分句清掉在讲比例的那些片段。
//
// 中文提示词是逗号流水句，所以按标点切片、逐片判断、再拼回去 ——
// 整句正则替换会把「，，」这种断口留在原地。
//
// 判据（满足其一就丢掉这一片）：
//   - 含比例记号（1:1、16:9…）
//   - 同时含「形状词」和「在谈画面的词」（正方形 + 构图）
func stripRatioClauses(prompt string) string {
	if prompt == "" {
		return ""
	}
	// 切分时保留分隔符，这样重组后标点不会丢。
	parts := splitKeepingSeparators(prompt)
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		body := strings.TrimSpace(strings.Trim(part, "，。；、,;.\n "))
		if body != "" && isRatioClause(body) {
			continue
		}
		kept = append(kept, part)
	}
	out := strings.TrimSpace(strings.Join(kept, ""))
	// 清完可能在结尾留下孤零零的标点。
	out = strings.TrimRight(out, "，。；、,;. \n")
	if out == "" {
		return ""
	}
	return out + "。"
}

func isRatioClause(clause string) bool {
	if ratioTokenPattern.MatchString(clause) {
		return true
	}
	return ratioShapePattern.MatchString(clause) && ratioContextPattern.MatchString(clause)
}

// splitKeepingSeparators 按中英文逗号/句号/分号/顿号/换行切片，**保留分隔符**。
func splitKeepingSeparators(s string) []string {
	out := make([]string, 0, 32)
	start := 0
	for i, r := range s {
		switch r {
		case '，', '。', '；', '、', ',', ';', '.', '\n':
			out = append(out, s[start:i+len(string(r))])
			start = i + len(string(r))
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
