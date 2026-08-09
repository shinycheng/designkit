// 本地导出平台尺寸：完全在浏览器里裁切/补边，**不调用生图接口、不花钱**。
//
// 为什么值得单独做：运营拿到一张满意的图后，往往还要为不同平台各出一版尺寸。
// 用生图去凑尺寸既慢又贵，而且每换一次比例画面就变了；本地裁一刀反而是所见即所得。

export const PLATFORM_SIZES = [
  { key: 'taobao', label: '淘宝/京东主图', width: 800, height: 800, note: '1:1' },
  { key: 'tmall', label: '天猫主图', width: 1000, height: 1000, note: '1:1' },
  { key: 'detail', label: '详情页', width: 750, height: 1000, note: '3:4' },
  { key: 'xhs', label: '小红书', width: 1080, height: 1440, note: '3:4' },
  { key: 'xhs_square', label: '小红书方图', width: 1080, height: 1080, note: '1:1' },
  { key: 'douyin', label: '抖音/快手封面', width: 1080, height: 1920, note: '9:16' },
  { key: 'banner', label: 'PC 横幅', width: 1920, height: 1080, note: '16:9' },
];

export const FIT_MODES = [
  {
    key: 'cover',
    label: '裁切填满',
    hint: '铺满整个画面，超出的边会被裁掉。适合主图。',
  },
  {
    key: 'contain',
    label: '补白边',
    hint: '完整保留原图，两侧补白边。透明底的图仍保留透明。适合怕裁到商品的场景。',
  },
];

/**
 * 把一张已加载的图片按目标尺寸绘制成 Blob。
 * @param {HTMLImageElement} image 已 decode 完成的图片
 * @param {{width:number,height:number}} target 目标像素尺寸
 * @param {'cover'|'contain'} mode 填充方式
 * @param {string} padColor 补白边时的底色
 */
export function renderToBlob(image, target, mode, padColor = '#ffffff') {
  const canvas = document.createElement('canvas');
  canvas.width = target.width;
  canvas.height = target.height;
  const ctx = canvas.getContext('2d');
  if (!ctx) throw new Error('当前浏览器不支持画布导出');

  ctx.imageSmoothingEnabled = true;
  ctx.imageSmoothingQuality = 'high';

  const sourceW = image.naturalWidth || image.width;
  const sourceH = image.naturalHeight || image.height;
  if (!sourceW || !sourceH) throw new Error('图片尺寸读取失败');

  const scale = mode === 'cover'
    ? Math.max(target.width / sourceW, target.height / sourceH)
    : Math.min(target.width / sourceW, target.height / sourceH);
  const drawW = Math.round(sourceW * scale);
  const drawH = Math.round(sourceH * scale);
  const dx = Math.round((target.width - drawW) / 2);
  const dy = Math.round((target.height - drawH) / 2);

  if (mode === 'contain') {
    // 只填「补出来的那几条边」，不铺满整张画布——铺满会把商品本身的透明区域
    // 也涂成白色，等于把辛苦拿到的透明底毁掉。这样处理后：
    // 不透明的图和以前的结果一模一样，透明底的图保住透明、白边照样有。
    ctx.fillStyle = padColor;
    if (dx > 0) {
      ctx.fillRect(0, 0, dx, target.height);
      ctx.fillRect(dx + drawW, 0, target.width - dx - drawW, target.height);
    }
    if (dy > 0) {
      ctx.fillRect(0, 0, target.width, dy);
      ctx.fillRect(0, dy + drawH, target.width, target.height - dy - drawH);
    }
  }
  ctx.drawImage(image, dx, dy, drawW, drawH);

  return new Promise((resolve, reject) => {
    canvas.toBlob((blob) => {
      if (blob) resolve(blob);
      else reject(new Error('导出失败，请重试'));
    }, 'image/png');
  });
}

/** 载入图片；结果图和上传图都是同源的 /files/... 路径，不存在跨域污染画布的问题。 */
export function loadImage(url) {
  return new Promise((resolve, reject) => {
    const image = new Image();
    image.crossOrigin = 'anonymous';
    image.onload = () => resolve(image);
    image.onerror = () => reject(new Error('图片加载失败'));
    image.src = url;
  });
}
