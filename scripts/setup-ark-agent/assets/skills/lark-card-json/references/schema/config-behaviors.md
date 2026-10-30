# Interactive Behaviors

- 何时使用：交互组件（按钮 / 输入框 / 各类选择器 / 交互容器）的 `behaviors` 字段，涉及回调、打开链接、事件即时响应、客户端消息、交互 toast。
- 主要用途：本文件用 demo 汇总全部行为类型，命中后只读本文件即可，无需再加载同级子目录。

## 总览

- 组件的 `behaviors` 是一个**数组**，每个元素是下面任意一种行为对象（即其它组件 zod 里引用的 `BaseBehavior`）；可挂多个，通常一个。
- 用 `type` 区分行为：`callback` / `client_message` / `event` / `open_url`。注意「打开链接」与「模板打开链接」的 `type` **都是 `open_url`**，靠字段区分。
- 下面每段 demo 用 `//` 注释标注字段的用途与取值范围；**最终输出必须是合法 JSON**（删注释、去尾逗号），再按本目录的字段定义校验（语法可用 `python3 -m json.tool` 过一遍）。

## 挂载位置：完整卡片示例

行为挂在交互组件的 `behaviors` 数组里。其余各节只给出数组里的**单个行为对象**。

```json5
{
  "schema": "2.0", // 卡片 JSON 结构版本，使用 2.0 结构必须显式声明（默认 1.0）
  "body": {
    "elements": [
      {
        "tag": "button", // 仅交互组件（按钮 / 输入框 / 选择器 / 交互容器等）能挂 behaviors
        "text": { "tag": "plain_text", "content": "点击" },
        "behaviors": [
          // behaviors：数组，元素为下列任意行为对象（BaseBehavior）
          {
            "type": "open_url", // 行为类型
            "default_url": "https://www.feishu.cn" // 兜底跳转地址
          }
        ]
      }
    ]
  }
}
```

## callback —— 回传交互

```json5
{
  "type": "callback", // 行为类型，固定值 "callback"
  "value": {
    // 回传交互数据。用途：点击后把数据回传给你的交互处理服务（卡片回调）。
    // 取值范围：object 类型；开放平台 SDK 仅支持 object 类型的回传数据。可选。
    "key_1": "value_1"
  }
}
```

## client_message —— 客户端消息

```json5
{
  "type": "client_message", // 行为类型，固定值 "client_message"
  "client_channel": "my_channel", // 客户端通道名称。用途：指定消息下发到哪个客户端通道。取值范围：string，必填。
  "value": {
    // 透传值。取值范围：object，可选。
    "key_1": "value_1"
  }
}
```

## event —— 事件 + 即时响应

```json5
{
  "type": "event", // 行为类型，固定值 "event"
  "value": {
    // 回传值。取值范围：object，可选。
    "event": "like"
  },
  "instant_react_toast": {
    // 交互后即时弹出的 toast（结构见末节 ActionToast）。取值范围：ActionToast 对象，可选。
    "type": "success", // info=信息 / success=成功 / error=错误 / warning=警告。可选
    "content": "操作成功" // toast 文本。string，可选
  },
  "instant_react_card": {
    // 交互后即时响应、替换当前卡片的整张卡片。取值范围：一张完整 Card JSON，可选。
    "schema": "2.0",
    "body": { "elements": [ /* 新卡片的组件 */ ] }
  }
}
```

## open_url —— 打开链接（多端跳转）

```json5
{
  "type": "open_url", // 行为类型，固定值 "open_url"
  "default_url": "https://www.feishu.cn",        // 默认/兜底跳转 URL，未命中具体端时使用。string，可选（建议始终提供）
  "pc_url": "https://www.feishu.cn/pc",          // PC 端跳转 URL。string，可选
  "ios_url": "https://www.feishu.cn/ios",        // iOS 端跳转 URL。string，可选
  "android_url": "https://www.feishu.cn/android",// Android 端跳转 URL。string，可选
  "harmony_url": "https://www.feishu.cn/harmony" // HarmonyOS 端跳转 URL。string，可选
}
```

## open_url（模板）—— 模板化多端链接

```json5
{
  "type": "open_url", // 行为类型，固定值 "open_url"（与上面的多端跳转同 type，靠字段区分）
  "multi_url": "${detail_url}" // 多端 URL 对象。用途：模板卡片里用变量引用一个多端 URL 对象。取值范围：string（通常是模板变量，如 ${...}），可选
}
```

## ActionToast —— 交互后 toast

被 `event` 行为的 `instant_react_toast` 引用，描述交互后弹出的提示内容。

```json5
{
  "type": "success", // toast 类型。取值范围：info=信息 / success=成功 / error=错误 / warning=警告。可选
  "content": "操作成功", // 文本内容。string，可选
  "i18n": {
    // 多语言文本映射。取值范围：object，key 为语言码、value 为该语言文本，均可选。
    // 支持语言码：zh_cn / zh_hk / zh_tw / en_us / ja_jp / ko_kr / de_de / es_es / fr_fr / it_it / pt_br / ru_ru / th_th / vi_vn / id_id / ms_my / hi_in
    "zh_cn": "操作成功",
    "en_us": "Success"
  }
}
```

## 注意事项

- `behaviors` 是数组，可在一个组件上挂多个行为，通常一个。
- `type` 取值：`callback` / `client_message` / `event` / `open_url`。「打开链接」与「模板打开链接」同为 `open_url`，靠 `default_url` 等多端字段 与 `multi_url` 区分。
- 其它组件 zod 里以 `BaseBehavior` 引用本文件描述的行为联合：`// import { BaseBehavior } from '../../../config/behaviors'`，不再指向已合并的子目录。
- demo 里的 `//` 注释仅用于说明；最终输出必须是合法 JSON，按本目录的字段定义校验。
