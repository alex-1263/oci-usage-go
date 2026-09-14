package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	oci "github.com/alex-1263/oci-usage-go/internal/oci"
)

// --- Feishu webhook push ---

type feishuMsg struct {
	MsgType string    `json:"msg_type"`
	Content feishuDiv `json:"content"`
}
type feishuDiv struct {
	Text string `json:"text"`
}

func pushFeishu(webhook, text string) error {
	body, _ := json.Marshal(feishuMsg{MsgType: "text", Content: feishuDiv{Text: text}})
	resp, err := http.Post(webhook, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("feishu status %d", resp.StatusCode)
	}
	return nil
}

func main() {
	var (
		config  = flag.String("c", filepath.Join(os.Getenv("HOME"), ".oci", "config"), "OCI config path")
		profile = flag.String("p", "DEFAULT", "config profile section")
		month   = flag.String("month", "", "month to query (YYYY-MM); default current month")
		webhook = flag.String("feishu", os.Getenv("FEISHU_WEBHOOK"), "feishu bot webhook url (or env FEISHU_WEBHOOK)")
		push    = flag.Bool("push", false, "push report to feishu webhook")
		quiet   = flag.Bool("q", false, "only print when outside free tier or errors")
	)
	flag.Parse()

	start, end := monthRange(*month)
	prof, err := oci.ParseConfig(*config, *profile)
	must(err, "load config")

	res, err := oci.QueryUsage(prof, start, end, 30*time.Second)
	must(err, "query usage")

	var b strings.Builder
	fmt.Fprintf(&b, "OCI 用量报告 %s ~ %s (%s)\n", start.Format("2006-01-02"), end.Add(-time.Second).Format("2006-01-02"), res.Region)
	fmt.Fprintf(&b, "出站流量: %.2f GB\n", res.OutboundGB)
	if res.CostFailed {
		fmt.Fprintf(&b, "⚠️ Cost API 查询失败，费用数据不可用，请人工核查！\n")
	}
	bad := 0
	for _, li := range res.LineItems {
		cost := "免费/无费用"
		if res.CostFailed {
			cost = "未知(Cost API 失败)"
		} else if li.Cost != nil && *li.Cost > 0 {
			cost = fmt.Sprintf("%.2f %s ⚠️", *li.Cost, li.Currency)
			bad++
		}
		fmt.Fprintf(&b, "%-16s %-24s %12.2f %-16s %s\n", truncate(oci.CNService(li.Service), 16), truncate(oci.SKUCN(li.SKU), 24), li.Quantity, li.Unit, cost)
	}
	if res.CostFailed {
		bad++
	}
	switch {
	case bad == 0:
		fmt.Fprintf(&b, "✅ 全部在免费额度内\n")
	default:
		fmt.Fprintf(&b, "⚠️ %d 项在免费额度之外，请立即检查！\n", bad)
	}
	out := b.String()

	if !*quiet || bad > 0 {
		fmt.Print(out)
	}
	if *push {
		must(pushFeishu(*webhook, out), "feishu push")
		fmt.Fprintln(os.Stderr, "已推送飞书")
	}
	if bad > 0 {
		os.Exit(2) // non-zero so cron wrappers / monitors can react
	}
}

func monthRange(m string) (time.Time, time.Time) {
	now := time.Now().UTC()
	if m == "" {
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	}
	re := regexp.MustCompile(`^(\d{4})-(\d{2})$`)
	parts := re.FindStringSubmatch(m)
	if parts == nil {
		fmt.Fprintln(os.Stderr, "invalid --month, want YYYY-MM")
		os.Exit(1)
	}
	y, mo := atoi(parts[1]), atoi(parts[2])
	return time.Date(y, time.Month(mo), 1, 0, 0, 0, 0, time.UTC), time.Date(y, time.Month(mo+1), 1, 0, 0, 0, 0, time.UTC)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func must(err error, what string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", what, err)
		os.Exit(1)
	}
}
