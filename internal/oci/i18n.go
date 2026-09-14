package oci

import "strings"

// serviceCN maps common OCI service names to Chinese. Unmapped names pass through.
var serviceCN = map[string]string{
	"compute":                         "计算实例",
	"block storage":                   "块存储",
	"object storage":                  "对象存储",
	"virtual cloud network":           "虚拟云网络",
	"load balancer":                   "负载均衡",
	"nat gateway":                     "NAT 网关",
	"container engine for kubernetes": "容器引擎 (OKE)",
	"oracle database":                 "数据库",
	"autonomous database":             "自治数据库",
	"nosql database":                  "NoSQL 数据库",
	"key management":                  "密钥管理",
	"logging":                         "日志服务",
	"monitoring":                      "监控",
	"notifications":                   "通知服务",
	"file storage service":            "文件存储",
	"dns":                             "DNS 解析",
	"email delivery":                  "邮件投递",
	"api gateway":                     "API 网关",
	"functions":                       "函数计算",
	"vault":                           "密钥保险库",
	"announcement service":            "公告服务",
	"audit":                           "审计",
	"budget":                          "预算",
	"resource manager":                "资源管理器",
}

// CNService returns the Chinese name for an OCI service, or the original.
func CNService(s string) string {
	if v, ok := serviceCN[strings.ToLower(strings.TrimSpace(s))]; ok {
		return v
	}
	return s
}

// skuCN maps a few frequent SKU phrases; SKU codes like "Standard - A1" stay as-is.
func SKUCN(sku string) string {
	switch {
	case strings.Contains(strings.ToLower(sku), "outbound data transfer"):
		return "出站数据传输"
	case strings.Contains(strings.ToLower(sku), "standard - a1"):
		return "A1 弹性实例 (ARM)"
	case strings.Contains(strings.ToLower(sku), "block volume"):
		return "块卷存储"
	case strings.Contains(strings.ToLower(sku), "boot volume"):
		return "引导卷存储"
	}
	return sku
}
