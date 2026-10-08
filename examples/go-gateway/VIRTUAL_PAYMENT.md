# 个人主体虚拟支付：PDF 翻译

按 personal-virtual-payment skill 接入道具直购，不引入充值、余额或代币。仅使用现网 `env=0`。真机测试会实际扣费，由开发者人工操作。

## 业务链路

选 PDF → 微信登录 → Go 暂存 → Python 识别实际页数并检查引擎 → Go 固化订单并签名 → wx.requestVirtualPayment → 推送/查单确认支付 → 持久化翻译权益 → 幂等启动 Python → 原进度、预览、下载页面。

客户端支付成功不作为发货依据。页面退出后服务端继续处理，可从首页最近订单恢复。每页对应一件道具：16 页、单价 10 分即总价 160 分。页数、引擎、文件、商品和单价由服务端校验固化，客户端不能改价。

## 配置和部署

1. 将 `.env.virtualpay.example` 的条目加入现有被 Git 忽略的 `.env`，通过 `./start.sh` 启动。不要向聊天、日志、前端或仓库粘贴密钥。既有 WECHAT_APPSECRET/JWT_SECRET 仍必需，JWT_SECRET 重启应保持不变。
2. OfferID、现网 AppKey 来自虚拟支付基本配置。AppKey 不是小程序 AppSecret。配置已发布的两种道具 ID，整数分单价必须与后台一致；模板 10/50 分只是示例。现有 AppID 为 wx760bf26760f46645。
3. 配置消息推送 Token 和 43 位 EncodingAESKey，后台选安全加密模式。HTTPS 发货推送 URL 为 `https://你的域名/pay/notify`，支持 GET 校验和加密 XML/JSON POST，拒绝明文支付通知。相关后台入口若复用接收地址，Token/AESKey 必须相同。
4. iOS 配置小程序简称并开通 Apple IAP，真机微信 >=8.0.68；前端有版本和接口能力检查。
5. 小程序 config.js 的 baseUrl 改为 HTTPS 网关，配置 request/uploadFile/downloadFile 合法域名；OSS 域名列入下载合法域名。手机的 127.0.0.1 不是电脑。
6. 配置服务器出口 IP 白名单，确保能获取 stable access_token 并访问 xpay 查单、确认发货 API。session_key 只在服务端短期保存，下单前重新登录。
7. 两个服务都需更新并重启。Python 仅监听内网，其 API 无鉴权，绝不可直接暴露公网。Go 经 PDF2ZH_API_URL 调用。
8. 设置 VIRTUALPAY_DATA_DIR 为持久目录，保留 orders/uploads 并限制访问、加密备份；包含用户 PDF、openid、订单资料。Python 的 PDF2ZH_WORK_DIR 也要持久化，尤其 idempotency 子目录不可清理。
9. 参数齐全、准备联调后才将 VIRTUALPAY_ENABLED=true；缺参数拒绝启动，启用后旧 POST /api/translate 返回 402，防止绕过付费。关闭支付仅用于开发，真实模式小程序会提示尚未配置。

## 接口与确认规则

除通知外均需 Bearer JWT，且校验当前用户归属。

| 接口 | 用途 |
| --- | --- |
| GET /pay/config | 开关与单价，不含密钥 |
| POST /pay/order | multipart file + data；实际页数计费，返回 order/payData |
| GET /pay/orders | 当前用户订单 |
| GET /pay/orders/{id} | 单个订单 |
| POST /pay/orders/{id}/start | 已支付订单幂等启动 |
| POST /pay/query | JSON order_id；服务端查单 |
| GET/POST /pay/notify | URL 校验、加密验签、发货/退款通知 |

签名严格使用原始 JSON 字符串，不重新排序格式化。查单请求字段是 order_id 而非 out_trade_no。发货检查用户、单号、道具、数量、attach、平台单号和金额；平台单号不能用于其他订单。订单落盘失败不应答成功。启动时立即补查，随后每 5 分钟查单，前端查询有节流。

退款通知经查单确认后使权益失效，拒绝后续下载并尝试取消任务；已下载文件无法撤回。Android 主动退款通过 MP 后台，本项目不开放退款管理 API；iOS 用户向 Apple 申请。未实现投诉或 Apple 退款咨询事件，发布前须核对后台启用的额外事件并补齐运营处理。

已签发的 OSS 临时下载链接在到期前仍可能有效；退款不会立即撤销外部存储链接。需要立即撤销时，须改为网关鉴权下载或增加对象撤销策略。

## 上线阻断项和运行边界

- 单 Go 进程、单 Python 实例。支付目录不能共享给多个网关；扩容需事务数据库、分布式队列及跨进程唯一约束。
- Go 重启恢复订单和付费任务归属，未提交的已付费订单可补启动。Python 任务状态仍在内存，重启可能丢失进度/结果索引；持久幂等标记阻止同单静默重复翻译并返回 409，需要人工恢复结果或退款。**长期收费上线前必须补齐持久化任务恢复和失败补偿方案**。
- 引擎预检查不能保证翻译成功，余额、网络、内容仍可能失败，需客服退款/重试制度。
- 未实现暂存 PDF 自动清理、订单分页或全终端月累计额度监控；需留存策略、资源限额、告警与对账。单笔金额上限不是月额度控制。
- 查单字段/状态、加密通知、iOS 金额字段须与后台当前文档及真单响应核对；本地模拟不是平台验收。金额不符时拒绝发货，不为兼容未知响应放宽校验。

## 规则告知与真机验收

按所用 skill：个人主体全终端月支付限额 10 万元；Android 费率 1%、T+3 结算；iOS 12%、约 45–60 天结算。180 天以内退款返还手续费，超过 180 天不返还；Android 在 MP 后台退款，iOS 向 App Store 申请。次月 5 日后可申请上月腾讯技术服务费发票。以当前后台协议为准。

本人测试账号用一页 PDF 人工小额支付。记录业务/平台单号、原价/实付金额、回调应答、task_id，不记录密钥或 session_key。验证 Android/iOS（如开放）、页面退出恢复、重复通知、丢推送查单、退款后拒绝下载，最后与 MP 账单对账。逐项记录见 VIRTUAL_PAYMENT_ACCEPTANCE.md，不得提前勾选未验证项目。
