// Package pricing holds the public Vitko Runners price list, compiled in and dated.
package pricing

// AsOf is the date of the prices below.
const AsOf = "2026-09-27"

// SlotMicrosPerMinute is the list price of one slot-minute in USD micros ($0.002).
const SlotMicrosPerMinute int64 = 2000

// FreeMinutesPerMonth are free slot-minutes each calendar month (UTC).
const FreeMinutesPerMonth = 2000

// GithubLinux2CoreMicrosPerMinute is GitHub's list price for a standard
// 2-core Linux x64 hosted runner ($0.006/min), the comparison basis.
const GithubLinux2CoreMicrosPerMinute int64 = 6000

// Label is the runs-on label for a Vitko runner.
const Label = "vitko-ubuntu-24.04"

// Doc is vitko.runners.pricing/v1.
type Doc struct {
	Schema              string     `json:"schema"`
	Product             string     `json:"product"`
	PricesAsOf          string     `json:"prices_as_of"`
	Currency            string     `json:"currency"`
	Slot                Slot       `json:"slot"`
	PriceMicrosPerMin   int64      `json:"price_micros_per_slot_minute"`
	Billing             Billing    `json:"billing"`
	FreeMinutesPerMonth int        `json:"free_minutes_per_month"`
	Comparison          Comparison `json:"comparison"`
	Notes               []string   `json:"notes"`
	URL                 string     `json:"url"`
}

// Slot is the unit of capacity a price applies to.
type Slot struct {
	RunsOn      string `json:"runs_on"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	VCPUsMax    int    `json:"vcpus_max"`
	MemoryBytes int64  `json:"memory_bytes_max"`
}

// Billing describes how time is metered.
type Billing struct {
	IncrementSeconds int    `json:"increment_seconds"`
	Description      string `json:"description"`
}

// Comparison is the dated GitHub basis for price claims.
type Comparison struct {
	GithubMicrosPerMinute int64  `json:"github_micros_per_minute"`
	Basis                 string `json:"basis"`
	Source                string `json:"source"`
	Ratio                 string `json:"ratio"`
	PricesAsOf            string `json:"prices_as_of"`
}

// Current returns today's price list.
func Current() Doc {
	return Doc{
		Schema:     "vitko.runners.pricing/v1",
		Product:    "Vitko Runners",
		PricesAsOf: AsOf,
		Currency:   "USD",
		Slot: Slot{
			RunsOn: Label, OS: "Ubuntu 24.04", Arch: "x64",
			VCPUsMax: 2, MemoryBytes: 8 << 30,
		},
		PriceMicrosPerMin: SlotMicrosPerMinute,
		Billing: Billing{
			IncrementSeconds: 1,
			Description:      "Billed per second of job time, not rounded up to the minute.",
		},
		FreeMinutesPerMonth: FreeMinutesPerMonth,
		Comparison: Comparison{
			GithubMicrosPerMinute: GithubLinux2CoreMicrosPerMinute,
			Basis:                 "GitHub's list price for a standard 2-core Linux x64 hosted runner, which GitHub rounds up to whole minutes per job.",
			Source:                "https://docs.github.com/en/billing/reference/actions-runner-pricing",
			Ratio:                 "A third of GitHub's price.",
			PricesAsOf:            AsOf,
		},
		Notes: []string{
			"Run `vitko runners estimate` on your GitHub usage report to see what your own CI would cost.",
		},
		URL: "https://runners.vitko.inc",
	}
}
