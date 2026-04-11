package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/spf13/cobra"
)

// --------------------------------------------------------------------------
// Flags
// --------------------------------------------------------------------------

var (
	statusSince time.Duration
	statusWatch bool
)

// statusCmd is the cobra subcommand for `dontguess status`.
var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Snapshot of wrapper log, exchange views, and operator health",
	Long: `Print a snapshot of:
  1. Wrapper attempt log summary ($DG_HOME/dontguess-attempts.log)
  2. Exchange view counts (buys, matches, settlements, puts, put-accepts, put-rejects)
  3. Operator health (pid, uptime, store size, last activity)
  4. Held-for-review count via unix socket

Flags:
  --since  window to aggregate (default 24h)
  --json   emit a single JSON object
  --watch  refresh every 5s with ANSI clear`,
	RunE: runStatus,
}

func init() {
	statusCmd.Flags().DurationVar(&statusSince, "since", 24*time.Hour, "time window to aggregate")
	statusCmd.Flags().BoolVar(&statusWatch, "watch", false, "refresh every 5s (Ctrl-C to exit)")
	rootCmd.AddCommand(statusCmd)
}

// --------------------------------------------------------------------------
// Snapshot struct
// --------------------------------------------------------------------------

// WrapperAttempts aggregates wrapper attempt log counts.
type WrapperAttempts struct {
	Total   int            `json:"total"`
	Success int            `json:"success"`
	Failed  int            `json:"failed"`
	ByTag   map[string]int `json:"by_tag"`
}

// ExchangeCounts holds the exchange view counts.
type ExchangeCounts struct {
	Buys          int  `json:"buys"`
	Matches       int  `json:"matches"`
	Settlements   int  `json:"settlements"`
	PutsSubmitted int  `json:"puts_submitted"`
	PutsAccepted  int  `json:"puts_accepted"`
	PutsRejected  int  `json:"puts_rejected"`
	PutsHeld      *int `json:"puts_held"`
	PutsHeldNote  string `json:"puts_held_note,omitempty"`
}

// OperatorHealth holds operator process health info.
type OperatorHealth struct {
	PID                  int   `json:"pid"`
	Alive                bool  `json:"alive"`
	UptimeSeconds        int64 `json:"uptime_seconds"`
	StoreSizeBytes       int64 `json:"store_size_bytes"`
	LastActivitySecondsAgo *int64 `json:"last_activity_seconds_ago"`
}

// StatusSnapshot is the full status snapshot returned by collectStatus.
type StatusSnapshot struct {
	SchemaVersion  int            `json:"schema_version"`
	Since          string         `json:"since"`
	WrapperAttempts WrapperAttempts `json:"wrapper_attempts"`
	Exchange       ExchangeCounts `json:"exchange"`
	Operator       OperatorHealth `json:"operator"`
}

// --------------------------------------------------------------------------
// dgHome resolver
// --------------------------------------------------------------------------

// resolveDGHome returns the DG_HOME directory: DG_HOME env if set, else ~/.cf.
func resolveDGHome() string {
	if dg := os.Getenv("DG_HOME"); dg != "" {
		return dg
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".cf"
	}
	return filepath.Join(home, ".cf")
}

// --------------------------------------------------------------------------
// Wrapper attempt log reader
// --------------------------------------------------------------------------

// attemptLine is a single line from dontguess-attempts.log.
// Schema matches the wrapper heredoc in site/install.sh — do NOT change the
// wrapper to match this struct; this struct must match the wrapper.
//
// Wrapper writes: {"ts":"<RFC3339>","pid":<int>,"cmd":"<str>","exit":<int>,
//                  "tag":"<str>","cf_home":"<str>","cwd":"<str>","caller":<null|"str">}
type attemptLine struct {
	TS     string  `json:"ts"`     // RFC3339 timestamp string (e.g. "2026-04-11T12:00:00Z")
	PID    int     `json:"pid"`
	Cmd    string  `json:"cmd"`
	Exit   int     `json:"exit"`
	Tag    string  `json:"tag"`
	CFHome string  `json:"cf_home"`
	CWD    string  `json:"cwd"`
	Caller *string `json:"caller"` // null when no caller identity is available
}

