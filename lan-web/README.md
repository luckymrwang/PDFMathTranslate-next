# 内网网页版（单镜像）

小程序的网页版：一个容器内含 网页 + Go 网关 + Python 翻译 API，监听 `:8080`。
使用用户名和密码登录，不走微信支付/积分。线上小程序仍用原来的
`pdf2zh` + `go-gateway` 两个镜像，互不影响（构建时将内网版入口覆盖到临时构建目录，不修改线上网关）。

## 构建与运行

```bash
# 在仓库根目录。基础镜像为 luckymrwang/pdf2zh:latest（amd64），本地没有会自动从 Docker Hub 拉取
docker compose -f lan-web/compose.yml up -d --build
# 浏览器打开 http://<主机>:8080
```

可选配置：复制 `lan-web/lan.env.example` 为 `lan-web/lan.env`（已被 git 忽略）：

| 变量 | 作用 |
|---|---|
| `LAN_USERNAME` / `LAN_PASSWORD` | 登录用户名和密码，默认分别为 `wangchl01` 和 `admin`；可在部署环境中覆盖 |
| `LAN_DATA_DIR` | 模型配置目录；Compose 和 Kubernetes 将其挂载为持久存储 |
| `WEB_PORT`（compose 环境变量） | 对外端口，默认 8080 |

## Kubernetes

`k8s.yaml` 在 `ping` 命名空间创建 Deployment、NodePort Service 和保存模型配置的 PVC。清单使用 `2026669.xyz:65000` 镜像代理；节点需要能拉取该镜像，并提供 `hostpath-incloud` 存储类。登录密码和 API Key 通过 HTTP 传输，请仅在可信内网使用或配置 HTTPS。

## 离线交付到内网机器

```bash
docker save pdf2zh-web:latest | gzip > pdf2zh-web.tar.gz      # 开发机
docker load < pdf2zh-web.tar.gz                               # 内网机
docker run -d --name pdf2zh-web --init --restart unless-stopped -p 8080:8080 \
    -e LAN_DATA_DIR=/data -v pdf2zh-web-config:/data \
    --env-file lan.env pdf2zh-web:latest
```

## 说明

- 译文存放在容器 `/tmp`，容器重启后清除；翻译记录只存浏览器本地。
- 登录后在网页中填写 OpenAI 兼容服务的 Base URL、模型名称和 API Key。配置保存在服务端，接口不会返回已保存的密钥。
- 示例密码适合临时使用；HTTP 访问不会加密登录密码或 API Key，建议修改密码并在需要时配置 HTTPS。
- 单文件上限 30 MB。
