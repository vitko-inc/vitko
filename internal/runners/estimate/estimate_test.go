package estimate

import (
	"bytes"
	"strings"
	"testing"
)

// The same hand-computed cases as the original estimator's tests.

const csvReport = `date,product,sku,quantity,unit_type,applied_cost_per_quantity,gross_amount,discount_amount,net_amount,username,organization,repository,workflow_path,cost_center_name
2026-09-01,actions,actions_linux,1000,minutes,0.006,6.0,6.0,0.0,a,acme,acme/api,.github/workflows/ci.yml,
2026-09-02,actions,linux_4_core,500,minutes,0.012,6.0,0,6.0,a,acme,acme/api,.github/workflows/ci.yml,
2026-09-02,actions,actions_macos,100,minutes,0.062,6.2,0,6.2,a,acme,acme/ios,.github/workflows/ci.yml,
2026-09-02,actions,actions_storage,10,gigabyte_hours,0.00033602,0.0034,0,0.0034,,acme,acme/api,,
2026-10-01,actions,actions_linux,5000,minutes,0.006,30.0,0,30.0,a,acme,acme/web,.github/workflows/ci.yml,
`

// The shape the REST API returns (GET /organizations/{org}/settings/billing/usage),
// with the SKU spellings it really uses.
const apiReport = `{"usageItems": [
 {"date": "2026-08-01T00:01:14Z", "product": "actions", "sku": "Actions Linux", "quantity": 3000,
  "unitType": "Minutes", "pricePerUnit": 0.006, "grossAmount": 18.0, "discountAmount": 0,
  "netAmount": 18.0, "organizationName": "acme", "repositoryName": "api"},
 {"date": "2026-08-02T00:00:00Z", "product": "actions", "sku": "Actions Linux ARM", "quantity": 100,
  "unitType": "Minutes", "pricePerUnit": 0.005, "grossAmount": 0.5, "discountAmount": 0,
  "netAmount": 0.5, "organizationName": "acme", "repositoryName": "api"},
 {"date": "2026-08-02T00:00:00Z", "product": "actions", "sku": "Actions storage", "quantity": 1.5,
  "unitType": "GigabyteHours", "pricePerUnit": 0.00033602, "grossAmount": 0.0005, "discountAmount": 0,
  "netAmount": 0.0005, "organizationName": "acme", "repositoryName": "api"}]}`

func run(t *testing.T, o Options, reports ...string) Result {
	t.Helper()
	var files [][]Row
	for _, r := range reports {
		rows, err := Parse([]byte(r))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, rows)
	}
	return Estimate(files, o)
}

func month(t *testing.T, r Result, m string) Month {
	t.Helper()
	for _, x := range r.Months {
		if x.Month == m {
			return x
		}
	}
	t.Fatalf("no month %s in %+v", m, r.Months)
	return Month{}
}

func hasClass(r Result, c string) bool {
	for _, x := range r.ByClass {
		if x.Class == c {
			return true
		}
	}
	return false
}

func TestCSV(t *testing.T) {
	r := run(t, Options{}, csvReport)
	sep, oct := month(t, r, "2026-09"), month(t, r, "2026-10")
	// 1000 min 2-core + 500 min 4-core (x2) = 2000 slot-minutes = $4.00; all inside 2,000 free.
	if sep.SlotMinutes != 2000 {
		t.Errorf("sep slot minutes = %v", sep.SlotMinutes)
	}
	if sep.VitkoBeforeFreeMicros != 4_000_000 || sep.VitkoMicros != 0 {
		t.Errorf("sep vitko = %d / %d", sep.VitkoBeforeFreeMicros, sep.VitkoMicros)
	}
	if sep.GithubGrossMicros != 12_000_000 || sep.GithubNetMicros != 6_000_000 { // macOS excluded
		t.Errorf("sep github = %d / %d", sep.GithubGrossMicros, sep.GithubNetMicros)
	}
	// October: 5000 min -> $10 list, 3000 after free -> $6.
	if oct.VitkoBeforeFreeMicros != 10_000_000 || oct.VitkoMicros != 6_000_000 {
		t.Errorf("oct vitko = %d / %d", oct.VitkoBeforeFreeMicros, oct.VitkoMicros)
	}
	if r.Storage.GigabyteHours != 10 {
		t.Errorf("storage = %v", r.Storage.GigabyteHours)
	}
	if !hasClass(r, "macos") {
		t.Error("macos class missing")
	}
	found := false
	for _, a := range r.Assumptions {
		if strings.Contains(a, "4-core") {
			found = true
		}
	}
	if !found {
		t.Errorf("larger-runner assumption missing: %v", r.Assumptions)
	}
}

func TestAPIJSON(t *testing.T) {
	r := run(t, Options{}, apiReport)
	aug := month(t, r, "2026-08")
	if aug.VitkoBeforeFreeMicros != 6_000_000 { // 3000 x $0.002
		t.Errorf("vitko list = %d", aug.VitkoBeforeFreeMicros)
	}
	if aug.VitkoMicros != 2_000_000 {
		t.Errorf("after free = %d", aug.VitkoMicros)
	}
	if aug.GithubGrossMicros != 18_000_000 {
		t.Errorf("github gross = %d", aug.GithubGrossMicros)
	}
	if !hasClass(r, "linux_arm") {
		t.Error("linux_arm class missing")
	}
}