// readWrapperLog parses $dgHome/dontguess-attempts.log and aggregates counts
// for lines within the since window. Malformed lines are silently skipped.
//
// Success is determined by exit==0 and tag=="success", matching the wrapper's
// _classify_tag logic. All tag values are aggregated in ByTag without
// hard-coding the list — the wrapper emits: success, no_exchange_configured,
// operator_down, identity_wrapped, not_admitted, other.
func readWrapperLog(dgHome string, cutoff time.Time) WrapperAttempts {
	out := WrapperAttempts{ByTag: make(map[string]int)}
	path := filepath.Join(dgHome, "dontguess-attempts.log")
	f, err := os.Open(path)
	if err != nil {
		// File not present — zero counts, not an error.
		return out
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry attemptLine
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			// Malformed line — skip.
			continue
		}
		// Parse RFC3339 timestamp.
		ts, err := time.Parse(time.RFC3339, entry.TS)
		if err != nil {
			// Malformed timestamp — skip.
			continue
		}
		if ts.Before(cutoff) {
			continue
		}
		out.Total++
		if entry.Tag != "" {
			out.ByTag[entry.Tag]++
		}
		if entry.Exit == 0 && entry.Tag == "success" {
			out.Success++
		} else {
			out.Failed++
		}
	}
	return out
}

// --------------------------------------------------------------------------
// Exchange view reader
// --------------------------------------------------------------------------

// readExchangeViews reads counts from the exchange campfire views for messages
// within the since window. Returns zero counts on any error (not fatal for
// the overall status command).
func readExchangeViews(dgHome string, cutoff time.Time) (ExchangeCounts, error) {
	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		return ExchangeCounts{}, fmt.Errorf("load exchange config: %w", err)
	}

	client, _, err := protocol.Init(dgHome)
	if err != nil {
		return ExchangeCounts{}, fmt.Errorf("protocol.Init: %w", err)
	}
	defer client.Close()

	cutoffNano := cutoff.UnixNano()
	cfID := cfg.ExchangeCampfireID

	readTag := func(tag string) (int, int64) {
		req := protocol.ReadRequest{
			CampfireID: cfID,
			Tags:       []string{tag},
		}
		result, err := client.Read(req)
		if err != nil {
			return 0, 0
		}
		count := 0
		var maxTS int64
		for _, m := range result.Messages {
			if m.Timestamp >= cutoffNano {
				count++
			}
			if m.Timestamp > maxTS {
				maxTS = m.Timestamp
			}
		}
		return count, maxTS
	}

	buys, _ := readTag(exchange.TagBuy)
	matches, _ := readTag(exchange.TagMatch)
	settlements, _ := readTag(exchange.TagSettle)
	puts, _ := readTag(exchange.TagPut)
	putAccepts, maxPATS := readTag(exchange.TagPhasePrefix + exchange.SettlePhaseStrPutAccept)
	putRejects, maxPRTS := readTag(exchange.TagPhasePrefix + exchange.SettlePhaseStrPutReject)

	_ = maxPATS
	_ = maxPRTS

	return ExchangeCounts{
		Buys:          buys,
		Matches:       matches,
		Settlements:   settlements,
		PutsSubmitted: puts,
		PutsAccepted:  putAccepts,
		PutsRejected:  putRejects,
	}, nil
}

