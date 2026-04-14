package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

// socketPath returns the path to the operator unix domain socket.
// Delegates to resolveDGHome (dgpath.go) — single source of truth for
// DG_HOME resolution (dontguess-435). The socket lives in a 0700 "ipc"
// subdirectory (dontguess-33a) to bound the TOCTOU window at the
// parent-dir level instead of relying on process-global umask tricks.
func socketPath() string {
	return filepath.Join(resolveDGHome(), "ipc", "dontguess.sock")
}

// dialSocket dials the operator socket and returns the connection.
// On failure it prints the standard "not reachable" message to stderr and exits 1.
func dialSocket() net.Conn {
	conn, err := net.Dial("unix", socketPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "dontguess operator: operator not reachable (is dontguess-operator running?)")
		os.Exit(1)
	}
	return conn
}

// sendRequest sends a JSON request over conn and decodes the response into dst.
func sendRequest(conn net.Conn, req map[string]any, dst any) error {
	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	dec := json.NewDecoder(conn)
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	return nil
}

// HeldPutEntry is the JSON shape returned by list-held.
type HeldPutEntry struct {
	PutMsgID  string `json:"put_msg_id"`
	TokenCost int64  `json:"token_cost"`
	Seller    string `json:"seller"`
}

// listHeldResponse is the full response shape for list-held.
type listHeldResponse struct {
	Puts []HeldPutEntry `json:"puts"`
}

// okResponse is the response shape for accept-put and reject-put.
type okResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// short returns the first 12 chars of s, or s itself if shorter.
func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// --- list-held ---

var listHeldCmd = &cobra.Command{
	Use:   "list-held",
	Short: "List puts held for review",
	RunE: func(cmd *cobra.Command, args []string) error {
		conn := dialSocket()
		defer conn.Close()

		var resp listHeldResponse
		if err := sendRequest(conn, map[string]any{"op": OpListHeld}, &resp); err != nil {
			return err
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(resp)
		}

		if len(resp.Puts) == 0 {
			fmt.Println("No puts held for review.")
			return nil
		}
		fmt.Printf("%-14s  %12s  %s\n", "PutMsgID", "TokenCost", "Seller")
		fmt.Printf("%-14s  %12s  %s\n", "---------", "---------", "------")
		for _, p := range resp.Puts {
			fmt.Printf("%-14s  %12d  %s\n", short(p.PutMsgID), p.TokenCost, short(p.Seller))
		}
		return nil
	},
}

// --- accept-put ---

var (
	acceptPutPrice   int64
	acceptPutExpires string
)

var acceptPutCmd = &cobra.Command{
	Use:   "accept-put <put-msg-id>",
	Short: "Accept a put held for review",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		putMsgID := args[0]

		price := acceptPutPrice
		expiresStr := acceptPutExpires

		// If price not supplied we need to get the token_cost first.
		if price == 0 || expiresStr == "" {
			conn := dialSocket()
			var listResp listHeldResponse
			if err := sendRequest(conn, map[string]any{"op": OpListHeld}, &listResp); err != nil {
				conn.Close()
				return err
			}
			conn.Close()

			found := false
			for _, p := range listResp.Puts {
				if p.PutMsgID == putMsgID {
					if price == 0 {
						price = p.TokenCost * 70 / 100
					}
					if expiresStr == "" {
						expiresStr = time.Now().UTC().Add(72 * time.Hour).Format(time.RFC3339)
					}
					found = true
					break
				}
			}
			// If not found, the put may have already been processed. Do NOT send
			// accept-put with price=0 — that would list the content at zero cost.
			if !found {
				return fmt.Errorf("put %q not found in held-for-review (may have already been processed)", putMsgID)
			}
		}

		conn := dialSocket()
		defer conn.Close()

		var resp okResponse
		if err := sendRequest(conn, map[string]any{
			"op":         OpAcceptPut,
			"put_msg_id": putMsgID,
			"price":      price,
			"expires":    expiresStr,
		}, &resp); err != nil {
			return err
		}

		if !resp.OK {
			return fmt.Errorf("accept-put failed: %s", resp.Error)
		}
		fmt.Printf("accepted put %s at price %d, expires %s\n", short(putMsgID), price, expiresStr)
		return nil
	},
}

// --- reject-put ---

var rejectPutReason string

var rejectPutCmd = &cobra.Command{
	Use:   "reject-put <put-msg-id>",
	Short: "Reject a put held for review",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		putMsgID := args[0]

		if rejectPutReason == "" {
			return fmt.Errorf("--reason is required")
		}

		conn := dialSocket()
		defer conn.Close()

		var resp okResponse
		if err := sendRequest(conn, map[string]any{
			"op":         OpRejectPut,
			"put_msg_id": putMsgID,
			"reason":     rejectPutReason,
		}, &resp); err != nil {
			return err
		}

		if !resp.OK {
			return fmt.Errorf("reject-put failed: %s", resp.Error)
		}
		fmt.Printf("rejected put %s\n", short(putMsgID))
		return nil
	},
}

// --- operatorCmd ---

var operatorCmd = &cobra.Command{
	Use:   "operator",
	Short: "Operator management commands",
}

func init() {
	// accept-put flags
	acceptPutCmd.Flags().Int64Var(&acceptPutPrice, "price", 0, "price in scrip (default: 70%% of token_cost)")
	acceptPutCmd.Flags().StringVar(&acceptPutExpires, "expires", "", "expiry time in RFC3339 (default: now+72h)")

	// reject-put flags
	rejectPutCmd.Flags().StringVar(&rejectPutReason, "reason", "", "reason for rejection (required)")

	operatorCmd.AddCommand(listHeldCmd)
	operatorCmd.AddCommand(acceptPutCmd)
	operatorCmd.AddCommand(rejectPutCmd)

	rootCmd.AddCommand(operatorCmd)
}
