package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// --------------------------------------------------------------------------
// `dontguess savings` — net token-savings + fiat valuation.
//
// The value proposition of the exchange is not "tokens sold" — it is how many
// real inference tokens REUSE has taken off the bill, and what that is worth in
// fiat. Two facts the raw sold-tokens number hides, and that this report exists
// to surface:
//
//  1. What was sold != what it saves. A put's token_cost is the cost to
//     GENERATE the work. Reuse avoids regenerating it (large) and only pays to
//     read it back into context (small). The gap is the saving.
//  2. Input and output tokens price very differently (Opus 4.8: $5 vs $25 per
//     MTok — output is 5x). Avoided regeneration is a mix of input+output;
//     consumption is pure input. So fiat is NOT tokens x one rate — we split
//     every avoided regeneration and value the parts separately.
//
// REALIZED savings (headline) come from matches that actually delivered another
// agent's work to a different buyer — money already off the bill. LATENT
// savings value stocked-but-unused inventory at one reuse each, kept strictly
// separate. Scrip is a coordination token, never counted as an API cost.
// --------------------------------------------------------------------------

// modelRates maps a model preset to (input, output) USD per million tokens.
var modelRates = map[string][2]float64{
	"opus-4.8":   {5.0, 25.0},
	"sonnet-4.6": {3.0, 15.0},
	"haiku-4.5":  {1.0, 5.0},
	"fable-5":    {10.0, 50.0},
}

const (
	defaultInRate        = 5.0  // Opus 4.8 input $/MTok
	defaultOutRate       = 25.0 // Opus 4.8 output $/MTok
	defaultGenOutputFrac = 0.30 // fraction of a generation's tokens that were output
	defaultConsumeFrac   = 0.05 // consumption cost as fraction of avoided regeneration
	junkTokenCostFloor   = 500  // token_cost below this is a smoke/test put
)

var (
	savingsSince      time.Duration
	savingsModel      string
	savingsInRate     float64
	savingsOutRate    float64
	savingsGenOutFrac float64
	savingsConsFrac   float64
)

var savingsCmd = &cobra.Command{
	Use:   "savings",
	Short: "Net token-savings from reuse, valued in fiat (input/output aware)",
	Long: `Report NET inference-token savings from reuse — not gross "tokens sold".

REALIZED savings (headline) come from matches that delivered another agent's
work to a different buyer: avoided regeneration (token_cost_original) minus a
small consumption cost. LATENT savings value stocked inventory at one reuse
each. Scrip is excluded — it is a coordination token, not an API cost.

Fiat is not tokens x one rate: avoided regeneration splits into cheap input and
expensive output tokens (Opus 4.8: $5 vs $25/MTok), valued separately;
consumption is pure input.

Tune the valuation:
  --model opus-4.8|sonnet-4.6|haiku-4.5|fable-5   preset in/out rates
  --in-rate / --out-rate                          override $/MTok directly
  --gen-output-frac (default 0.30)                output share of a generation
  --consume-frac    (default 0.05)                cost to read cached work back
  --since           (default 0 = all time)        window to aggregate
  --json                                          machine-readable output`,
	RunE: runSavings,
}

func init() {
	savingsCmd.Flags().DurationVar(&savingsSince, "since", 0, "window to aggregate (0 = all time)")
	savingsCmd.Flags().StringVar(&savingsModel, "model", "", "rate preset: opus-4.8|sonnet-4.6|haiku-4.5|fable-5")
	savingsCmd.Flags().Float64Var(&savingsInRate, "in-rate", defaultInRate, "input $/MTok")
	savingsCmd.Flags().Float64Var(&savingsOutRate, "out-rate", defaultOutRate, "output $/MTok")
	savingsCmd.Flags().Float64Var(&savingsGenOutFrac, "gen-output-frac", defaultGenOutputFrac, "output share of a generation's tokens")
	savingsCmd.Flags().Float64Var(&savingsConsFrac, "consume-frac", defaultConsumeFrac, "consumption cost as fraction of avoided regeneration")
	rootCmd.AddCommand(savingsCmd)
}

// --------------------------------------------------------------------------
// Report structs (also the --json shape)
// --------------------------------------------------------------------------

// SavingsReport is the full report returned by collectSavings.
type SavingsReport struct {
	SchemaVersion int             `json:"schema_version"`
	Window        savingsWindow   `json:"window"`
	Valuation     valuationParams `json:"valuation_assumptions"`
	Activity      savingsActivity `json:"activity"`
	Realized      realizedSavings `json:"realized_savings"`
	Latent        latentSavings   `json:"latent_savings"`
	Economy       internalEconomy `json:"internal_economy"`
}