// readExchangeViewsWithClient is used in tests to pass in a pre-built client
// and store instead of calling protocol.Init.
func readExchangeViewsWithClient(client *protocol.Client, st store.Store, cfID string, cutoff time.Time) ExchangeCounts {
	cutoffNano := cutoff.UnixNano()

	readTag := func(tag string) (int, int64) {
		req := protocol.ReadRequest{
			CampfireID: cfID,
			Tags:       []string{tag},
			SkipSync:   true,
		}
		result, err := client.Read(req)
		if err != nil {
			return 0, 0
		}
		count := 0
		var maxTS int64
		for _, m := range result.Messages {
			if m.Timestamp >= cutoffNano {
				count++
			}
			if m.Timestamp > maxTS {
				maxTS = m.Timestamp
			}
		}
		return count, maxTS
	}

	buys, _ := readTag(exchange.TagBuy)
	matches, _ := readTag(exchange.TagMatch)
	settlements, _ := readTag(exchange.TagSettle)
	puts, _ := readTag(exchange.TagPut)
	putAccepts, _ := readTag(exchange.TagPhasePrefix + exchange.SettlePhaseStrPutAccept)
	putRejects, _ := readTag(exchange.TagPhasePrefix + exchange.SettlePhaseStrPutReject)

	return ExchangeCounts{
		Buys:          buys,
		Matches:       matches,
		Settlements:   settlements,
		PutsSubmitted: puts,
		PutsAccepted:  putAccepts,
		PutsRejected:  putRejects,
	}
}

// --------------------------------------------------------------------------
// Operator health
// --------------------------------------------------------------------------

// readOperatorHealth collects operator process health from $dgHome.
func readOperatorHealth(dgHome string, cutoff time.Time, msgLastActivity int64) OperatorHealth {
	h := OperatorHealth{}

	// Read PID file.
	pidPath := filepath.Join(dgHome, "dontguess.pid")
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		// No PID file → operator not running.
		return h
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil || pid <= 0 {
		return h
	}
	h.PID = pid

	// Verify PID is alive and belongs to the operator binary.
	h.Alive = pidAlive(pid)

	// Uptime: try /proc/<pid>/stat, fall back to pid file mtime.
	h.UptimeSeconds = processUptime(pid, pidPath)

	// Store size.
	dbPath := store.StorePath(dgHome)
	if info, err := os.Stat(dbPath); err == nil {
		h.StoreSizeBytes = info.Size()
	}

	// Last activity: max timestamp from exchange messages within since window.
	if msgLastActivity > 0 && msgLastActivity >= cutoff.UnixNano() {
		ago := time.Since(time.Unix(0, msgLastActivity)).Seconds()
		agoInt := int64(ago)
		h.LastActivitySecondsAgo = &agoInt
	}

	return h
}

// pidAlive returns true if pid is alive and the process is named
// "dontguess-operator" (or contains "dontguess").
func pidAlive(pid int) bool {
	// Try /proc/<pid>/comm first (Linux).
	commPath := fmt.Sprintf("/proc/%d/comm", pid)
	data, err := os.ReadFile(commPath)
	if err == nil {
		comm := strings.TrimSpace(string(data))
		// comm is truncated to 15 chars by kernel.
		return strings.Contains(comm, "dontguess") || comm == "dontguess-opera"
	}
	// Fallback: check if the process exists at all via signal 0.
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// processUptime returns the number of seconds the process has been running.
// It reads /proc/<pid>/stat to get the starttime in jiffies (clock ticks since boot),
// then reads /proc/stat "btime" for boot time, then computes elapsed.
// Falls back to pid file mtime if /proc is unavailable.
func processUptime(pid int, pidPath string) int64 {
	bootTime, err := readBootTime()
	if err == nil {
		startTicks, err := readProcessStartTicks(pid)
		if err == nil {
			clkTck := int64(100) // sysconf(_SC_CLK_TCK) on Linux — almost always 100
			startTime := time.Unix(bootTime+startTicks/clkTck, 0)
			return int64(time.Since(startTime).Seconds())
		}
	}
	// Fallback: use pid file mtime.
	info, err := os.Stat(pidPath)
	if err != nil {
		return 0
	}
	return int64(time.Since(info.ModTime()).Seconds())
}

// readBootTime parses /proc/stat for the "btime" field (boot time as unix epoch seconds).
func readBootTime() (int64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "btime ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return strconv.ParseInt(parts[1], 10, 64)
			}
		}
	}
	return 0, fmt.Errorf("btime not found in /proc/stat")
}

