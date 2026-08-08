"""首次启动时的种子数据：管理员账号 + 示例提示词模板。

示例模板只在「模板表完全为空」时写入，不会覆盖你后续导入的正式提示词库。
"""
import logging

from sqlalchemy.orm import Session

from .models import PromptCategory, PromptTemplate, User
from .security import hash_password

logger = logging.getLogger("designkit.seed")

DEFAULT_ADMIN_USERNAME = "admin"
DEFAULT_ADMIN_PASSWORD = "admin123456"

SEED_CATEGORIES = ["白底图", "场景图", "节日营销", "创意背景"]

SEED_TEMPLATES = [
    {
        "name": "纯白背景商品图",
        "category": "白底图",
        "description": "电商平台主图标配：纯白无缝背景 + 柔和影棚光，突出商品本身。",
        "prompt_template": (
            "Professional e-commerce product photograph of the product from the uploaded image, "
            "placed on a pure seamless white background, soft studio lighting, gentle natural shadow "
            "under the product, centered composition, ultra sharp details, 8k quality, no text, no watermark."
        ),
        "variables": [],
        "default_params": {"size": "1024x1024", "n": 1, "quality": "high"},
        "requires_input_image": True,
    },
    {
        "name": "自定义场景商品图",
        "category": "场景图",
        "description": "把商品放进你指定的场景里，可选整体风格。",
        "prompt_template": (
            "Place the product from the uploaded image into this scene: {scene}. "
            "Overall style: {style}. Keep the product exactly as it looks in the original photo, "
            "photorealistic lighting and shadows that match the scene, professional e-commerce "
            "photography, high resolution, no text, no watermark."
        ),
        "variables": [
            {"name": "scene", "label": "场景描述", "type": "text", "default": "", "required": True,
             "placeholder": "例如：现代简约客厅的木质茶几上"},
            {"name": "style", "label": "整体风格", "type": "select", "default": "明亮清新",
             "options": ["明亮清新", "高级质感", "温馨居家", "户外自然", "极简性冷淡"], "required": False},
        ],
        "default_params": {"size": "1024x1024", "n": 1, "quality": "high"},
        "requires_input_image": True,
    },
    {
        "name": "大理石台面质感图",
        "category": "场景图",
        "description": "常用于美妆、香水、家居小件：大理石台面 + 柔光，高级感。",
        "prompt_template": (
            "The product from the uploaded image standing on a luxurious white marble countertop, "
            "soft diffused daylight from the left, a few subtle green plant leaves blurred in the "
            "background, premium minimalist aesthetic, photorealistic, high-end e-commerce product "
            "photography, no text, no watermark."
        ),
        "variables": [],
        "default_params": {"size": "1024x1024", "n": 1, "quality": "high"},
        "requires_input_image": True,
    },
    {
        "name": "节日促销氛围图",
        "category": "节日营销",
        "description": "为大促/节日做的氛围场景，节日和主色调可选。",
        "prompt_template": (
            "Festive e-commerce marketing photo of the product from the uploaded image, themed for "
            "{holiday}, decorated background with tasteful {color} tones, celebratory but premium "
            "atmosphere, product remains the clear hero of the image, photorealistic lighting, "
            "high resolution, no text, no watermark."
        ),
        "variables": [
            {"name": "holiday", "label": "节日", "type": "select", "default": "双十一大促",
             "options": ["双十一大促", "618 大促", "圣诞节", "新年/春节", "情人节", "母亲节", "黑色星期五"],
             "required": True},
            {"name": "color", "label": "主色调", "type": "select", "default": "red and gold",
             "options": ["red and gold", "green and red", "pink and white", "blue and silver"],
             "required": False},
        ],
        "default_params": {"size": "1024x1024", "n": 2, "quality": "high"},
        "requires_input_image": True,
    },
    {
        "name": "户外自然光场景",
        "category": "场景图",
        "description": "适合户外用品、运动、服饰配件：真实户外自然光。",
        "prompt_template": (
            "The product from the uploaded image photographed outdoors in {scene}, golden hour "
            "natural sunlight, shallow depth of field with softly blurred background, lifestyle "
            "e-commerce photography, photorealistic, high resolution, no text, no watermark."
        ),
        "variables": [
            {"name": "scene", "label": "户外场景", "type": "select", "default": "a sunny park lawn",
             "options": ["a sunny park lawn", "a sandy beach at sunset", "a mountain camping site",
                         "a cozy garden table", "a forest trail"],
             "required": True},
        ],
        "default_params": {"size": "1536x1024", "n": 1, "quality": "high"},
        "requires_input_image": True,
    },
    {
        "name": "渐变纯色创意背景",
        "category": "创意背景",
        "description": "社媒风格的渐变纯色背景 + 几何光影，适合数码、快消品。",
        "prompt_template": (
            "The product from the uploaded image floating in front of a smooth {color} gradient "
            "studio background, minimal geometric soft shadows, trendy social-media product "
            "aesthetic, dramatic rim lighting, photorealistic, high resolution, no text, no watermark."
        ),
        "variables": [
            {"name": "color", "label": "背景色系", "type": "select", "default": "peach to coral",
             "options": ["peach to coral", "mint to teal", "lavender to violet", "sky blue to indigo",
                         "cream to amber"],
             "required": True},
        ],
        "default_params": {"size": "1024x1536", "n": 1, "quality": "high"},
        "requires_input_image": True,
    },
]


def seed(db: Session) -> None:
    if db.query(User).count() == 0:
        db.add(
            User(
                username=DEFAULT_ADMIN_USERNAME,
                password_hash=hash_password(DEFAULT_ADMIN_PASSWORD),
                display_name="管理员",
                role="admin",
                must_change_password=True,  # 首次登录强制改密
            )
        )
        db.commit()
        logger.warning(
            "=" * 60 + "\n已创建初始管理员账号 %s / %s\n**这是公开的默认口令，登录后必须立即修改密码！**\n" + "=" * 60,
            DEFAULT_ADMIN_USERNAME, DEFAULT_ADMIN_PASSWORD,
        )

    if db.query(PromptTemplate).count() == 0:
        categories = {}
        for i, name in enumerate(SEED_CATEGORIES):
            c = db.query(PromptCategory).filter(PromptCategory.name == name).first()
            if c is None:
                c = PromptCategory(name=name, sort=i)
                db.add(c)
                db.flush()
            categories[name] = c
        for i, item in enumerate(SEED_TEMPLATES):
            db.add(
                PromptTemplate(
                    name=item["name"],
                    category_id=categories[item["category"]].id,
                    description=item["description"],
                    prompt_template=item["prompt_template"],
                    variables=item["variables"],
                    default_params=item["default_params"],
                    requires_input_image=item["requires_input_image"],
                    sort=i,
                )
            )
        db.commit()
        logger.info("已写入 %d 个示例提示词模板", len(SEED_TEMPLATES))
