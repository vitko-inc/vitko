// Package estimate answers "what would this GitHub Actions usage cost on
// Vitko Runners?" from GitHub's own usage report. It runs offline: the report
// never leaves the machine.
//
// Input, in either form (several files may be given, for example one per month):
//   - the CSV from GitHub: Settings > Billing and licensing > Usage > Get usage
//     report (summarized or detailed);
//   - the JSON from GET /organizations/{org}/settings/billing/usage
//     ({"usageItems": [...]}).
//
// Rules:
//   - Only GitHub-hosted Linux x64 minutes can move. Arm, Windows, macOS and
//     GPU minutes are reported but not priced.
//   - Actions storage (artifacts, caches) stays on GitHub's bill whatever runner
//     runs the job; it is reported, not moved.
//   - GitHub reports whole minutes, rounded up per job. Vitko bills per second,
//     so pricing those minutes at the Vitko rate is an upper bound. A
//     per-second factor below 1 shows an illustrative lower figure.
//   - A Vitko slot is up to 2 vCPU / 8 GB. A larger GitHub runner counts as
//     cores / 2 slots (rounded up), and the output says so.
//   - GitHub gross = what GitHub's list price bills; GitHub net = what was
//     actually paid after included minutes and discounts. Vitko figures are
//     shown before and after the free minutes of each month.
package estimate

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/vitko-inc/vitko/internal/runners/pricing"
)

// Row is one usage line with normalized (snake_case) keys.
type Row map[string]any

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// Norm lowercases and turns runs of other characters into single underscores.
func Norm(s string) string {
	return strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "_"), "_")
}

func num(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return x
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		s := strings.NewReplacer("$", "", ",", "").Replace(strings.TrimSpace(x))
		if s == "" {
			return 0
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0
		}
		return f
	case bool:
		if x {
			return 1
		}
		return 0
	}
	return 0
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}

// aliases: CSV export (snake_case), REST JSON (camelCase), older CSVs.
var aliases = map[string][]string{
	"date":       {"date"},
	"product":    {"product"},
	"sku":        {"sku"},
	"quantity":   {"quantity"},
	"unit_type":  {"unit_type", "unittype", "unit_type_"},
	"rate":       {"applied_cost_per_quantity", "priceperunit", "price_per_unit"},
	"gross":      {"gross_amount", "grossamount"},
	"net":        {"net_amount", "netamount"},
	"multiplier": {"multiplier"},
	"repo":       {"repository", "repositoryname", "repository_slug", "repository_name"},
	"workflow":   {"workflow_path", "actions_workflow"},
}

func pick(r Row, key string) (any, bool) {
	for _, a := range aliases[key] {
		if v, ok := r[a]; ok && v != nil && v != "" {
			return v, true
		}
	}
	return nil, false
}