// readProcessStartTicks parses /proc/<pid>/stat and returns field 22 (starttime,
// 0-indexed as field 21) in clock ticks since boot.
func readProcessStartTicks(pid int) (int64, error) {
	statPath := fmt.Sprintf("/proc/%d/stat", pid)
	data, err := os.ReadFile(statPath)
	if err != nil {
		return 0, err
	}
	// The comm field (index 1) can contain spaces, so find the closing ')'.
	s := string(data)
	idx := strings.LastIndex(s, ")")
	if idx < 0 {
		return 0, fmt.Errorf("malformed /proc stat")
	}
	rest := strings.TrimSpace(s[idx+1:])
	// Fields after ')' are separated by spaces; field index 22 (1-based) is
	// field index 20 in this sub-slice (fields start at index 3 overall,
	// so 22-3=19 in the space-separated tail).
	fields := strings.Fields(rest)
	if len(fields) < 20 {
		return 0, fmt.Errorf("not enough fields in /proc stat")
	}
	return strconv.ParseInt(fields[19], 10, 64)
}

// --------------------------------------------------------------------------
// Held-for-review via socket
// --------------------------------------------------------------------------

// readHeldCount dials the operator socket and sends list-held.
// On failure, returns nil (means "operator not reachable").
func readHeldCount(dgHome string) (*int, string) {
	sockPath := filepath.Join(dgHome, "dontguess.sock")
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return nil, "operator not reachable"
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck

	enc := json.NewEncoder(conn)
	if err := enc.Encode(map[string]any{"op": "list-held"}); err != nil {
		return nil, "operator not reachable"
	}

	var resp struct {
		Puts []any `json:"puts"`
	}
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&resp); err != nil {
		return nil, "operator not reachable"
	}

	n := len(resp.Puts)
	return &n, ""
}

// --------------------------------------------------------------------------
// collectStatus
// --------------------------------------------------------------------------

// collectStatus gathers all data sources and returns a StatusSnapshot.
func collectStatus(dgHome string, since time.Duration) (*StatusSnapshot, error) {
	cutoff := time.Now().Add(-since)

	wa := readWrapperLog(dgHome, cutoff)

	excCounts, excErr := readExchangeViews(dgHome, cutoff)
	if excErr != nil {
		// Non-fatal: zero counts.
		excCounts = ExchangeCounts{}
	}

	// Held-for-review via socket.
	held, heldNote := readHeldCount(dgHome)
	excCounts.PutsHeld = held
	if heldNote != "" {
		excCounts.PutsHeldNote = heldNote
	}

	// lastActivity from exchange messages — collect max timestamp from all views.
	var lastActivityNano int64
	if excErr == nil {
		// Re-read to get raw timestamps for last_activity (the view reader
		// above discards the per-tag maxTS; we take the last_activity from the
		// operator health perspective as the max over all view messages).
		lastActivityNano = collectLastActivity(dgHome, cutoff)
	}

	op := readOperatorHealth(dgHome, cutoff, lastActivityNano)

	snap := &StatusSnapshot{
		SchemaVersion:   1,
		Since:           since.String(),
		WrapperAttempts: wa,
		Exchange:        excCounts,
		Operator:        op,
	}
	return snap, nil
}

