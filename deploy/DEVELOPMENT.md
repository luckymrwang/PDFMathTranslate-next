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
`47.88.84.45` 上通过独立 Compose 项目 `pdfmath-dev` 运行，入口为
`dev.2026669.me → 开发服务器的 Caddy → 127.0.0.1:8080`。
正式、开发网关虽然都使用 8080，但位于不同服务器，不会产生端口冲突。
开发网关通过容器网络访问独立 Python 服务；宿主机 Python 调试端口为
`127.0.0.1:11009`。内部端口不冲突，两个服务都只向宿主机回环地址发布。

## 首次部署

### 当前开发机：复用 Python 环境，用 systemd 运行

`47.88.84.45` 现有约 2GB 内存和较少空闲磁盘，已有
`/root/miniconda3/envs/babledoc` Python 环境。此次部署选择 systemd，
避免在该机器上重新构建完整镜像。实际代码目录为
`/root/pdf/PDFMathTranslate-next`（它位于开发机，不是正式机的目录）。

服务模板位于 `deploy/dev/pdfmath-dev-{api,gateway}.service`，绑定开发机
回环地址 `8080` / `11009`，订单和翻译文件位于 `/var/lib/pdfmath-dev`。
API 限制 1 个并发、1400MB 物理内存及 2GB swap（开发机已有 4GB swap），
优先使用小 PDF 测试。PDF 工作进程即使处理一页也可能超出物理内存上限，
需要使用 swap，速度会变慢；上述限制不代表大文件一定能运行成功。

构建 Linux amd64 网关（可在开发电脑交叉编译），将二进制安装到开发机
`/usr/local/lib/pdfmath-dev/go-gateway`。填写 `deploy/dev/.env`，除小程序
身份、独立 JWT 和翻译引擎凭据外，还需设置：

```dotenv
GATEWAY_ADDR=127.0.0.1:8080
PDF2ZH_API_URL=http://127.0.0.1:11009
VIRTUALPAY_ENABLED=false
VIRTUALPAY_DATA_DIR=/var/lib/pdfmath-dev/orders
```

然后在开发机代码目录安装服务：

```bash
install -m 644 deploy/dev/pdfmath-dev-api.service /etc/systemd/system/
install -m 644 deploy/dev/pdfmath-dev-gateway.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now pdfmath-dev-api pdfmath-dev-gateway
systemctl status pdfmath-dev-api pdfmath-dev-gateway --no-pager
curl --fail http://127.0.0.1:11009/health
curl --fail http://127.0.0.1:8080/health
```

日常更新时同步代码和网关二进制，确认没有需要保留的开发任务，再重启
`pdfmath-dev-api` / `pdfmath-dev-gateway`。日志用
`journalctl -u pdfmath-dev-api -u pdfmath-dev-gateway` 查看。
不要同时启动下述 Compose，它会占用同样的端口。

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
开发 Compose 默认强制关闭虚拟支付，因此可以测试登录和引擎接口，也可
通过 `/api/translate` 调试翻译；当前小程序订单、文件准备和支付流程不能
在此状态下完整运行。`demoMode` 不是后端支付模拟器。

需要专门测试真实支付时，先准备独立订单存储、正确的商品及支付配置和
通知路由方案，再显式设置 `DEV_VIRTUALPAY_ENABLED=true` 启动开发 Compose。
该变量是 Compose 插值变量，需在启动命令环境中设置；仅放入 gateway 的
`deploy/dev/.env` 不会启用。不要把正式支付通知地址直接切到开发环境。
模拟支付或支付沙箱需要另行实现。

## 日常更新

当前 systemd 部署按上述 systemd 步骤更新。若选择 Compose 部署，在开发
目录更新代码后，继续使用明确指定文件的 Compose 命令：

```bash
docker compose -f deploy/docker-compose.dev.yml up -d --build
docker compose -f deploy/docker-compose.dev.yml logs -f --tail 100
```

重建可能打断开发翻译任务，先确认开发环境没有需要保留的运行中任务。
不要在开发目录运行 `deploy/docker-deploy.sh`、`deploy/restart-all.sh`，
也不要把本文件与正式 Compose 合并使用。停止开发环境：

```bash
docker compose -f deploy/docker-compose.dev.yml down
```

不加 `-v`，以保留开发订单卷。正式发布继续走正式环境的独立发布流程，
保持旧版客户端接口兼容，并在提交审核前验证审核包实际连接的环境。
