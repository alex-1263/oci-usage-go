# oci-usage-go

单二进制的 Oracle Cloud (OCI) 用量/费用监控工具，专为 **Always Free / PAYG 免费quota用户**设计：
每天跑一次，一眼确认「是否仍在免费额度内、出站流量用了多少」，并可将报告推送到飞书群机器人。

## 为什么写这个

- 官方 `oci` CLI 的 Usage API 输出是难读的 JSON，且免费 SKU 常以多币种重复出现，直接求和会翻倍
- 现有第三方工具需要 Node/Python 运行时；本工具是 **零依赖的 Go 静态二进制**
- 对免费用户真正重要的只有一件事：**有没有冒出 $0 以外的计费项？** 本工具将其作为一等公民

## 功能

- 读取标准 `~/.oci/config`（与官方 SDK/CLI 完全兼容）
- 实现 OCI Signature v1 请求签名（纯 Go 标准库，零第三方依赖）
- 查询当月 USAGE + COST，聚合为可读表格
- 免费额度判定：任何 `cost > 0` 的项目标 ⚠️，退出码 `2` 方便监控联动
- Cost API 失败时**绝不**静默报告 $0（显式警告 + 退出码 2）
- 出站流量（10T/月配额）单独汇总
- `--push` 推送飞书群机器人

## 使用

```bash
# 本地查看当月用量
oci-usage-go

# 查指定月份
oci-usage-go -month 2026-08

# 推送飞书（webhook 也可用环境变量 FEISHU_WEBHOOK）
oci-usage-go -push -feishu https://open.feishu.cn/open-apis/bot/v2/hook/xxxx

# 静默模式：仅在免费额度之外/出错时输出（适合 cron）
oci-usage-go -push -q
```

`~/.oci/config` 示例（官方格式）：

```ini
[DEFAULT]
user=ocid1.user.oc1..xxxx
fingerprint=aa:bb:cc:...
tenancy=ocid1.tenancy.oc1..xxxx
region=us-ashburn-1
key_file=~/.oci/oci_api_key.pem
```

API key 创建：控制台 → 头像 → My profile → API keys → Add API key。

### systemd timer（每天 09:00 推送）

```ini
# /etc/systemd/system/oci-usage.service
[Unit]
Description=OCI usage report to Feishu

[Service]
Type=oneshot
Environment=FEISHU_WEBHOOK=https://open.feishu.cn/open-apis/bot/v2/hook/xxxx
ExecStart=/usr/local/bin/oci-usage-go -push -q
```

```ini
# /etc/systemd/system/oci-usage.timer
[Unit]
Description=Daily OCI usage report

[Timer]
OnCalendar=*-*-* 09:00:00
Persistent=true

[Install]
WantedBy=timers.target
```

## 构建

```bash
go build -o oci-usage-go .
# 交叉编译 Linux
GOOS=linux GOARCH=amd64 go build -o oci-usage-go-linux-amd64 .
```

## 测试

```bash
go test ./...
```

覆盖：签名串构造与 RSA 验签、多币种聚合（USD 优先）、免费额度判定、
出站流量识别、Cost API 失败显式上报、`~/.oci/config` 解析。

## License

MIT