func TestPerSecondFactor(t *testing.T) {
	r := run(t, Options{PerSecondFactor: 0.5}, apiReport)
	if m := month(t, r, "2026-08"); m.VitkoBeforeFreeMicros != 3_000_000 {
		t.Errorf("vitko list = %d", m.VitkoBeforeFreeMicros)
	}
}

func TestClassify(t *testing.T) {
	type want struct {
		class string
		cores int
	}
	for _, c := range []struct {
		product, sku, unit string
		want               want
		checkCores         bool
	}{
		{"actions", "linux_16_core", "minutes", want{"linux_x64", 16}, true},
		{"actions", "linux_4_core_gpu", "minutes", want{"gpu", 0}, false},
		{"actions", "linux_4_core_arm", "minutes", want{"linux_arm", 0}, false},
		{"actions", "windows_4_core_arm", "minutes", want{"windows", 0}, false},
		{"actions", "actions_linux_slim", "minutes", want{"linux_x64", 1}, true},
		{"actions", "Actions macOS XLarge", "Minutes", want{"macos", 0}, false},
		{"ghec", "Enterprise Cloud", "UserMonths", want{"not_actions", 0}, false},
	} {
		cls, cores := Classify(c.product, c.sku, c.unit)
		if cls != c.want.class || (c.checkCores && cores != c.want.cores) {
			t.Errorf("Classify(%q, %q, %q) = %s, %d; want %+v", c.product, c.sku, c.unit, cls, cores, c.want)
		}
	}
}

// TestSyntheticReport checks the port against the original estimator's
// output on the same synthetic report (a made-up organization, two months).
func TestSyntheticReport(t *testing.T) {
	rows, err := Load("testdata/synthetic-usage-report.csv")
	if err != nil {
		t.Fatal(err)
	}
	r := Estimate([][]Row{rows}, Options{})
	if r.RowsRead != 1891 || r.RowsIgnored != 0 {
		t.Errorf("rows = %d, ignored = %d", r.RowsRead, r.RowsIgnored)
	}
	cents := func(m int64) int64 { return (m + 5000) / 10000 }
	for _, c := range []struct {
		month                       string
		gross, net, list, afterFree int64 // cents
	}{
		{"2026-08", 30290, 28490, 10330, 9930},
		{"2026-09", 30877, 29077, 10614, 10214},
	} {
		m := month(t, r, c.month)
		got := []int64{cents(m.GithubGrossMicros), cents(m.GithubNetMicros), cents(m.VitkoBeforeFreeMicros), cents(m.VitkoMicros)}
		want := []int64{c.gross, c.net, c.list, c.afterFree}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: got %v, want %v", c.month, got, want)
				break
			}
		}
	}
	tt := r.Totals
	if cents(tt.GithubGrossMicros) != 61167 || cents(tt.GithubNetMicros) != 57567 || cents(tt.VitkoBeforeFreeMicros) != 20944 || cents(tt.VitkoMicros) != 20144 {
		t.Errorf("totals = %+v", tt)
	}
	if *tt.SavingVsGithubGross != 0.6576 || *tt.SavingVsGithubNet != 0.6501 { // 65.8% and 65.0%
		t.Errorf("savings = %v / %v", *tt.SavingVsGithubGross, *tt.SavingVsGithubNet)
	}
	classes := map[string]int64{
		"linux_arm": 4711, "linux_x64_1core": 549, "linux_x64_2core": 41502,
		"linux_x64_4core": 16888, "linux_x64_8core": 7223, "macos": 1801, "windows": 2082,
	}
	for _, c := range r.ByClass {
		if int64(c.Minutes+0.5) != classes[c.Class] {
			t.Errorf("%s minutes = %v, want %d", c.Class, c.Minutes, classes[c.Class])
		}
	}
	if strings.Join(r.NotMovable, ",") != "linux_arm,macos,windows" {
		t.Errorf("not movable = %v", r.NotMovable)
	}
	var b bytes.Buffer
	WriteText(&b, r)
	for _, want := range []string{"$611.67", "$575.67", "$209.44", "$201.44", "65.8% less", "65.0% less"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("text lacks %q", want)
		}
	}
}

func TestPaidNothing(t *testing.T) {
	r := run(t, Options{}, "date,product,sku,quantity,unit_type,applied_cost_per_quantity,gross_amount,discount_amount,net_amount\n2026-09-01,actions,actions_linux,100,minutes,0.006,0.6,0.6,0\n")
	if r.Totals.SavingVsGithubNet != nil || r.Totals.SavingVsGithubNetUnavailable == nil {
		t.Errorf("totals = %+v", r.Totals)
	}
}

func TestEmptyReport(t *testing.T) {
	r := run(t, Options{}, "date,product,sku,quantity\n")
	if r.Totals.SavingVsGithubGross != nil || len(r.Months) != 0 {
		t.Errorf("%+v", r)
	}
}
