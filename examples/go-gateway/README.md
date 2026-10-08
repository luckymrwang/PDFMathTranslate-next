# go-gateway — 微信小程序登录网关

纯 Go 标准库实现（无第三方依赖）的微信小程序网关，负责：

- 用 `wx.login` 返回的 `code` 调用微信 `code2session` 换取 `openid` / `session_key`
- 签发自实现的 HS256 JWT 会话 token（`session_key` 永不下发给客户端）
- 校验 Bearer token 的鉴权中间件，保护后续业务接口

现已对接 Python 翻译 API、任务归属校验、可选 OSS 结果归档和个人主体虚拟支付。支付订单和付费任务归属可从磁盘恢复；普通任务归属和 Python 任务状态仍在内存中。尚未实现分布式队列或完整任务恢复。

虚拟支付配置见 [VIRTUAL_PAYMENT.md](VIRTUAL_PAYMENT.md)，逐项部署验收见 [VIRTUAL_PAYMENT_ACCEPTANCE.md](VIRTUAL_PAYMENT_ACCEPTANCE.md)。未配置时支付默认关闭；启用后普通提交接口拒绝绕过支付。

## 环境变量

| 变量 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `WECHAT_APPID` | 否 | `wx760bf26760f46645` | 小程序 AppID |
| `WECHAT_APPSECRET` | **是** | — | 小程序 AppSecret（机密，仅经环境变量传入） |
| `JWT_SECRET` | **是** | — | 签发 token 的 HMAC 密钥，至少 16 字节 |
| `PDF2ZH_API_URL` | 否 | `http://127.0.0.1:11008` | Python 翻译服务地址 |
| `GATEWAY_ADDR` | 否 | `:8080` | 监听地址 |

### 翻译引擎

Google、Bing 默认可用。下列环境变量配置后会向小程序开放对应大模型；API Key 只存于 Go 进程环境，不下发给小程序。`GET /api/engines` 返回当前可选名称。

| 小程序选项 | 必需变量 | 可选变量 |
| --- | --- | --- |
| GPT-6 | `OPENAI_API_KEY` | `OPENAI_MODEL`（默认 `gpt-6-luna`）、`OPENAI_BASE_URL` |
| DeepSeek-V4.1-Flash | `DEEPSEEK_API_KEY` | `DEEPSEEK_MODEL`（默认 `deepseek-flash`），官方地址 `https://api.deepseek.com/v1` |
| Gemini 2.5 Pro | `GEMINI_API_KEY` | `GEMINI_MODEL`（默认 `gemini-2.5-pro`） |
| Claude 3.7 Sonnet | `CLAUDE_CODE_PATH`（Python 服务可执行的 Claude Code CLI 路径） | `CLAUDE_CODE_MODEL` |

模型名称与实际供应商版本可能发生变化，生产环境应核对对应模型 ID。Go 会覆盖客户端传来的 `translate_engine_settings`，避免客户端注入密钥或选择未开放模型。

> 安全：`WECHAT_APPSECRET` 与 `JWT_SECRET` 绝不要写进代码或日志；`session_key` 只保存在服务端。

### 对象存储（可选，用于结果转存 + 临时下载链接）

配置后启用 `GET /api/translate/{id}/result`，网关会把翻译结果 PDF 转存到 S3 兼容存储，并返回预签名临时 URL（小程序直接下载，省网关带宽）。兼容 AWS S3 / 阿里云 OSS（S3 协议端点）/ MinIO / Cloudflare R2。四项必填齐全即自动启用，否则该接口返回 501。

| 变量 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `OSS_ENDPOINT` | 是 | — | 如 `https://oss-cn-hangzhou.aliyuncs.com` |
| `OSS_BUCKET` | 是 | — | 存储桶名 |
| `OSS_ACCESS_KEY` | 是 | — | AccessKeyID（机密） |
| `OSS_SECRET_KEY` | 是 | — | AccessKeySecret（机密） |
| `OSS_REGION` | 否 | `us-east-1` | 区域，如 `cn-hangzhou` |
| `OSS_PATH_STYLE` | 否 | `true` | path-style URL（MinIO 用 true；OSS 虚拟主机风格设 `false`） |
| `OSS_URL_TTL` | 否 | `3600` | 预签名链接有效期（秒） |

## 运行

```bash
export WECHAT_APPSECRET=你的小程序AppSecret
export JWT_SECRET=$(openssl rand -hex 32)
go run .
```

本地模型配置已保存在 Git 忽略的 `.env` 中。设置好微信登录所需环境变量后，用 `./start.sh` 启动即可加载该文件；直接 `go run .` 不会自动加载 `.env`。不要提交或分享 `.env`。

小程序标准翻译默认选择 Bing。翻译 API 默认 `skip_image_translation: true`：识别到的图片及图内文字保持原样，关闭图片 OCR，图外正文和图注继续翻译。该保护依赖版面识别，未识别成图片的区域仍可能按正文处理。

