export const MAX_UPLOADS = 4;
export const MAX_UPLOAD_BYTES = 20 * 1024 * 1024;
export const ACCEPTED_IMAGE_TYPES = new Set([
  'image/png', 'image/jpeg', 'image/webp', 'image/heic', 'image/heif',
]);
// Safari/Chrome 对 HEIC 常给空 MIME，退回按扩展名判断
export const ACCEPTED_IMAGE_EXTS = /\.(png|jpe?g|webp|heic|heif)$/i;

export function isAcceptedImageFile(file) {
  if (file.type) return ACCEPTED_IMAGE_TYPES.has(file.type);
  return ACCEPTED_IMAGE_EXTS.test(file.name || '');
}

// 比例清单以后端 /api/web/size-presets 为准（后端同时用它做校验，避免两边漂移）。
// 这里的初值只是接口还没返回时的兜底，拉到之后会被整体替换。
export const SIZE_OPTIONS = [
  { value: '1024x1024', label: '方图', ratio: '1:1', usage: '淘宝/京东主图' },
  { value: '1024x1360', label: '竖图', ratio: '3:4', usage: '详情页、小红书' },
  { value: '1024x1280', label: '竖图', ratio: '4:5', usage: '社媒信息流' },
  { value: '1024x1536', label: '长竖图', ratio: '2:3', usage: '长图详情' },
  { value: '1024x1824', label: '超竖图', ratio: '9:16', usage: '抖音/快手封面' },
  { value: '1536x1024', label: '横图', ratio: '3:2', usage: '场景与横幅' },
  { value: '1360x1024', label: '横图', ratio: '4:3', usage: 'PC 端banner' },
  { value: '1824x1024', label: '超横图', ratio: '16:9', usage: '视频封面' },
  { value: 'auto', label: '自动', ratio: '自适应', usage: '交给模型决定' },
];

export function applySizePresets(presets) {
  if (!Array.isArray(presets) || !presets.length) return;
  SIZE_OPTIONS.length = 0;
  for (const item of presets) {
    if (item && item.value) SIZE_OPTIONS.push(item);
  }
}

export const QUALITY_OPTIONS = [
  { value: 'auto', label: '自动' },
  { value: 'high', label: '高' },
  { value: 'medium', label: '中' },
  { value: 'low', label: '低' },
];

export function visibleUploads(state) {
  return state.uploads.filter((item) => item.status !== 'removed');
}

export function uploadedItems(state) {
  return state.uploads.filter((item) => item.status === 'uploaded' && item.id != null);
}

export function pendingUploadItems(state) {
  return state.uploads.filter((item) => item.status === 'queued' || item.status === 'uploading');
}

export function failedUploadItems(state) {
  return state.uploads.filter((item) => item.status === 'failed');
}

export function selectedTemplateValue(variable, values) {
  return values[variable.name] ?? variable.default ?? '';
}

// 只做形状检查，不要求必须在预设清单里：管理员可以给模板设一个合法但不在
// 推荐清单里的尺寸，用「在不在清单里」当闸门会把它静默丢掉、退回 1:1
export function isUsableSize(value) {
  const text = String(value || '');
  return text === 'auto' || /^\d{3,4}x\d{3,4}$/.test(text);
}

export function applyTemplateDefaults(state, template) {
  const params = template?.default_params || {};
  if (isUsableSize(params.size)) state.size = params.size;
  if (params.n != null) state.n = Math.max(1, Math.min(4, Number.parseInt(params.n, 10) || 1));
  if (QUALITY_OPTIONS.some((option) => option.value === params.quality)) state.quality = params.quality;

  state.varValues = {};
  for (const variable of template?.variables || []) {
    if (variable.type === 'select' && Array.isArray(variable.options) && variable.options.length) {
      state.varValues[variable.name] = variable.options.includes(variable.default)
        ? variable.default
        : variable.options[0];
    } else if (variable.default != null) {
      state.varValues[variable.name] = variable.default;
    }
  }
}

export function configSignature(state) {
  // 用于判断「失败之后配置有没有被改过」。后端的 /retry 会原样重跑旧任务的
  // 提示词与参数，所以只有配置一字未改时重试才符合用户预期。
  return JSON.stringify({
    template: state.selected === null ? 'free' : (state.selected?.id ?? null),
    prompt: state.selected === null ? (state.freePrompt || '').trim() : '',
    variables: state.varValues || {},
    extra: (state.extra || '').trim(),
    uploads: uploadedItems(state).map((item) => item.id).sort(),
    // 正在上传的图也要算进去：否则刚拖进一张图、还没传完时会被判成「配置没变」，
    // 点下去就用不含新图的旧参数重跑了
    pendingUploads: pendingUploadItems(state).length,
    n: state.n,
    size: state.size,
    quality: state.quality,
  });
}

export function deriveSubmitState(state) {
  if (state.submitting) {
    return { canSubmit: false, action: 'submit', label: '正在创建任务…', reason: '正在提交生成任务' };
  }

  if (state.job?.status === 'pending' || state.job?.status === 'processing') {
    return { canSubmit: false, action: 'submit', label: '任务生成中…', reason: '当前任务完成后可以再次生成' };
  }

  // 失败后配置没动过才提供「重试旧任务」；一旦改了配置就落到下面的常规校验，
  // 按新配置创建新任务，否则用户改了模板/张数却仍按旧参数出图。
  if (
    state.job?.status === 'failed'
    && state.submittedSignature
    && state.submittedSignature === configSignature(state)
  ) {
    return { canSubmit: true, action: 'retry', label: '重新生成', reason: '用相同配置重试这次失败的任务' };
  }

  if (state.selected === undefined) {
    return { canSubmit: false, action: 'submit', label: '请选择生成方式', reason: '请选择一个模板或自由模式' };
  }

  if (state.selected === null && !state.freePrompt.trim()) {
    return { canSubmit: false, action: 'submit', label: '请填写画面描述', reason: '自由模式需要填写画面描述' };
  }

  const pending = pendingUploadItems(state);
  if (pending.length) {
    return { canSubmit: false, action: 'submit', label: '等待商品图上传', reason: `还有 ${pending.length} 张图片正在上传` };
  }

  const uploaded = uploadedItems(state);
  if (state.selected?.requires_input_image && !uploaded.length) {
    const failed = failedUploadItems(state);
    return {
      canSubmit: false,
      action: 'submit',
      label: '请先上传商品图',
      reason: failed.length ? '请重试或移除上传失败的图片' : '当前模板需要至少一张商品图',
    };
  }

  for (const variable of state.selected?.variables || []) {
    const value = selectedTemplateValue(variable, state.varValues);
    if (variable.required && !String(value).trim()) {
      const name = variable.label || variable.name;
      return { canSubmit: false, action: 'submit', label: `请填写${name}`, reason: `模板必填项“${name}”尚未填写` };
    }
  }

  return {
    canSubmit: true,
    action: 'submit',
    label: `生成 ${state.n} 张商品图`,
    reason: `${SIZE_OPTIONS.find((item) => item.value === state.size)?.ratio || state.size} · ${state.quality === 'high' ? '高画质' : '已选择画质'}`,
  };
}

export function buildGenerationPayload(state) {
  return {
    template_id: state.selected ? state.selected.id : null,
    prompt: state.selected === null ? state.freePrompt.trim() : null,
    variables: { ...state.varValues },
    extra_instructions: state.extra.trim() || null,
    upload_ids: uploadedItems(state).map((item) => item.id),
    n: state.n,
    size: state.size,
    quality: state.quality,
  };
}
