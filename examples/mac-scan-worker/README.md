# Mac 扫描件处理机

线上用户上传**纯扫描 PDF**（页面是图片、没有文字层）并选择 **GPT-5.6 / GPT-6** 时，
网关不会把订单交给 BabelDOC（它会报 `Scanned PDF detected` 并失败），而是放进扫描件队列。
这台 Mac 通过 HTTPS 主动拉取订单、处理完成后回传，网关立即交付给用户。
Mac 只需要访问外网，**不需要开放端口**。

Mac 用**长轮询**等订单：每次请求最多挂起 5 分钟，订单一进队列就立即返回。
01:00–07:00 每次长轮询后再休息 25 分钟（夜间订单最多晚约 25 分钟开始处理），
网关知道它在休息，不会误报离线。

处理流程（`scan_pipeline.py`）：

1. **OCR 与扶正**：`ocrmypdf --ocr-engine appleocr --force-ocr --rotate-pages`
   识别文字，同时把横躺、倒置的页面转正（带 /Rotate 标记的页面会先烘焙成图片）。
2. **补充识别**：Apple OCR 偶尔会漏掉整行，用 Tesseract 补上 Apple 没识别到的行。
3. **翻译**：每页把页面图片和编号后的 OCR 行发给视觉模型（GPT-5.6 订单用 `gpt-5.6-luna`，
   GPT-6 订单用 `gpt-6-luna`，与网关翻译同一引擎用的是同一个模型），
   模型对照图片纠正 OCR 错字，按段落分组翻译；漏掉的行会带着页面图片再确认一次。
   logo（含与标志排在一起的机构名、院系名）一律保持原样。调用时 `temperature=0`，减少每次输出的差异。
   按页缓存：重试时不重复调用；同一页面内容（同一模型、同一语言方向）的译文还会存到
   `WORK_DIR/_shared`，同一个文件再次下单直接复用，结果完全一致（30 天未使用自动清理）。
4. **原位替换**：用取样的背景色盖住原文，在原位置写入译文，保留签章、表格线和数字。
   生成双语对照版（左原文、右译文）和/或纯译文版，按订单的输出格式回传。

### 完整质检

每个扫描件订单都做：

1. **统一术语**：先通读全文抽取最多 80 个术语，整本使用同一译法。
2. **规则检查**：残留原文、译文被压缩或字号过小。
3. **视觉质检**：原页和译文页一起交给模型逐页检查（残留、遮挡、重叠、数字、漏译），只计“严重”问题。
4. **重做**：有问题的页面带着问题清单重新翻译一次，再质检。

仍有问题的页面 **少于 5 页**：直接交付，并把质检报告发给你；**达到 5 页**：暂停交付，
用户端显示“质量复核中”，通知你复核（之后每 6 小时提醒一次），用 `release` 放行。

## 订单如何流转

| 情况 | 结果 |
| --- | --- |
| 扫描件 + GPT-5.6/GPT-6 | 进入 Mac 队列，Mac 自动处理并交付，状态页显示处理进度 |
| 扫描件 + Bing/Google/DeepSeek | 仍走原流程（小程序会提示这些引擎不能识别图片）；失败后通知你，可用 `adopt` 交给 Mac |
| Mac 处理出错 | 自动重新排队，最多 3 次；仍失败则订单标记失败，提示用户联系客服，并通知你 |
| 模型额度用尽 / 代理不可用 | 订单退回队列，**不计失败次数**；Mac 暂停到额度恢复时间后再接单，并通知你 |
| Mac 正常关机 / 重启 | 处理程序收到关机信号，立即把当前订单退回队列（**不计失败次数**）并通知你 |
| Mac 断电 / 死机 | 20 分钟后订单自动退回队列（不计失败次数）；开机后处理程序据系统启动时间判断是断电，同样不计 |
| 处理程序自己崩溃（未重启电脑） | 计 1 次失败；同一订单累计中断 5 次判定失败，防止坏文件反复拖垮 Mac |
| 开机后 | 接着处理：已完成的 OCR 和逐页翻译都保留，只做剩下的页面（缓存原子写入，断电不会写坏） |
| Mac 离线 | 队列有单等了 10 分钟且 Mac 过了约定回来的时间：通知你，之后每 6 小时提醒；Mac 回来时通知“已恢复” |
| Mac 在线但无法接单 | 如模型代理没启动：通知具体原因，而不是笼统的“离线” |
| 订单退款 | Mac 下一次上报进度时被告知停止，结果不会交付 |

译文保留期从**交付时间**起算（默认 72 小时）。

## 安装

前提：已有 `~/pdf-tools/ocr-venv`（ocrmypdf + appleocr 插件）、Tesseract（`brew install tesseract`）、
BabelDOC 的字体（`~/.cache/babeldoc/fonts`），以及 babeldoc conda 环境（含 pymupdf、openai、httpx）。

1. 服务器 `examples/go-gateway/.env` 加上 `SCAN_WORKER_TOKEN`（`openssl rand -hex 32` 生成）
   和 `NOTIFY_WEBHOOK_URL`（企业微信/飞书/钉钉群机器人、Bark 或 Server酱地址），重启网关。
2. 在 Mac 上执行：

   ```bash
   sh examples/mac-scan-worker/install.sh
   ```

   第一次会生成 `~/.config/pdfmath-scan-worker.env`，填入同一个 `SCAN_WORKER_TOKEN` 和
   `OPENAI_API_KEY` 后再执行一次，即注册为开机自启的后台服务（`caffeinate` 防止空闲睡眠）。
3. 日志：`~/Library/Logs/pdfmath-scan-worker.log`
   停止：`launchctl bootout gui/$(id -u)/com.pdfmath.scan-worker`

## 让 Mac 开机后能自动恢复（需要你在系统里设置）

处理程序是登录后启动的后台服务（LaunchAgent），所以 **Mac 必须进入你的桌面它才会运行**：

- **FileVault**：开启时，重启后停在解锁界面，必须有人输入密码，期间不会接单（离线通知会提醒你）。
  想无人值守地重启后自动恢复，需要关闭 FileVault 并开启自动登录（系统设置 → 用户与群组 → 自动登录）。
  这会降低本机数据的安全性，由你权衡。计划内重启可用 `sudo fdesetup authrestart`，重启一次免输密码。
- **断电后自动开机**：`sudo pmset -a autorestart 1`
- **模型代理**：把 `magpie.app` 加入登录项（系统设置 → 通用 → 登录项），开机后自动启动。
- 处理程序运行期间会阻止系统空闲睡眠，不需要改睡眠设置。

## 常用命令

```bash
# 把失败或未交付的付费订单交给 Mac 处理（例如用户用 Bing 下单的扫描件）
python examples/mac-scan-worker/scan_worker.py adopt T0123...

# 放行暂停交付的订单（可换上修正后的文件）
python examples/mac-scan-worker/scan_worker.py release T0123...
python examples/mac-scan-worker/scan_worker.py release T0123... --dual 修正后.pdf

# 本地处理一个文件（人工兜底、试效果；--full-qa 走与线上订单相同的完整质检，
# --shared-cache 目录可在多次运行间复用译文）
python examples/mac-scan-worker/scan_worker.py process input.pdf out/ --output both --full-qa
```

## 已知限制

- 需要固定译法时可用 `GLOSSARY_FILE` 指定术语表（CSV：原文,译文）。
- 质检由模型完成，可能漏掉问题，也可能误报；只重做一轮。
- 质检让每页的模型调用量约增加一倍，处理时间也相应变长。
- 处理速度取决于模型并发和额度；数百页的文档可能需要几十分钟。
