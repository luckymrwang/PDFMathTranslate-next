# 开发环境：dev.2026669.me

正式和开发环境部署在两台独立服务器：

| 环境 | 域名 | 服务器 |
| --- | --- | --- |
| 正式 | `2026669.me` | `103.85.226.245` |
| 开发 | `dev.2026669.me` | `47.88.84.45` |

后端仓库 `PDFMathTranslate-next` 和小程序仓库 `pdftranslate` 均使用独立
`dev` 分支开发。开发服务器检出后端 `dev` 分支；更新时先确认工作区干净，
再执行 `git pull --ff-only origin dev`。不要将开发部署直接推送到正式分支。

正式服务继续使用 `2026669.me → :8080 → :11008`。开发环境仅在
`47.88.84.45` 上通过 `deploy` 脚本运行，入口为
`dev.2026669.me → 开发服务器的 Caddy → 127.0.0.1:8080`。
正式、开发网关虽然都使用 8080，但位于不同服务器，不会产生端口冲突。
开发网关访问独立 Python 服务 `127.0.0.1:11009`。两个服务都只监听
开发机回环地址；Caddy 负责对外提供 HTTPS。

## 首次部署

### 当前开发机：使用 deploy 脚本

`47.88.84.45` 现有约 2GB 内存和 4GB swap，复用已有的
`/root/miniconda3/envs/babledoc` Python 环境。代码目录为
`/root/pdf/PDFMathTranslate-next`，当前分支为 `dev`。

不使用 systemd 管理后端；脚本通过 `setsid nohup` 在后台启动进程。
这不是进程守护器：进程异常退出或服务器重启后需要再次运行启动脚本。
API 使用 1 个并发、QPS 1，优先用小 PDF 测试；没有 systemd 的内存和
CPU 限额，PDF 工作进程仍可能占用较多内存或 swap。

开发网关使用独立配置 `deploy/dev/.env`，不读取正式网关配置。
首次配置可从 `deploy/dev/.env.example` 复制，填写 AppID、AppSecret、
独立 JWT_SECRET 和翻译引擎凭据，并设置：

```dotenv
GATEWAY_ADDR=127.0.0.1:8080
PDF2ZH_API_URL=http://127.0.0.1:11009
VIRTUALPAY_ENABLED=false
VIRTUALPAY_DATA_DIR=/var/lib/pdfmath-dev/orders
```

配置文件权限设为 `600`。开发机已配置好该文件，不需要重新复制模板。
服务器需有 Go 编译器（脚本会自动查找 `/usr/local/go/bin`）、Conda、
`curl`、`ss`、`setsid` 和 `nohup`。

在开发机代码目录执行：

```bash
# 只重启当前 dev 代码，不拉取
bash deploy/restart-dev.sh

# 拉取 origin/dev，编译网关并重启
bash deploy/pull-dev.sh
```

两个入口都复用 `deploy/restart-all.sh`。更新入口只允许在干净的 `dev`
工作区执行 `git pull --ff-only origin dev`；拉取失败或网关编译失败不会
停止当前服务。重启前检查正在运行的翻译任务；有任务或无法确认任务状态
时拒绝重启。需要明确放弃任务时才传 `-f`。

日志文件：

```bash
tail -f deploy/dev/logs/pdf2zh-api.log deploy/dev/logs/go-gateway.log
curl --fail http://127.0.0.1:11009/health
curl --fail http://127.0.0.1:8080/health
```

API 监听 `11009`，网关监听 `8080`，订单和翻译临时文件位于
`/var/lib/pdfmath-dev`。迁移时已停用原来的 `pdfmath-dev-api` 和
`pdfmath-dev-gateway` systemd 服务，避免自动重启导致端口抢占。
不要同时启动下面的可选 Compose 部署。

### 可选：Docker Compose

以下部署命令只在开发服务器 `47.88.84.45` 上执行，不在正式服务器执行。
建议将开发代码放在 `/root/pdf-dev/PDFMathTranslate-next`。
在开发服务器的开发代码目录执行：

```bash
mkdir -p deploy/dev
cp deploy/dev/.env.example deploy/dev/.env
chmod 600 deploy/dev/.env
```

编辑 `deploy/dev/.env`，填写正式小程序的 AppID、匹配的 AppSecret，以及
新生成的独立 JWT_SECRET。按需添加翻译引擎凭据。对象存储使用独立测试
bucket；不要直接复制正式配置。订单与上传文件使用独立的 Docker 命名卷。