// collectLastActivity returns the max timestamp (nanos) of any exchange message
// seen in the since window. Used for operator.last_activity_seconds_ago.
func collectLastActivity(dgHome string, cutoff time.Time) int64 {
	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		return 0
	}
	client, _, err := protocol.Init(dgHome)
	if err != nil {
		return 0
	}
	defer client.Close()

	cutoffNano := cutoff.UnixNano()
	allTags := []string{
		exchange.TagPut, exchange.TagBuy, exchange.TagMatch, exchange.TagSettle,
	}
	var maxTS int64
	for _, tag := range allTags {
		result, err := client.Read(protocol.ReadRequest{
			CampfireID: cfg.ExchangeCampfireID,
			Tags:       []string{tag},
		})
		if err != nil {
			continue
		}
		for _, m := range result.Messages {
			if m.Timestamp >= cutoffNano && m.Timestamp > maxTS {
				maxTS = m.Timestamp
			}
		}
	}
	return maxTS
}

// --------------------------------------------------------------------------
// Output
// --------------------------------------------------------------------------

// printStatus prints the snapshot in human-readable or JSON format.
func printStatus(snap *StatusSnapshot, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(snap) //nolint:errcheck
		return
	}

	fmt.Printf("=== dontguess status (last %s) ===\n\n", snap.Since)

	fmt.Printf("Wrapper attempts\n")
	fmt.Printf("  total:    %d\n", snap.WrapperAttempts.Total)
	fmt.Printf("  success:  %d\n", snap.WrapperAttempts.Success)
	fmt.Printf("  failed:   %d\n", snap.WrapperAttempts.Failed)
	if len(snap.WrapperAttempts.ByTag) > 0 {
		fmt.Printf("  by tag:\n")
		for tag, n := range snap.WrapperAttempts.ByTag {
			fmt.Printf("    %-30s  %d\n", tag, n)
		}
	}
	fmt.Println()

	fmt.Printf("Exchange\n")
	fmt.Printf("  buys:           %d\n", snap.Exchange.Buys)
	fmt.Printf("  matches:        %d\n", snap.Exchange.Matches)
	fmt.Printf("  settlements:    %d\n", snap.Exchange.Settlements)
	fmt.Printf("  puts submitted: %d\n", snap.Exchange.PutsSubmitted)
	fmt.Printf("  puts accepted:  %d\n", snap.Exchange.PutsAccepted)
	fmt.Printf("  puts rejected:  %d\n", snap.Exchange.PutsRejected)
	if snap.Exchange.PutsHeld != nil {
		fmt.Printf("  puts held:      %d\n", *snap.Exchange.PutsHeld)
	} else {
		fmt.Printf("  puts held:      n/a (%s)\n", snap.Exchange.PutsHeldNote)
	}
	fmt.Println()

	fmt.Printf("Operator\n")
	if snap.Operator.Alive {
		fmt.Printf("  status:      alive (pid %d)\n", snap.Operator.PID)
		fmt.Printf("  uptime:      %ds\n", snap.Operator.UptimeSeconds)
	} else {
		fmt.Printf("  status:      not running\n")
	}
	fmt.Printf("  store size:  %d bytes\n", snap.Operator.StoreSizeBytes)
	if snap.Operator.LastActivitySecondsAgo != nil {
		fmt.Printf("  last active: %ds ago\n", *snap.Operator.LastActivitySecondsAgo)
	} else {
		fmt.Printf("  last active: n/a\n")
	}
}

// --------------------------------------------------------------------------
// Command runner
// --------------------------------------------------------------------------

func runStatus(_ *cobra.Command, _ []string) error {
	dgHome := resolveDGHome()

	renderOnce := func() error {
		snap, err := collectStatus(dgHome, statusSince)
		if err != nil {
			return err
		}
		printStatus(snap, jsonOutput)
		return nil
	}

	if !statusWatch {
		return renderOnce()
	}

	// Watch mode: clear screen + re-render every 5s, exit on SIGINT.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Render immediately before first tick.
	fmt.Print("\x1b[2J\x1b[H")
	if err := renderOnce(); err != nil {
		return err
	}

	for {
		select {
		case <-sig:
			return nil
		case <-ticker.C:
			fmt.Print("\x1b[2J\x1b[H")
			if err := renderOnce(); err != nil {
				return err
			}
		}
	}
}