type savingsWindow struct {
	FirstEvent string  `json:"first_event"`
	LastEvent  string  `json:"last_event"`
	SpanHours  float64 `json:"span_hours"`
}

type valuationParams struct {
	InRate        float64 `json:"model_in_rate_usd_per_mtok"`
	OutRate       float64 `json:"model_out_rate_usd_per_mtok"`
	GenOutputFrac float64 `json:"gen_output_frac"`
	ConsumeFrac   float64 `json:"consume_frac"`
	Note          string  `json:"note"`
}

type savingsActivity struct {
	Ops                  map[string]int `json:"ops"`
	DistinctParticipants int            `json:"distinct_participants"`
	DistinctSellers      int            `json:"distinct_sellers"`
	PutsSubstantive      int            `json:"puts_substantive"`
	PutsJunkUnder500     int            `json:"puts_junk_under_500"`
	BuysReal             int            `json:"buys_real"`
	BuysSynthetic        int            `json:"buys_synthetic"`
	MissesReal           int            `json:"misses_real"`
}

type reuseDetail struct {
	When        string   `json:"when"`
	Buyer       string   `json:"buyer"`
	Seller      string   `json:"seller"`
	AvoidTokens int64    `json:"avoided_tokens"`
	PriceScrip  int64    `json:"price_scrip"`
	Similarity  *float64 `json:"similarity"`
	Description string   `json:"description"`
}

type realizedSavings struct {
	ReuseEvents        int           `json:"reuse_events"`
	TrivialExcluded    int           `json:"trivial_or_self_reuses_excluded"`
	RealContentHitRate float64       `json:"real_content_hit_rate"`
	AvoidedRegenTokens int64         `json:"avoided_regeneration_tokens"`
	NetTokensSaved     int64         `json:"net_tokens_saved"`
	NetFiatSavedUSD    float64       `json:"net_fiat_saved_usd"`
	Detail             []reuseDetail `json:"detail"`
}

type latentSavings struct {
	Note                       string  `json:"note"`
	InventoryEntries           int     `json:"inventory_entries"`
	InventoryAvoidableTokens   int64   `json:"inventory_avoided_regeneration_tokens"`
	NetTokensIfEachReusedOnce  int64   `json:"net_tokens_saved_if_each_reused_once"`
	NetFiatIfEachReusedOnceUSD float64 `json:"net_fiat_saved_if_each_reused_once_usd"`
}

type internalEconomy struct {
	Note                   string `json:"note"`
	ScripMinted            int64  `json:"scrip_minted"`
	CompressionRewardScrip int64  `json:"compression_reward_scrip_posted"`
	ReuseScripPaidByBuyers int64  `json:"reuse_scrip_paid_by_buyers"`
}

// --------------------------------------------------------------------------
// Payload parsing helpers
// --------------------------------------------------------------------------

// flexInt parses a JSON number that may be encoded as either a bare number
// (42000) or a quoted string ("42000"). Puts send token_cost as a string via
// the CLI; match results marshal token_cost_original as an int64.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*f = flexInt(v)
	return nil
}

type putPayloadJSON struct {
	TokenCost   flexInt `json:"token_cost"`
	ContentType string  `json:"content_type"`
	Description string  `json:"description"`
}

type buyPayloadJSON struct {
	Task string `json:"task"`
}

type matchResultJSON struct {
	EntryID           string  `json:"entry_id"`
	SellerKey         string  `json:"seller_key"`
	Price             int64   `json:"price"`
	Similarity        float64 `json:"similarity"`
	TokenCostOriginal flexInt `json:"token_cost_original"`
	Description       string  `json:"description"`
}

type matchPayloadJSON struct {
	Results    []matchResultJSON `json:"results"`
	SearchMeta struct {
		TotalCandidates int `json:"total_candidates"`
	} `json:"search_meta"`
}

type mintPayloadJSON struct {
	Amount flexInt `json:"amount"`
}

type assignPayloadJSON struct {
	TaskType string  `json:"task_type"`
	Reward   flexInt `json:"reward"`
}

// isSyntheticTask reports whether a buy task is operator/synthetic traffic
// (heartbeat keepalives, smoke tests) rather than a real content request.
func isSyntheticTask(task string) bool {
	t := strings.ToLower(task)
	return strings.Contains(t, "heartbeat") || strings.Contains(t, "keepalive") || strings.Contains(t, "smoke")
}