## 接口

### `POST /api/login`

```jsonc
// 请求
{ "code": "wx.login 返回的 code" }

// 响应
{ "token": "<jwt>", "expires_in": 604800, "openid": "o_xxx" }
```

小程序端：

```js
wx.login({
  success: ({ code }) => {
    wx.request({
      url: 'https://你的域名/api/login',
      method: 'POST',
      data: { code },
      success: ({ data }) => wx.setStorageSync('token', data.token),
    })
  },
})
```

### `GET /api/me`（需鉴权）

请求头携带 `Authorization: Bearer <token>`，返回当前用户：

```json
{ "openid": "o_xxx" }
```

### 翻译代理（均需鉴权，`Authorization: Bearer <token>`）

网关把请求转发给 Python 翻译服务（`PDF2ZH_API_URL`，即 `pdf2zh --api`），并按 `openid` 做任务归属校验——用户只能访问自己提交的任务。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/engines` | 返回可用引擎名称列表，不含密钥 |
| `POST` | `/api/translate` | `multipart/form-data`（`file` + `data` JSON，PDF 最大 50 MB），返回 `{ "id": "<taskID>" }` |
| `GET` | `/api/translate/{id}` | 查询任务状态 |
| `GET` | `/api/translate/{id}/stream` | SSE 实时进度 |
| `GET` | `/api/translate/{id}/mono` | 下载单语结果 PDF（经网关代理） |
| `GET` | `/api/translate/{id}/dual` | 下载双语结果 PDF（经网关代理） |
| `GET` | `/api/translate/{id}/result` | 转存结果到对象存储，返回预签名临时下载 URL（需配置 OSS） |
| `DELETE` | `/api/translate/{id}` | 取消任务 |

`GET /api/translate/{id}/result` 响应（任务完成后调用；首次调用触发转存，之后幂等返回新签名）：

```json
{
  "mono_url": "https://.../results/<openid>/<id>-mono.pdf?X-Amz-Signature=...",
  "dual_url": "https://.../results/<openid>/<id>-dual.pdf?X-Amz-Signature=...",
  "expires_in": 3600
}
```

`data` 字段结构（由 Go 校验并映射到 Python API）：

```jsonc
{
  "lang_in": "en",
  "lang_out": "zh",
  "qps": 4,
  "pages": "1-5",
  "engine": "Google"
}
```

小程序端提交示例：

```js
wx.uploadFile({
  url: 'https://你的域名/api/translate',
  filePath: tempFilePath,
  name: 'file',
  header: { Authorization: 'Bearer ' + wx.getStorageSync('token') },
  formData: { data: JSON.stringify({ lang_in: 'en', lang_out: 'zh', engine: 'Google' }) },
  success: ({ data }) => {
    const { id } = JSON.parse(data)
    // 之后用 id 轮询 /api/translate/{id} 或下载 /api/translate/{id}/dual
  },
})
```

> 注意：微信小程序端不支持 SSE，建议用 `GET /api/translate/{id}` 轮询进度；`/stream` 供 Web/服务端消费。

### 与小程序联调

1. 在本项目启动 Python 服务：`PDF2ZH_API_HOST=127.0.0.1 pdf2zh --api`（默认端口 `11008`）；不要把未鉴权的 Python API 直接暴露到公网。
2. 设置 `WECHAT_APPSECRET` 和至少 16 字节的 `JWT_SECRET` 后运行 `go run .`。
3. 小程序目录 `/Users/sino/Documents/www/bucket/wxapp/pdftranslate` 的 `config.js` 已默认指向本机 `http://127.0.0.1:8080`，供微信开发者工具联调；开发工具须临时关闭合法域名校验。
4. 真机及正式发布时，将小程序 `baseUrl` 改为可访问的 HTTPS 网关域名；在微信公众平台配置该域名为 request、uploadFile、downloadFile 合法域名。若启用 OSS，OSS 下载域名也须列入 downloadFile 合法域名。`127.0.0.1` 在手机上指手机自身，不能用于真机。

真实模式确认弹层由服务器读取 PDF 实际页数并返回订单金额，再调用 `wx.requestVirtualPayment`。只有服务端确认已支付才能开始翻译。演示模式仍为本地估算，不扣费。

### `GET /health`

```json
{ "status": "ok" }
```

## 测试

```bash
go vet ./...
go test -race ./...   # JWT、支付门禁、签名、幂等、订单持久化与并发
node virtualpay-miniapp.test.cjs /Users/sino/Documents/www/bucket/wxapp/pdftranslate
```

Python 支付接口测试在项目根目录使用已安装项目依赖的环境运行：`python -m unittest discover -s tests -p test_virtualpay_api.py -v`。测试均使用本地模拟，不进行真实支付。
