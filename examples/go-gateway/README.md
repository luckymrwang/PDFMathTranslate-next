# go-gateway — 微信小程序登录网关

纯 Go 标准库实现（无第三方依赖）的微信小程序登录网关，负责：

- 用 `wx.login` 返回的 `code` 调用微信 `code2session` 换取 `openid` / `session_key`
- 签发自实现的 HS256 JWT 会话 token（`session_key` 永不下发给客户端）
- 校验 Bearer token 的鉴权中间件，保护后续业务接口

未来可在此网关内再接入对 Python 翻译服务（`pdf2zh --api`）的转发、OSS、队列、计费等。

## 环境变量

| 变量 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `WECHAT_APPID` | 否 | `wx760bf26760f46645` | 小程序 AppID |
| `WECHAT_APPSECRET` | **是** | — | 小程序 AppSecret（机密，仅经环境变量传入） |
| `JWT_SECRET` | **是** | — | 签发 token 的 HMAC 密钥，至少 16 字节 |
| `PDF2ZH_API_URL` | 否 | `http://127.0.0.1:11008` | Python 翻译服务地址（预留） |
| `GATEWAY_ADDR` | 否 | `:8080` | 监听地址 |

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
| `POST` | `/api/translate` | `multipart/form-data`（`file` + `data` JSON），返回 `{ "id": "<taskID>" }` |
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

`data` 字段结构（透传给 Python API）：

```jsonc
{
  "lang_in": "en",
  "lang_out": "zh",
  "qps": 4,
  "pages": "1-5",
  "translate_engine_settings": { "translate_engine_type": "Google" }
}
```

小程序端提交示例：

```js
wx.uploadFile({
  url: 'https://你的域名/api/translate',
  filePath: tempFilePath,
  name: 'file',
  header: { Authorization: 'Bearer ' + wx.getStorageSync('token') },
  formData: { data: JSON.stringify({ lang_in: 'en', lang_out: 'zh' }) },
  success: ({ data }) => {
    const { id } = JSON.parse(data)
    // 之后用 id 轮询 /api/translate/{id} 或下载 /api/translate/{id}/dual
  },
})
```

> 注意：微信小程序端不支持 SSE，建议用 `GET /api/translate/{id}` 轮询进度；`/stream` 供 Web/服务端消费。

### `GET /health`

```json
{ "status": "ok" }
```

## 测试

```bash
go vet ./...
go test ./...   # token 签发/校验/防篡改/过期用例
```