// valueReuse values one avoided regeneration of tc tokens, splitting it into
// input+output and subtracting a pure-input consumption cost.
func valueReuse(tc int64, gof, cf, inRate, outRate float64) (netTokens float64, netFiat float64) {
	t := float64(tc)
	genOut := t * gof
	genIn := t * (1.0 - gof)
	consume := t * cf
	netIn := genIn - consume
	netOut := genOut
	netTokens = netIn + netOut
	netFiat = netIn*inRate/1e6 + netOut*outRate/1e6
	return
}

// --------------------------------------------------------------------------
// collectSavings
// --------------------------------------------------------------------------

func collectSavings(dgHome string, since time.Duration, inRate, outRate, gof, cf float64) (*SavingsReport, error) {
	msgs, err := loadLocalMessages(dgHome)
	if err != nil {
		return nil, fmt.Errorf("loading local store: %w", err)
	}

	var cutoffNano int64
	if since > 0 {
		cutoffNano = time.Now().Add(-since).UnixNano()
	}

	// Index every message by ID for antecedent (buyer) resolution. This map is
	// built over ALL messages, unfiltered by window, so a match near the window
	// edge can still find the buy it answers.
	byID := make(map[string]*exchangeMsg, len(msgs))
	for i := range msgs {
		byID[msgs[i].ID] = &exchangeMsg{sender: msgs[i].Sender, tags: msgs[i].Tags, payload: msgs[i].Payload}
	}

	ops := map[string]int{}
	participants := map[string]struct{}{}
	sellers := map[string]struct{}{}
	var firstNano, lastNano int64

	var substPuts, junkPuts int
	var inventoryTokens int64
	var buysReal, buysSynthetic, missesReal int
	var scripMinted, compressionReward, reuseScripPaid int64

	realReuses := []reuseDetail{}
	trivialReuses := 0

	primaryTag := func(tags []string) string {
		if len(tags) == 0 {
			return "<none>"
		}
		return tags[0]
	}

	for i := range msgs {
		m := &msgs[i]
		if cutoffNano > 0 && m.Timestamp < cutoffNano {
			continue
		}
		if firstNano == 0 || m.Timestamp < firstNano {
			firstNano = m.Timestamp
		}
		if m.Timestamp > lastNano {
			lastNano = m.Timestamp
		}
		participants[shortID(m.Sender)] = struct{}{}
		tag := primaryTag(m.Tags)
		ops[tag]++

		switch {
		case hasTag(m.Tags, "exchange:put"):
			var p putPayloadJSON
			_ = json.Unmarshal(m.Payload, &p)
			sellers[shortID(m.Sender)] = struct{}{}
			if int64(p.TokenCost) >= junkTokenCostFloor {
				substPuts++
				inventoryTokens += int64(p.TokenCost)
			} else {
				junkPuts++
			}
		case hasTag(m.Tags, "exchange:buy") && !hasTag(m.Tags, "exchange:buy-miss"):
			var p buyPayloadJSON
			_ = json.Unmarshal(m.Payload, &p)
			if isSyntheticTask(p.Task) {
				buysSynthetic++
			} else {
				buysReal++
			}
		case hasTag(m.Tags, "exchange:buy-miss"):
			var p buyPayloadJSON
			_ = json.Unmarshal(m.Payload, &p)
			if !isSyntheticTask(p.Task) {
				missesReal++
			}
		case hasTag(m.Tags, "exchange:match"):
			var p matchPayloadJSON
			if err := json.Unmarshal(m.Payload, &p); err != nil {
				continue
			}
			buyer := resolveBuyer(m.Antecedents, byID)
			for _, res := range p.Results {
				seller := shortID(res.SellerKey)
				crossAgent := seller != "" && buyer != "" && seller != buyer
				if crossAgent && p.SearchMeta.TotalCandidates > 1 {
					sim := res.Similarity
					realReuses = append(realReuses, reuseDetail{
						When:        time.Unix(0, m.Timestamp).UTC().Format(time.RFC3339),
						Buyer:       buyer,
						Seller:      seller,
						AvoidTokens: int64(res.TokenCostOriginal),
						PriceScrip:  res.Price,
						Similarity:  &sim,
						Description: truncate(res.Description, 80),
					})
					reuseScripPaid += res.Price
				} else {
					trivialReuses++
				}
			}
		case hasTag(m.Tags, "dontguess:scrip-mint") || hasTag(m.Tags, "dontguess:scrip-buy-hold"):
			var p mintPayloadJSON
			_ = json.Unmarshal(m.Payload, &p)
			scripMinted += int64(p.Amount)
		case hasTag(m.Tags, "exchange:assign"):
			var p assignPayloadJSON
			_ = json.Unmarshal(m.Payload, &p)
			if p.TaskType == "compress" {
				compressionReward += int64(p.Reward)
			}
		}
	}

	// Realized headline.
	var realizedTokens, realizedFiat float64
	var realizedAvoided int64
	for _, x := range realReuses {
		nt, nf := valueReuse(x.AvoidTokens, gof, cf, inRate, outRate)
		realizedTokens += nt
		realizedFiat += nf
		realizedAvoided += x.AvoidTokens
	}
	hitRate := 0.0
	if buysReal > 0 {
		hitRate = float64(len(realReuses)) / float64(buysReal)
	}

	// Latent (inventory reused once each).
	latentTokens, latentFiat := valueReuse(inventoryTokens, gof, cf, inRate, outRate)

	span := 0.0
	first, last := "", ""
	if firstNano > 0 {
		first = time.Unix(0, firstNano).UTC().Format(time.RFC3339)
		last = time.Unix(0, lastNano).UTC().Format(time.RFC3339)
		span = time.Duration(lastNano - firstNano).Hours()
	}

	return &SavingsReport{
		SchemaVersion: 1,
		Window:        savingsWindow{FirstEvent: first, LastEvent: last, SpanHours: round1(span)},
		Valuation: valuationParams{
			InRate: inRate, OutRate: outRate, GenOutputFrac: gof, ConsumeFrac: cf,
			Note: "avoided regeneration split into input+output and valued separately; " +
				"consumption is pure input; scrip/price excluded (internal economy).",
		},
		Activity: savingsActivity{
			Ops:                  ops,
			DistinctParticipants: len(participants),
			DistinctSellers:      len(sellers),
			PutsSubstantive:      substPuts,
			PutsJunkUnder500:     junkPuts,
			BuysReal:             buysReal,
			BuysSynthetic:        buysSynthetic,
			MissesReal:           missesReal,
		},
		Realized: realizedSavings{
			ReuseEvents:        len(realReuses),
			TrivialExcluded:    trivialReuses,
			RealContentHitRate: round3(hitRate),
			AvoidedRegenTokens: realizedAvoided,
			NetTokensSaved:     int64(realizedTokens + 0.5),
			NetFiatSavedUSD:    round2(realizedFiat),
			Detail:             realReuses,
		},
		Latent: latentSavings{
			Note:                       "value of stocked inventory IF each entry is reused once; upper-bound pool, not realized.",
			InventoryEntries:           substPuts,
			InventoryAvoidableTokens:   inventoryTokens,
			NetTokensIfEachReusedOnce:  int64(latentTokens + 0.5),
			NetFiatIfEachReusedOnceUSD: round2(latentFiat),
		},
		Economy: internalEconomy{
			Note:                   "scrip is a coordination token, not an API cost; shown for completeness only.",
			ScripMinted:            scripMinted,
			CompressionRewardScrip: compressionReward,
			ReuseScripPaidByBuyers: reuseScripPaid,
		},
	}, nil
}

