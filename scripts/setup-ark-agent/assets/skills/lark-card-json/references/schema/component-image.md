# 图片 Image

- 何时使用：展示 / 图片
- 主要用途：图片组件通过 `img_key` 展示单张图片，可设置标题、替代文本、圆角、裁剪方式、大小和预览行为。

## 组件介绍

图片组件通过 `img_key` 展示单张图片，可设置标题、替代文本、圆角、裁剪方式、大小和预览行为。

## 字段说明

```ts
// import { PlainText } from '../../../config/shared-primitives/plain-text'
// import { BaseText } from '../../../config/shared-primitives/base-text'

const Img = z.object({
  alt: PlainText.describe('图片替代说明'),
  img_key: z.string().describe('图片 key'),
  tag: z.literal('img').describe('组件标签，固定为 img').optional(),
  compact_width: z.boolean().describe('是否使用紧凑宽度').optional(),
  corner_radius: z.string().describe('圆角大小').optional(),
  custom_width: z.number().describe('自定义宽度').optional(),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
  fallback_img_key: z.string().describe('图片加载失败时的兜底图片 key').optional(),
  img_id: z.string().describe('图片资源 ID').optional(),
  margin: z.string().describe('外边距').optional(),
  mode: z.enum(['fit_horizontal', 'crop_center', 'large', 'medium', 'small', 'tiny', 'stretch', 'big']).describe('展示模式。fit_horizontal=适宽；crop_center=居中裁剪；large=大；medium=中；small=小；tiny=极小；stretch=拉伸；big=大尺寸').optional(),
  preview: z.boolean().describe('是否允许点击预览').optional(),
  scale_type: z.enum(['fit_horizontal', 'crop_center', 'crop_top']).describe('图片缩放或裁剪方式。fit_horizontal=适宽；crop_center=居中裁剪；crop_top=顶部裁剪').optional(),
  size: z.string().describe('尺寸').optional(),
  title: BaseText.describe('标题').optional(),
  transparent: z.boolean().describe('是否透明底色').optional(),
})
```


## 注意事项

- 图片必须使用上传后得到的 `img_key`，不能直接写图片 URL。
- `alt` 用于图片说明和不可见场景。
- 有整卡跳转时，如不希望图片抢占点击预览，可关闭 `preview`。