```bash
docker compose -f deploy/docker-compose.dev.yml config --quiet
docker compose -f deploy/docker-compose.dev.yml up -d --build
docker compose -f deploy/docker-compose.dev.yml ps
curl --fail http://127.0.0.1:8080/health
curl --fail http://127.0.0.1:11009/health
```

开发镜像使用独立的 `pdfmath-dev/*:local` 名称，不覆盖正式镜像 `latest`。
首次构建 Python 镜像较重。开发 Python 默认限制为 1 CPU、2GB 内存、1 个
并发任务，仅适合小 PDF；大文件可能触发内存限制。可根据开发服务器实际
配置调整限制。两台服务器的进程、容器和本地磁盘分离；对象存储、支付和
翻译 API 仍需使用各自的环境配置，避免共用正式订单或文件数据。

## 域名和小程序

DNS 将 `dev.2026669.me` 指向 `47.88.84.45`，正式域名仍指向
`103.85.226.245`。在开发服务器的 Caddy 配置中添加：

```caddyfile
dev.2026669.me:443 {
    reverse_proxy http://127.0.0.1:8080
}
```

这里的 `127.0.0.1` 指开发服务器，不指正式服务器。Caddy 需运行在宿主机
或使用 host 网络，才能访问该回环端口。若 Caddy 使用独立的 Docker 桥接
网络，应调整为共享容器网络连接网关。

以下示例假设开发服务器也使用名为 `caddy` 的容器，并将配置文件挂载到
`/etc/caddy/Caddyfile`；按开发服务器的实际安装方式调整。在开发服务器
保存配置后，先 validate 成功，再 reload：

```bash
docker exec caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
docker exec caddy caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
curl --fail https://dev.2026669.me/health
```

如果宿主机文件已经是 `8080`，容器内仍显示 `18080`，可能是编辑器原子
替换文件后，单文件 bind mount 仍引用旧 inode。此时直接 reload 会再次
加载旧文件。应核对容器内实际文件，备份后同步开发域名路由，再 validate
和 reload；不要仅凭宿主机文件判断配置已经生效。

如果正式服务器还保留之前添加的 dev 域名路由，可在确认 DNS 和开发服务
正常后移除该路由；不修改正式域名和正式支付回调的路由。

在「PDF翻译通」正式账号后台，将 `https://dev.2026669.me` 添加到 request、
uploadFile、downloadFile 合法域名，保留 `https://2026669.me`。
小程序继续使用正式 AppID；开发版和体验版请求 dev 域名，正式版请求正式
域名。正式版沿用旧缓存键，开发版和体验版共享独立的 dev 缓存。

## 支付边界

当前代码的下单、查单、发货参数仍指向生产支付。开发域名不代表支付沙箱。
开发配置关闭虚拟支付（Compose 默认也强制关闭），因此可以测试登录和引擎接口，也可
通过 `/api/translate` 调试翻译；当前小程序订单、文件准备和支付流程不能
在此状态下完整运行。`demoMode` 不是后端支付模拟器。

需要专门测试真实支付时，先准备独立订单存储、正确的商品及支付配置和
通知路由方案，再显式设置 `DEV_VIRTUALPAY_ENABLED=true` 启动开发 Compose。
该变量是 Compose 插值变量，需在启动命令环境中设置；仅放入 gateway 的
`deploy/dev/.env` 不会启用。不要把正式支付通知地址直接切到开发环境。
模拟支付或支付沙箱需要另行实现。

## 日常更新

当前部署使用 `bash deploy/pull-dev.sh` 更新。若选择 Compose 部署，在开发
目录更新代码后，继续使用明确指定文件的 Compose 命令：

```bash
docker compose -f deploy/docker-compose.dev.yml up -d --build
docker compose -f deploy/docker-compose.dev.yml logs -f --tail 100
```

重建可能打断开发翻译任务，先确认开发环境没有需要保留的运行中任务。
不要直接运行正式 Docker 更新脚本 `deploy/docker-deploy.sh`，也不要将开发
和正式 Compose 合并使用。脚本部署请使用上述 dev 入口。停止可选 Compose：

```bash
docker compose -f deploy/docker-compose.dev.yml down
```

不加 `-v`，以保留开发订单卷。正式发布继续走正式环境的独立发布流程，
保持旧版客户端接口兼容，并在提交审核前验证审核包实际连接的环境。