// Parse reads one usage file's bytes (CSV or JSON).
func Parse(data []byte) ([]Row, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("not valid JSON: %v", err)
		}
		var items []any
		switch x := v.(type) {
		case map[string]any:
			if u, ok := x["usageItems"].([]any); ok {
				items = u
			} else {
				return nil, fmt.Errorf("JSON has no usageItems list")
			}
		case []any:
			items = x
		}
		rows := make([]Row, 0, len(items))
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			r := Row{}
			for k, val := range m {
				r[Norm(k)] = val
			}
			rows = append(rows, r)
		}
		return rows, nil
	}
	cr := csv.NewReader(bytes.NewReader(data))
	cr.FieldsPerRecord = -1
	recs, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("not a valid CSV: %v", err)
	}
	if len(recs) == 0 {
		return nil, nil
	}
	head := recs[0]
	rows := make([]Row, 0, len(recs)-1)
	for _, rec := range recs[1:] {
		r := Row{}
		for i, h := range head {
			if i < len(rec) {
				r[Norm(h)] = rec[i]
			}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// Load reads a usage file from disk.
func Load(path string) ([]Row, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

var coreRe = regexp.MustCompile(`(\d+)_?core`)

// Classify returns the runner class and core count for a usage row. Classes:
// linux_x64, linux_arm, windows, macos, gpu, storage, not_actions, other.
func Classify(product, sku, unit string) (string, int) {
	p, s, u := Norm(product), Norm(sku), Norm(unit)
	switch {
	case strings.Contains(s, "storage") || strings.HasPrefix(u, "gigabyte"):
		return "storage", 0
	case p != "" && p != "actions":
		return "not_actions", 0
	case u != "" && u != "minutes":
		return "other", 0
	case strings.Contains(s, "gpu"):
		return "gpu", 0
	case strings.Contains(s, "macos") || strings.HasPrefix(s, "mac"):
		return "macos", 0
	case strings.Contains(s, "windows"):
		return "windows", 0
	case strings.Contains(s, "linux"):
		cores := 2
		if m := coreRe.FindStringSubmatch(s); m != nil {
			cores, _ = strconv.Atoi(m[1])
		} else if strings.Contains(s, "slim") {
			cores = 1
		}
		if strings.Contains(s, "arm") {
			return "linux_arm", cores
		}
		return "linux_x64", cores
	}
	return "other", 0
}

// Options tune the estimate.
type Options struct {
	// PerSecondFactor in (0, 1]; 1 is the upper bound.
	PerSecondFactor float64
	ByRepo          bool
}

// Month is one month of movable Linux x64 usage.
type Month struct {
	Month                 string  `json:"month"`
	MovableMinutes        float64 `json:"movable_minutes"`
	SlotMinutes           float64 `json:"slot_minutes"`
	FreeMinutes           float64 `json:"free_minutes"`
	GithubGrossMicros     int64   `json:"github_gross_micros"`
	GithubNetMicros       int64   `json:"github_net_micros"`
	VitkoBeforeFreeMicros int64   `json:"vitko_before_free_micros"`
	VitkoMicros           int64   `json:"vitko_micros"`
}

// Class is usage of one runner class in GitHub's report.
type Class struct {
	Class       string  `json:"class"`
	Movable     bool    `json:"movable"`
	Minutes     float64 `json:"minutes"`
	GrossMicros int64   `json:"github_gross_micros"`
	NetMicros   int64   `json:"github_net_micros"`
}

// Storage is Actions storage, which stays on GitHub's bill.
type Storage struct {
	GigabyteHours float64 `json:"gigabyte_hours"`
	GrossMicros   int64   `json:"github_gross_micros"`
	NetMicros     int64   `json:"github_net_micros"`
}

// Totals sums the months.
type Totals struct {
	SlotMinutes                    float64  `json:"slot_minutes"`
	GithubGrossMicros              int64    `json:"github_gross_micros"`
	GithubNetMicros                int64    `json:"github_net_micros"`
	VitkoBeforeFreeMicros          int64    `json:"vitko_before_free_micros"`
	VitkoMicros                    int64    `json:"vitko_micros"`
	SavingVsGithubGross            *float64 `json:"saving_vs_github_gross"`
	SavingVsGithubGrossUnavailable *string  `json:"saving_vs_github_gross_unavailable_reason"`
	SavingVsGithubNet              *float64 `json:"saving_vs_github_net"`
	SavingVsGithubNetUnavailable   *string  `json:"saving_vs_github_net_unavailable_reason"`
}

// Repo is movable minutes per repository.
type Repo struct {
	Repository string  `json:"repository"`
	Minutes    float64 `json:"minutes"`
}

// Result is vitko.runners.estimate/v1.
type Result struct {
	Schema              string   `json:"schema"`
	PricesAsOf          string   `json:"prices_as_of"`
	Currency            string   `json:"currency"`
	RateMicrosPerMinute int64    `json:"rate_micros_per_minute"`
	FreeMinutesPerMonth int      `json:"free_minutes_per_month"`
	PerSecondFactor     float64  `json:"per_second_factor"`
	Files               int      `json:"files"`
	RowsRead            int      `json:"rows_read"`
	RowsIgnored         int      `json:"rows_ignored"`
	Months              []Month  `json:"months"`
	Totals              Totals   `json:"totals"`
	ByClass             []Class  `json:"by_class"`
	Storage             Storage  `json:"storage_stays_on_github"`
	NotMovable          []string `json:"not_movable"`
	Repositories        []Repo   `json:"repositories,omitempty"`
	Assumptions         []string `json:"assumptions"`
	Caveats             []string `json:"caveats"`
}

// Micros converts dollars to integer micros.
func Micros(usd float64) int64 { return int64(math.Round(usd * 1e6)) }

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// Caveats go with every estimate.
var Caveats = []string{
	"This is an estimate from GitHub's own usage report, at list prices on both sides. It is not a quote or an invoice.",
	"Job durations on Vitko may differ from durations on GitHub-hosted runners.",
	"Minutes on runners other than Linux x64 stay on GitHub, and so does Actions storage.",
	"Larger GitHub runners are counted as cores / 2 slots (a slot is up to 2 vCPU / 8 GB), rounded up.",
	"Dated: GitHub prices are the ones applied in your report; the Vitko list price is $0.002 per slot-minute as of " + pricing.AsOf + ".",
}

type acc struct{ minutes, gross, net float64 }

// Estimate computes the result from rows grouped by file.
func Estimate(files [][]Row, o Options) Result {
	f := o.PerSecondFactor
	if f == 0 {
		f = 1
	}
	byClass := map[string]*acc{}
	type macc struct{ slot, gross, net float64 }
	months := map[string]*macc{}
	repos := map[string]float64{}
	larger := map[int]bool{}
	var storage acc
	rowsSeen, ignored := 0, 0
	for _, rows := range files {
		for _, r := range rows {
			rowsSeen++
			product, _ := pick(r, "product")
			sku, _ := pick(r, "sku")
			unit, _ := pick(r, "unit_type")
			cls, cores := Classify(str(product), str(sku), str(unit))
			qv, _ := pick(r, "quantity")
			q := num(qv)
			rv, _ := pick(r, "rate")
			rate := num(rv)
			var gross float64
			if gv, ok := pick(r, "gross"); ok {
				gross = num(gv)
			} else {
				mv, _ := pick(r, "multiplier")
				mult := num(mv)
				if mult == 0 {
					mult = 1
				}
				gross = q * rate * mult
			}
			net := gross
			if nv, ok := pick(r, "net"); ok {
				net = num(nv)
			}
			if cls == "not_actions" {
				ignored++
				continue
			}
			if cls == "storage" {
				storage.minutes += q
				storage.gross += gross
				storage.net += net
				continue
			}
			key := cls
			if cls == "linux_x64" {
				key = fmt.Sprintf("linux_x64_%dcore", cores)
			}
			c := byClass[key]
			if c == nil {
				c = &acc{}
				byClass[key] = c
			}
			c.minutes += q
			c.gross += gross
			c.net += net
			if cls == "linux_x64" {
				slots := int(math.Max(1, math.Ceil(float64(cores)/2)))
				if slots > 1 {
					larger[cores] = true
				}
				dv, _ := pick(r, "date")
				month := str(dv)
				if len(month) > 7 {
					month = month[:7]
				}
				if month == "" {
					month = "unknown"
				}
				m := months[month]
				if m == nil {
					m = &macc{}
					months[month] = m
				}
				m.slot += q * float64(slots)
				m.gross += gross
				m.net += net
				repo := "(no repository)"
				if rp, ok := pick(r, "repo"); ok {
					repo = str(rp)
				}
				repos[repo] += q
			}
		}
	}

	res := Result{
		Schema: "vitko.runners.estimate/v1", PricesAsOf: pricing.AsOf, Currency: "USD",
		RateMicrosPerMinute: pricing.SlotMicrosPerMinute, FreeMinutesPerMonth: pricing.FreeMinutesPerMonth,
		PerSecondFactor: f, Files: len(files), RowsRead: rowsSeen, RowsIgnored: ignored,
		Months: []Month{}, ByClass: []Class{}, NotMovable: []string{}, Assumptions: []string{},
		Caveats: Caveats,
	}
	rate := float64(pricing.SlotMicrosPerMinute) / 1e6
	free := float64(pricing.FreeMinutesPerMonth)
	var tGross, tNet, tList, tAfter, tSlot float64
	keys := make([]string, 0, len(months))
	for k := range months {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m := months[k]
		billable := m.slot * f
		list := billable * rate
		after := math.Max(0, billable-free)
		res.Months = append(res.Months, Month{
			Month: k, MovableMinutes: round1(m.slot), SlotMinutes: round1(billable),
			FreeMinutes:       math.Min(free, round1(billable)),
			GithubGrossMicros: Micros(m.gross), GithubNetMicros: Micros(m.net),
			VitkoBeforeFreeMicros: Micros(list), VitkoMicros: Micros(after * rate),
		})
		tGross += m.gross
		tNet += m.net
		tSlot += billable
		tList += list
		tAfter += after * rate
	}
	t := Totals{SlotMinutes: round1(tSlot), GithubGrossMicros: Micros(tGross), GithubNetMicros: Micros(tNet),
		VitkoBeforeFreeMicros: Micros(tList), VitkoMicros: Micros(tAfter)}
	if tGross > 0 {
		v := math.Round((1-tList/tGross)*10000) / 10000
		t.SavingVsGithubGross = &v
	} else {
		s := "no Linux x64 minutes in the report"
		t.SavingVsGithubGrossUnavailable = &s
	}
	if tNet > 0 {
		v := math.Round((1-tAfter/tNet)*10000) / 10000
		t.SavingVsGithubNet = &v
	} else if tGross > 0 {
		s := "you paid GitHub $0 for these minutes (they were inside your included minutes)"
		t.SavingVsGithubNetUnavailable = &s
	} else {
		s := "no Linux x64 minutes in the report"
		t.SavingVsGithubNetUnavailable = &s
	}
	res.Totals = t

	classKeys := make([]string, 0, len(byClass))
	for k := range byClass {
		classKeys = append(classKeys, k)
	}
	sort.Strings(classKeys)
	for _, k := range classKeys {
		c := byClass[k]
		movable := strings.HasPrefix(k, "linux_x64")
		res.ByClass = append(res.ByClass, Class{Class: k, Movable: movable, Minutes: round1(c.minutes),
			GrossMicros: Micros(c.gross), NetMicros: Micros(c.net)})
		if !movable {
			res.NotMovable = append(res.NotMovable, k)
		}
	}
	res.Storage = Storage{GigabyteHours: math.Round(storage.minutes*100) / 100, GrossMicros: Micros(storage.gross), NetMicros: Micros(storage.net)}

	if f < 1 {
		res.Assumptions = append(res.Assumptions, fmt.Sprintf("A per-second factor of %g was applied. It is illustrative: the real ratio depends on your jobs.", f))
	} else {
		res.Assumptions = append(res.Assumptions, "The Vitko figure is an upper bound: GitHub rounds each job up to whole minutes, and Vitko bills per second.")
	}
	if _, ok := byClass["linux_x64_1core"]; ok {
		res.Assumptions = append(res.Assumptions, "GitHub's 1-core Linux runners already cost $0.002/min on GitHub, so those minutes show no saving.")
	}
	if len(larger) > 0 {
		var cs []int
		for c := range larger {
			cs = append(cs, c)
		}
		sort.Ints(cs)
		parts := make([]string, len(cs))
		for i, c := range cs {
			parts[i] = fmt.Sprintf("%d-core", c)
		}
		res.Assumptions = append(res.Assumptions, fmt.Sprintf("Larger Linux runners (%s) are counted as cores / 2 slots at $0.002 per slot-minute. Sizes above 2 vCPU / 8 GB are not on the price list, so treat these rows as an estimate.", strings.Join(parts, ", ")))
	}
	if len(res.NotMovable) > 0 {
		res.Assumptions = append(res.Assumptions, "Not offered on Vitko, so these stay on GitHub: "+strings.Join(res.NotMovable, ", ")+".")
	}
	if o.ByRepo {
		res.Repositories = []Repo{}
		for k, v := range repos {
			res.Repositories = append(res.Repositories, Repo{Repository: k, Minutes: round1(v)})
		}
		sort.Slice(res.Repositories, func(i, j int) bool {
			a, b := res.Repositories[i], res.Repositories[j]
			if a.Minutes != b.Minutes {
				return a.Minutes > b.Minutes
			}
			return a.Repository < b.Repository
		})
	}
	return res
}

func usd(micros int64) string {
	neg := micros < 0
	if neg {
		micros = -micros
	}
	cents := (micros + 5000) / 10000
	s := fmt.Sprintf("%d.%02d", cents/100, cents%100)
	whole, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, ch := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	out := "$" + b.String() + "." + frac
	if neg {
		out = "-" + out
	}
	return out
}

func thousands(f float64) string {
	s := strconv.FormatInt(int64(math.Round(f)), 10)
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// WriteText renders the result for a terminal.
func WriteText(w io.Writer, r Result) {
	fmt.Fprintf(w, "Read %d rows from %d file(s); %d rows that aren't Actions usage (seats, Copilot, ...) were ignored.\n", r.RowsRead, r.Files, r.RowsIgnored)
	fmt.Fprintf(w, "\nGitHub usage by runner type:\n")
	fmt.Fprintf(w, "  %-20s%12s%14s%14s\n", "runner type", "minutes", "GitHub list", "GitHub paid")
	for _, c := range r.ByClass {
		fmt.Fprintf(w, "  %-20s%12s%14s%14s\n", c.Class, thousands(c.Minutes), usd(c.GrossMicros), usd(c.NetMicros))
	}
	fmt.Fprintf(w, "  %-20s%12s%14s%14s  (stays on GitHub)\n", "actions storage", "", usd(r.Storage.GrossMicros), usd(r.Storage.NetMicros))
	fmt.Fprintf(w, "\nLinux x64 minutes that can move, per month:\n")
	fmt.Fprintf(w, "  %-9s%14s%13s%14s%14s\n", "month", "GitHub list", "GitHub paid", "Vitko list", "after free")
	for _, m := range r.Months {
		fmt.Fprintf(w, "  %-9s%14s%13s%14s%14s\n", m.Month, usd(m.GithubGrossMicros), usd(m.GithubNetMicros), usd(m.VitkoBeforeFreeMicros), usd(m.VitkoMicros))
	}
	t := r.Totals
	fmt.Fprintf(w, "\nTotal: GitHub list %s (you paid %s) vs Vitko list %s (%s after %s free minutes a month)\n",
		usd(t.GithubGrossMicros), usd(t.GithubNetMicros), usd(t.VitkoBeforeFreeMicros), usd(t.VitkoMicros), thousands(float64(r.FreeMinutesPerMonth)))
	if t.SavingVsGithubGross != nil {
		fmt.Fprintf(w, "List price vs list price: %.1f%% less\n", *t.SavingVsGithubGross*100)
	}
	if t.SavingVsGithubNet != nil {
		fmt.Fprintf(w, "Against what you actually paid GitHub: %.1f%% less (after the free minutes)\n", *t.SavingVsGithubNet*100)
	} else if t.SavingVsGithubNetUnavailable != nil && t.SavingVsGithubGross != nil {
		fmt.Fprintf(w, "You paid GitHub $0 for these minutes (inside your included minutes), so there is no saving on this usage.\n")
	}
	fmt.Fprintf(w, "\nAssumptions:\n")
	for _, a := range r.Assumptions {
		fmt.Fprintf(w, "  - %s\n", a)
	}
	if r.Repositories != nil {
		fmt.Fprintf(w, "\nLinux x64 minutes by repository:\n")
		for _, rp := range r.Repositories {
			fmt.Fprintf(w, "  %12s  %s\n", thousands(rp.Minutes), rp.Repository)
		}
	}
	fmt.Fprintf(w, "\nCaveats:\n")
	for i, c := range r.Caveats {
		fmt.Fprintf(w, "  %d. %s\n", i+1, c)
	}
}
