export const MAX_UPLOADS = 4;
export const MAX_UPLOAD_BYTES = 20 * 1024 * 1024;
export const ACCEPTED_IMAGE_TYPES = new Set(['image/png', 'image/jpeg', 'image/webp']);

export const SIZE_OPTIONS = [
  { value: '1024x1024', label: '方图', ratio: '1:1', usage: '商品主图' },
  { value: '1536x1024', label: '横图', ratio: '3:2', usage: '场景与横幅' },
  { value: '1024x1536', label: '竖图', ratio: '2:3', usage: '详情与社媒' },
  { value: 'auto', label: '自动', ratio: '自适应', usage: '交给模型选择' },
];

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

export function applyTemplateDefaults(state, template) {
  const params = template?.default_params || {};
  if (SIZE_OPTIONS.some((option) => option.value === params.size)) state.size = params.size;
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

export function deriveSubmitState(state) {
  if (state.submitting) {
    return { canSubmit: false, action: 'submit', label: '正在创建任务…', reason: '正在提交生成任务' };
  }

  if (state.job?.status === 'pending' || state.job?.status === 'processing') {
    return { canSubmit: false, action: 'submit', label: '任务生成中…', reason: '当前任务完成后可以再次生成' };
  }

  if (state.job?.status === 'failed') {
    return { canSubmit: true, action: 'retry', label: '重新生成', reason: '保留当前配置并重试失败任务' };
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
