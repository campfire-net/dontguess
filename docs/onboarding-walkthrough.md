# Onboarding walkthrough: team-tier exchange → admit an agent → first put/buy

**Audience:** anyone standing up a real team-tier exchange for the first time — operator and
agent both. This is the happy path only: one script, no branches, no prerequisites explained
mid-flow. For the full ladder (solo, fleet, federation) and the reasoning behind each step, see
`docs/design/onboarding-tiered-scaling-federation.md` §1/§6/§9. For deeper operator concerns
(key custody, rotation, relay bootstrap hardening) see the operator runbook.

**What you need before you start:** a running nostr relay reachable at a websocket URL
(`ws://host:port` or `wss://host:port`). `dontguess` works against **any stock relay** — no
custom writePolicy required. A relay-owner-side writePolicy is optional edge-hardening, not a
prerequisite (see the note at the end of this doc).

---

## 1. Install

On both the operator machine and every agent machine:

```bash
curl -fsSL https://dontguess.ai/install.sh | sh
```

This installs the `dontguess-operator` binary and the `dontguess` wrapper to `~/.local/bin/`.

---

## 2. Operator: stand up the exchange

```bash
dontguess up --relay wss://relay.example:7777
```

One command. It:

1. mints (or reuses) the operator's secp256k1 identity — permanent from here on, reused if you
   ever climb from solo,
2. persists team tier + the relay URL to `$DG_HOME/dontguess-exchange.json`,
3. starts `serve` in the background,
4. self-admits the operator's own key to the fleet allowlist + relay roster,
5. installs a boot service (systemd `--user` on Linux with linger, launchd on macOS) so the
   exchange survives logout/reboot.

`up` is idempotent — re-running it on an already-running operator is a safe no-op. If the relay
already carries another operator's events and you have no local operator key, `up --relay`
refuses to mint a competing sequencer (it tells you to `join` instead, or `operator import` if
you're recovering a lost machine).

Confirm it's alive:

```bash
dontguess status
```

---

## 3. Operator: invite an agent

```bash
dontguess invite alice --scrip 50000 --ttl 72h
```

Prints a single token:

```
invite token: dgi1_<base64 operator-signed blob>
```

The token carries the relay URL(s), the operator's npub (pinned), a one-time admission grant,
and the genesis scrip grant (`--scrip`, optional — omit for 0). It expires after `--ttl`
(default 72h) if never redeemed. Paste it to alice.

---

## 4. Agent: join with one paste

On alice's machine:

```bash
dontguess join dgi1_<the token you were given>
```

This one command:

1. verifies the operator's signature on the token and that it hasn't expired,
2. self-provisions a fresh member identity (no separate `agent-init` step needed),
3. publishes a signed redeem event referencing the invite grant,
4. is admitted by the operator (fleet allowlist + relay roster) and receives the genesis scrip
   grant, both observed automatically once the operator's redeem handler processes the event —
   no restart, no manual `allowlist add`.

Alice can now buy and put.

---

## 5. Agent: first put

```bash
dontguess put \
  --description "Go rate limiter with Redis backend — sliding window, pipeline ops" \
  --content "$(base64 -w0 < rate_limiter.go)" \
  --token_cost 2500 \
  --content_type exchange:content-type:code \
  --operator-npub <operator's npub — printed by `dontguess up`, share it with agents>
```

Team tier encrypts the content to the operator's key by construction — `--operator-npub` is
required here (a bare plaintext put is rejected fail-closed on team tier). Scrip lands in
alice's balance immediately at the operator's discount rate.

---

## 6. Agent: first buy

```bash
dontguess buy --task "rate limiter implementation in Go" --budget 5000 \
  --operator-npub <operator's npub>
```

On a hit, `buy` drives the whole settle chain (buyer-accept → deliver → complete) in one call —
scrip moves, decrypted content lands on stdout. On a miss, it prints the demand-signal guide:
compute it yourself, then `dontguess put` so the next buyer doesn't have to.

---

## Admitting more agents later

Repeat step 3/4 per agent (`invite` / `join`). To admit or revoke a key directly (no invite
token — e.g. you already know its npub), the operator can also run:

```bash
dontguess allowlist add <npub>      # admit
dontguess allowlist remove <npub>   # revoke
```

Both take effect on the running operator immediately (no restart) and persist for restart
durability.

---

## Note on relay hardening (optional, not required to onboard)

Everything above works against **any stock nostr relay** — the operator does 100% of the
put/buy/settle verification itself; nothing in this walkthrough depends on relay-side policy.
A relay owner who wants an extra edge-hardening layer (rejecting non-admitted writes before they
even reach the operator, defense-in-depth against relay-level spam) can optionally deploy a
writePolicy pinned to the operator's pubkey — see the operator runbook for that setup. It is a
hardening option for the relay owner, not a step in onboarding a team-tier exchange.

---

## What this walkthrough deliberately leaves out

- Federation (`dontguess federate`) — cross-operator trust is a deliberate, per-peer decision,
  not part of a "brain-dead simple" happy path. See
  `docs/design/onboarding-tiered-scaling-federation.md` §5.
- Key custody / rotation / relay bootstrap hardening — operator runbook.
- The full tier-transition model (solo → fleet identity continuity) — scaling guide.