// exchangeMsg is the minimal slice of a message needed for antecedent lookup.
type exchangeMsg struct {
	sender  string
	tags    []string
	payload []byte
}

// resolveBuyer walks a match's antecedents to the buy event it answers and
// returns that buy's sender (the real buyer). The match itself is emitted by
// the operator, so the match sender is NOT the buyer.
func resolveBuyer(antecedents []string, byID map[string]*exchangeMsg) string {
	for _, a := range antecedents {
		if ae, ok := byID[a]; ok && hasTag(ae.tags, "exchange:buy") && !hasTag(ae.tags, "exchange:buy-miss") {
			return shortID(ae.sender)
		}
	}
	return ""
}

func shortID(hex string) string {
	if len(hex) <= 12 {
		return hex
	}
	return hex[:12]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }
func round3(f float64) float64 { return float64(int64(f*1000+0.5)) / 1000 }

// --------------------------------------------------------------------------
// Output
// --------------------------------------------------------------------------

func printSavings(rep *SavingsReport, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return
	}
	bar := strings.Repeat("=", 72)
	fmt.Println(bar)
	fmt.Println("  DontGuess — Token-Savings Report")
	fmt.Println(bar)
	fmt.Printf("  window        : %s  ->  %s\n", rep.Window.FirstEvent, rep.Window.LastEvent)
	fmt.Printf("                  (%.1f h of activity)\n", rep.Window.SpanHours)
	fmt.Printf("  valued at     : $%g/MTok in, $%g/MTok out   (gen_output_frac=%g, consume_frac=%g)\n",
		rep.Valuation.InRate, rep.Valuation.OutRate, rep.Valuation.GenOutputFrac, rep.Valuation.ConsumeFrac)
	fmt.Println()
	fmt.Println("-- Activity " + strings.Repeat("-", 60))
	fmt.Printf("  participants  : %d keys (%d sellers stocked inventory)\n",
		rep.Activity.DistinctParticipants, rep.Activity.DistinctSellers)
	fmt.Printf("  inventory     : %d substantive puts, %d junk (<500)\n",
		rep.Activity.PutsSubstantive, rep.Activity.PutsJunkUnder500)
	fmt.Printf("  buys          : %d real  +  %d synthetic(heartbeat)\n",
		rep.Activity.BuysReal, rep.Activity.BuysSynthetic)
	fmt.Printf("  ops           : %s\n", formatOps(rep.Activity.Ops))
	fmt.Println()
	fmt.Println("== REALIZED NET SAVINGS (already off the bill) " + strings.Repeat("=", 25))
	fmt.Printf("  reuse events        : %d   (real-content hit rate %.0f%%; %d trivial/self excluded)\n",
		rep.Realized.ReuseEvents, rep.Realized.RealContentHitRate*100, rep.Realized.TrivialExcluded)
	fmt.Printf("  avoided regeneration: %s tokens\n", commaInt(rep.Realized.AvoidedRegenTokens))
	fmt.Printf("  NET TOKENS SAVED    : %s\n", commaInt(rep.Realized.NetTokensSaved))
	fmt.Printf("  NET FIAT SAVED      : $%s\n", commaFloat(rep.Realized.NetFiatSavedUSD))
	if len(rep.Realized.Detail) > 0 {
		fmt.Println("  breakdown:")
		for _, d := range rep.Realized.Detail {
			sim := "n/a"
			if d.Similarity != nil {
				sim = fmt.Sprintf("%.3f", *d.Similarity)
			}
			when := d.When
			if len(when) > 16 {
				when = when[:16]
			}
			fmt.Printf("    - %s  %s tok  sim=%s  %s->%s\n", when, commaInt(d.AvoidTokens), sim, d.Seller, d.Buyer)
			fmt.Printf("        %s\n", d.Description)
		}
	}
	fmt.Println()
	fmt.Println("-- Latent savings (inventory on the shelf) " + strings.Repeat("-", 29))
	fmt.Printf("  %d entries = %s avoidable tokens\n", rep.Latent.InventoryEntries, commaInt(rep.Latent.InventoryAvoidableTokens))
	fmt.Printf("  if each reused once: %s tokens = $%s\n",
		commaInt(rep.Latent.NetTokensIfEachReusedOnce), commaFloat(rep.Latent.NetFiatIfEachReusedOnceUSD))
	fmt.Println()
	fmt.Println("-- Internal economy (not token savings) " + strings.Repeat("-", 32))
	fmt.Printf("  scrip minted=%s  compression-reward scrip posted=%s  reuse scrip paid=%s\n",
		commaInt(rep.Economy.ScripMinted), commaInt(rep.Economy.CompressionRewardScrip), commaInt(rep.Economy.ReuseScripPaidByBuyers))
	fmt.Println(bar)
}

