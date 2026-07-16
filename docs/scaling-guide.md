# Scaling Guide — solo → fleet → federation

Audience: both humans and autonomous agents. Every command below is copy-paste
runnable. Design source: `docs/design/onboarding-tiered-scaling-federation.md`
§6 (tier-transition invariants) + §7.4 (docs plan). Confidentiality authority:
`docs/design/content-confidentiality-envelope-541.md`.

## The three rungs

| Rung | What it is | One-command reach | Who sees your plaintext |
|---|---|---|---|
| **SOLO** | one machine, individual tier, local store, no relay | `dontguess up` | nobody — local-only |
| **FLEET** | many machines, ONE shared operator over a relay | `dontguess up --relay <urls>` (operator) + `dontguess invite`/`dontguess join` (members) | your operator (§541) |
| **FEDERATION** | multiple independent operators trading liquidity | `dontguess federate <peer>` — **OPEN, not shipped** (see below) | your operator, plus a peer operator only if you opt a specific entry into custodial resale |

There is no rung where "nobody but you" holds after you climb past solo, and no
rung where federation is a passive, no-consequence step. Read the
confidentiality posture below before you run `--relay` or opt into custodial
resale.

## Rung 1 — SOLO

```
$ dontguess up
```

- Local store at `$DG_HOME`, plaintext content, no relay, no scrip.
- This is also where your **permanent operator identity gets minted** —
  `up` mints a secp256k1 key on the first run and uses it as
  `State.OperatorKey` from day one, even though solo never touches a relay.
  That identity is what carries you up the ladder later with **zero re-sign**
  (§6 of the design doc; ADV-17).
- Confidentiality: **local-only.** Nothing leaves the machine. There is no
  operator distinct from you at this rung.

## Rung 2 — FLEET

One operator, many member machines, over a nostr relay. Team tier
envelope-encrypts content (§541) — the operator holds the key that decrypts it
to service matches.

### Becoming the operator

```
$ dontguess up --relay ws://192.168.2.40:7777,ws://192.168.2.41:7777
```

- Reuses the **same operator key** minted at solo `up` — no fork, no re-sign.
- Persists tier=team + the relay set, starts `serve`, self-admits the
  operator's own key into the fleet allowlist via the same signed-IPC path
  `dontguess allowlist add` uses, and installs a boot service (systemd
  `--user` + linger on Linux, launchd on macOS) so the operator survives
  logout/reboot.
- **ADV-4 refuse-mint guard:** if this machine has no local operator private
  key yet, `up --relay` probes the relay set first. If it finds an existing
  operator's events already there, it **refuses to mint** a second, competing
  operator identity and tells you to run `dontguess join <token>` instead (or
  `dontguess operator import` if you're recovering a lost machine that legitimately
  owns the operator key). It only proceeds to mint on an unverifiable relay if
  you pass `--new-operator` to explicitly confirm you're bootstrapping a
  brand-new fleet — a positively-detected existing operator always wins over
  that flag.
- **FLEET is ONE operator.** Never run `up --relay` on a second machine to
  "add capacity" — that is what `dontguess operator export` / `operator import`
  (1Password-backed key custody) exist for, for the rare genuine
  multi-host-single-operator case. Every other second machine is a **member**.

### Bringing on a member — one paste each side

```
# operator:
$ dontguess invite alice --scrip 50000 --ttl 72h
invite token: dgi1_<base64 operator-signed blob>

# alice's machine:
$ dontguess join dgi1_<blob>
✓ verified operator signature, not expired
✓ provisioned member identity
✓ admitted to fleet allowlist
✓ genesis grant: 50000 scrip
```

`invite` mints an operator-signed, single-use, TTL'd token (relay URLs +
operator npub pin + a one-time grant id + optional genesis scrip). `join`
verifies the signature, self-provisions a fresh member key, and publishes a
kind-3410 redeem event the operator's own reader verifies and promotes — no
manual `agent-init` / `allowlist add` / `mint` juggling. The relay accepting
the redeem write is a transport receipt only; **the operator does 100% of the
actual verification and admission**, exactly as it does for every other
exchange operation.

### Hot admin, no restart

```
$ dontguess allowlist add <npub>
$ dontguess allowlist remove <npub>
```

Takes effect sub-second on a running operator (signed live IPC), and persists
to config for restart durability. No operator restart is needed to admit or
revoke a member.

### On the relay itself

Team tier works against **any stock nostr relay** — the operator does 100% of
the trust/admission verification on the exchange side (`selfAdmitOperator`,
`allowlist add/remove`, the redeem verification in `join`). You do not need a
custom relay policy to run a fleet.

A relay operator (the person who owns the strfry instance, which may or may
not be the same person as the dontguess operator) can **optionally** deploy a
roster-aware `writePolicy` that pins the dontguess operator's pubkey as the
sole roster authority and rejects writes from un-admitted keys at the relay
edge — this is edge-hardening / defense-in-depth, not a requirement. Document
it to relay owners as an optional hardening step, never as a required part of
onboarding a fleet.

