# QUICKSTART — 0 → exchange

Three rungs, three narrated transcripts. Each block is copy-paste runnable —
run it, read what it prints, and you're on the exchange. No pseudo-commands:
every line below is a real `dontguess` invocation against the shipped v0.7.0+
CLI (`up`/`invite`/`join`/`allowlist`/`operator export`/`operator import`).

Design reference: `docs/design/onboarding-tiered-scaling-federation.md` §1/§5/§7.

## Rung 1 — SOLO (one machine, no relay)

One command. No relay, no scrip, no fleet — just a local cache.

```
$ dontguess up
✓ operator identity (secp256k1) ready: npub1...
✓ engine running (individual tier)
```

**Confidentiality posture:** individual tier is **local-only, plaintext-on-disk**
under `$DG_HOME` (default `~/.dontguess`). Nothing leaves the machine — there is
no relay to leak to. Re-running `up` is idempotent: if `serve` is already up, it
says so and changes nothing.

You can now `dontguess buy` / `dontguess put` immediately — see the root
`CLAUDE.md` DontGuess block for the day-to-day buy/put loop.

## Rung 2 — FLEET (many machines, one shared operator over relays)

Two commands: the operator promotes itself with `up --relay`; every additional
machine joins with one pasted token. **The SAME operator identity solo minted
carries forward — no re-sign, no fork** (design §1/§6, ADV-17).

**On the operator's machine:**

```
$ dontguess up --relay ws://192.168.2.40:7777
✓ operator identity (secp256k1) ready: npub1...
✓ engine running (team tier)
✓ self-admitted operator key to the fleet allowlist + relay roster
✓ boot service installed (systemd --user), linger=true
```

`--relay` accepts a comma-separated list for multiple relays. `up --team` (or
`--fleet`) declares the tier explicitly and fails loud — never a silent solo
downgrade — if no relay is configured anywhere (flag, `DONTGUESS_RELAY_URLS`
env, or a prior persisted config). A second machine run with no local operator
key refuses to mint a competing operator if it detects existing operator
events already on the relay (ADV-4) — recover a lost operator machine with
`dontguess operator export` / `dontguess operator import` instead of
re-bootstrapping.

**Mint a member invite (on the operator):**

```
$ dontguess invite alice --scrip 50000 --ttl 72h

invite token for "alice":

dgi1_<base64 operator-signed blob>

  operator npub: npub1...
  relays:        [ws://192.168.2.40:7777]
  genesis scrip: 50000
  expires:       2026-07-19T12:00:00Z

On the member's machine, one paste:
  dontguess join dgi1_<base64 operator-signed blob>
```

**On the new member's machine — one paste:**

```
$ dontguess join dgi1_<base64 operator-signed blob>
✓ verified operator signature, not expired
✓ provisioned member identity
✓ admitted to fleet allowlist + relay roster
✓ genesis grant: 50000 scrip
→ you can buy/put/settle now
```

Admit/revoke individual sellers directly with `dontguess allowlist add
<npub>` / `dontguess allowlist remove <npub>` — takes effect live, no
operator restart.

**Confidentiality posture:** team tier is **encrypted by construction** — puts
are sealed to the operator's key (NIP-44) before they ever touch a relay;
plaintext puts are rejected fail-closed. **The operator itself sees plaintext**
(it must, to price/re-sell/dispute) — this is the fleet's single-operator
trust boundary, not a leak (see `docs/design/content-confidentiality-envelope-541.md`).
Content of **any size** is delivered end to end at this rung — there is no
32 KiB inline-only ceiling; large content routes through the same
encrypted-put/fetch path as small content.

The optional strfry `writePolicy` roster hardening some relay owners deploy is
an **edge-hardening add-on**, not a requirement — team tier works against
**any** stock nostr relay, because the operator itself performs 100% of the
allowlist/trust verification server-side. Do not treat a custom relay policy
as a prerequisite for `up --relay`.

## Rung 3 — FEDERATION (many independent operators, cross-operator liquidity)

**Not yet shipped.** `dontguess federate <peer-beacon>` is intentionally the
one command on this ladder that is **not** brain-dead-simple by design — it is
the single most consequential trust decision an operator can make (a
mis-scoped federation can expose your entire plaintext corpus to a peer, see
ADV-9/ADV-10 in the design doc). It ships **paper-first**: the trust model,
router-vs-custodial resale modes, and sybil-defense economics must close on
paper before any `federate` code lands (design §5, Gate C). As of this
writing zero federation handlers exist in `pkg/exchange` — do not attempt to
script around this; there is no runnable command for this rung yet.

**Confidentiality posture (planned, once it ships):** router mode — the
origin operator delivers content directly to the buyer; a federated peer
matches inventory but never receives the CEK, so it never sees plaintext.
Custodial mode — a peer that re-sells your inventory on your behalf **does**
see your plaintext corpus; this is only ever an explicit, per-entry,
seller-opt-in grant, never implied by discovery or federation itself. Only
federate in custodial mode with an operator you would let read everything.

Track progress: `docs/design/onboarding-tiered-scaling-federation.md` §5 /
§8 (ADV-9/10/11/13) / §9 Gate C.

---

**Permanent constraints, all rungs (state these to every operator/agent):**
operator-key leak decrypts the entire historical corpus offline from
already-scraped relay+Blossom data — there is no forward secrecy and no
after-the-fact content revocation once something is public. See
`docs/design/onboarding-tiered-scaling-federation.md` §7.3 for the full
operator-key-leak threat model and rotation runbook.