func formatOps(ops map[string]int) string {
	keys := make([]string, 0, len(ops))
	for k := range ops {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return ops[keys[i]] > ops[keys[j]] })
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, ops[k]))
	}
	return strings.Join(parts, ", ")
}

// commaInt formats an int64 with thousands separators.
func commaInt(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// commaFloat formats a float with two decimals and thousands separators.
func commaFloat(f float64) string {
	whole := int64(f)
	frac := int64((f-float64(whole))*100 + 0.5)
	return fmt.Sprintf("%s.%02d", commaInt(whole), frac)
}

// --------------------------------------------------------------------------
// Runner
// --------------------------------------------------------------------------

func runSavings(cmd *cobra.Command, _ []string) error {
	inRate, outRate := savingsInRate, savingsOutRate
	if savingsModel != "" {
		r, ok := modelRates[savingsModel]
		if !ok {
			return fmt.Errorf("unknown --model %q (choose opus-4.8|sonnet-4.6|haiku-4.5|fable-5)", savingsModel)
		}
		// --model sets rates unless the user also passed explicit --in-rate/--out-rate.
		if !cmd.Flags().Changed("in-rate") {
			inRate = r[0]
		}
		if !cmd.Flags().Changed("out-rate") {
			outRate = r[1]
		}
	}

	dgHome := resolveDGHome()
	rep, err := collectSavings(dgHome, savingsSince, inRate, outRate, savingsGenOutFrac, savingsConsFrac)
	if err != nil {
		return err
	}
	printSavings(rep, jsonOutput)
	return nil
}