### Confidentiality at this rung

**Single-operator-plaintext (§541).** Team-tier content is envelope-encrypted
on the wire and at rest on the relay, but your operator holds the CEK
(content-encryption key) needed to service matches — the operator can read
everything put through it in plaintext. This is not a bug to route around; it
is the model. Be honest about it:

- **No forward secrecy.** If the operator's private key ever leaks, the
  attacker can offline-decrypt the operator's *entire historical corpus* from
  data already public on the relay/Blossom. Rotating the key afterward
  protects only content put *after* rotation — it gives zero retroactive
  protection.
- **No revocation.** Once a ciphertext blob is published to the relay/Blossom,
  it is append-only. There is no "unpublish."

Full threat model and the operator-facing rotation runbook live in
`docs/design/onboarding-tiered-scaling-federation.md` §7.3.

## Rung 3 — FEDERATION (OPEN — not shipped)

Federation is multiple independent operators trading liquidity with each
other. `dontguess federate <peer-beacon>` is **deliberately not a one-command,
brain-dead-simple verb** — it is the single most consequential trust decision
in the ladder, because federating can expose your entire plaintext corpus to
another operator.

**Status: paper-first design only.** The wire protocol
(`federation:propose/accept/inventory-offer/match-request/match-confirm/revoke/reconcile`)
has zero shipped handlers as of this writing. Do not depend on `federate`
working end-to-end; treat this section as the confidentiality posture you
will inherit once it ships, not as usable commands today.

### The two federation modes

- **ROUTER mode (the intended default).** A peer operator sees only metadata
  (description, teaser, token cost, domains, embedding) — **never the CEK,
  never the plaintext.** On a cross-operator match, the peer forwards the
  buyer's funded reservation back to your operator, and *your* operator
  re-wraps the key directly to the buyer. The peer is a discovery/matching
  router only.
- **CUSTODIAL mode (opt-in only, never automatic).** Your operator re-wraps
  the CEK to the peer operator so the peer can deliver on your behalf (e.g.
  when your operator is offline). This is an **explicit, per-entry seller
  opt-in** — not a side effect of federating, not a default, not something
  discovery implies. Once you opt an entry into custodial resale, your
  confidentiality boundary for that entry widens to whichever of the two
  operators is least trustworthy.

### Confidentiality at this rung

- **Router mode:** still single-operator-plaintext — federating for discovery
  does not, by itself, give any peer read access to your content.
- **Custodial mode:** **is not safe by default and must never be described as
  such.** Opting an entry into custodial resale is equivalent to granting the
  receiving peer the same plaintext access your home operator has, for that
  entry. Only federate custodially with an operator you would trust to read
  everything you'd hand it. A sybil operator that gets custodial resale of
  your whole inventory can pull every entry's plaintext — strictly worse than
  a passive relay scraper, who only ever sees ciphertext.

Do not describe federation — router or custodial — as adding no new trust
exposure. Router mode holds the line at "your one operator sees plaintext."
Custodial mode moves that line to "your one operator, plus whichever peers
you've explicitly opted specific entries into," and that is a real,
consequential widening the seller must consciously choose per entry, not
inherit from joining a federation.

## Tier-transition invariants (§6)

These hold no matter which rung you're climbing between:

1. **ONE operator identity, permanent from solo onward.** The secp256k1 key
   minted at your very first `dontguess up` is the same key used as the team
   and federation operator identity later. Climbing rungs never re-signs,
   never forks, never re-attributes historical records to a different key.
2. **What migrates vs what stays.** Your operator identity and its scrip
   ledger migrate with you up the ladder. Content does **not** silently
   migrate: pre-climb solo-tier plaintext puts stay local-only and are never
   automatically republished to a relay in cleartext the instant you run
   `up --relay`. Re-putting an entry as encrypted team-tier content is a
   deliberate, per-entry action you take — never an automatic side effect of
   climbing.
3. **Mixed-log v-tag dispatch.** Because of (2), a single operator's event log
   can legitimately contain both pre-climb plaintext solo puts and post-climb
   v2 encrypted team puts side by side. The dispatch path distinguishes them
   by envelope version tag (v-tag) at fold time — a plaintext solo-era record
   and an encrypted team-era record are handled through different code paths
   in the same log, not migrated into a single format. This is expected,
   permanent state for any operator that started solo and climbed — not a
   transitional inconsistency to "clean up."

## Informed-consent summary (read once, applies at every rung above solo)

- Your home operator can read your plaintext content once you're above solo.
- Federating for custodial resale extends that same read access to the peer,
  per-entry, only if you explicitly opt in.
- There is no forward secrecy — one operator-key leak decrypts that
  operator's entire historical corpus, offline, from data already public on
  the relay/Blossom.
- There is no content revocation once something is published.

Full language: `docs/design/onboarding-tiered-scaling-federation.md` §7.3 and
the `§8.9 Informed consent` block in the project `CLAUDE.md`.
