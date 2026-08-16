-- 「模特上身」提示词模板 ×5（决策 39 第 4 条，2026-08-16 已在 NAS 库执行过）。
-- source='designkit'，与 youmind 同步互不干扰（同步的 upsert 只按 source_ref 圈定）。
-- 重新部署到空库时执行本文件即可；uid 是一次性生成的 ULID，重复执行会撞唯一键（幂等靠 uid）。
-- 实测记录：条纹+文字 logo 的压力测试图，gpt-image-2 试穿保真度达标
-- （条纹间距、袖子无条纹、"MONI CO." 文字逐字正确），不需要引入专用 VTON 模型。
BEGIN;
INSERT INTO designkit_prompts (uid, category_id, title, body, variables, source, is_enabled, created_at, updated_at) VALUES ('3B5QXFTEZWED3F48YAW8DF98XA', 7, '模特上身 · 简约棚拍', '让一位符合商品风格的模特自然穿上图中这件服装，浅灰无缝纸背景棚拍，全身正面站姿，柔和影棚灯光，商业电商摄影质感，服装的颜色、图案、文字和 logo 必须与原图完全一致，不得改动，画面干净无文字无水印', '[]', 'designkit', true, now(), now());
INSERT INTO designkit_prompts (uid, category_id, title, body, variables, source, is_enabled, created_at, updated_at) VALUES ('QX791JFEZ33PWQAQGS5TKZ8V0D', 7, '模特上身 · 街拍日常', '让一位年轻模特穿上图中这件服装，城市街头日常抓拍风格，自然光，行走中的半身构图，背景虚化，服装的颜色、图案、文字和 logo 必须与原图完全一致，不得改动，真实生活感，无文字无水印', '[]', 'designkit', true, now(), now());
INSERT INTO designkit_prompts (uid, category_id, title, body, variables, source, is_enabled, created_at, updated_at) VALUES ('6ZZPCQYFAVY9ZXGVGSCNGZKNMB', 7, '模特上身 · 居家氛围', '让一位模特穿上图中这件服装，明亮温馨的居家场景，坐在沙发或窗边，午后自然光，放松惬意的氛围，服装的颜色、图案、文字和 logo 必须与原图完全一致，不得改动，无文字无水印', '[]', 'designkit', true, now(), now());
INSERT INTO designkit_prompts (uid, category_id, title, body, variables, source, is_enabled, created_at, updated_at) VALUES ('SZXY6XTT9QC6FNRY3BEF5F8C7G', 7, '模特上身 · 版型细节', '模特穿上图中这件服装后的躯干局部特写，突出面料纹理、缝线和版型垂坠感，柔和侧光，服装的颜色、图案、文字和 logo 必须与原图完全一致，不得改动，微距质感，无文字无水印', '[]', 'designkit', true, now(), now());
INSERT INTO designkit_prompts (uid, category_id, title, body, variables, source, is_enabled, created_at, updated_at) VALUES ('AXGYPAV535XT86Q0RJ3ADCMTHX', 7, '模特上身 · 背面展示', '让一位模特穿上图中这件服装并转身背对镜头，展示背面版型和细节，浅色影棚背景，全身构图，服装的颜色、图案、文字和 logo 必须与原图完全一致，不得改动，无文字无水印', '[]', 'designkit', true, now(), now());
COMMIT;
SELECT count(*) FROM designkit_prompts WHERE source='designkit';
