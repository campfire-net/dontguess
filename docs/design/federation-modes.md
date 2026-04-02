# Federation Modes — Deployment Tiers

**Status:** Design
**Date:** 2026-04-01

---

## Premise

DontGuess is a convention on a campfire. The exchange engine is a subscriber that processes `exchange:*` messages wherever the campfire lives — filesystem, hosted, bridged, peered. Federation is not a dontguess feature. It is a property of the campfire the exchange runs on.

The same pattern applies to every campfire application (social, ready, dontguess). Each defines its conventions. The infrastructure layers — transport, hosting, peering, discovery, trust, naming — are shared. Each application gets the full deployment progression for free.

---

## The Five Modes

### Mode 1: Project

**One exchange per project. Context relevant only here.**

- Transport: filesystem (`~/.campfire/rooms/`)
- No networking. No federation.
- `dontguess init` creates the exchange campfire
- `dontguess serve` subscribes locally
- Agents join via `cf join <exchange-id>`
- Exchange is disposable when the project ends

**Infrastructure required:** None beyond campfire CLI.

**E2E proof:** `test/e2e-install.sh` — init, join, put, buy, match.

---

### Mode 2: Individual

**One exchange across all projects on a single machine.**

- Transport: filesystem
- The exchange lives at the sysop's **center campfire** (created by `cf init`, open join protocol, aliased as `~center`)
- All projects share one exchange — a Go rate limiter analysis in project A is discoverable when project B needs it
- Domain tags and semantic matching handle cross-project relevance
- `dontguess init --scope user` attaches to center instead of creating a project-scoped campfire

**Infrastructure required:** None beyond campfire CLI.

**E2E proof needed:** Cross-project put/buy round-trip through center campfire.

---

### Mode 3: Team

**One exchange across a sysop's machines, or a small team's machines.**

Same exchange, different transport. Three hosting options — the exchange engine doesn't care which. It subscribes to a campfire ID; the transport is the campfire's problem.

**Option A: campfire-hosting (recommended)**
- `cf init --remote https://mcp.getcampfire.dev`
- Center campfire lives on hosted infrastructure (Azure ACA, three sovereign regions)
- Exchange engine connects via P2P HTTP long-poll
- All machines reach the same campfire
- Turnkey — no self-hosting, no NAT problems, SSE delivery

**Option B: Self-hosted relay**
- Sysop runs a campfire relay on a VPS or home server
- `cf bridge` relays between machines
- Exchange engine on one machine, others connect via P2P HTTP

**Option C: Direct P2P**
- `cf join --via <endpoint>`
- Machines peer directly
- Exchange engine on one machine, others long-poll

**Infrastructure required:** campfire-hosting account, OR a server for self-hosting, OR direct connectivity between machines.

**E2E proof needed:** Put on machine A, buy on machine B, match delivered. Test each hosting option independently.

---

### Mode 4: Organization

**Multiple operators, each with their own exchange. Selective inventory sharing.**

Multiple sysops (or teams) each run their own exchange. Campfire peering handles the transport layer. DontGuess federation conventions handle the business agreement — what inventory to share, at what trust floor, with what settlement terms.

- Campfire peering (`core-peer-establish`) connects exchange campfires
- `federation:propose` / `federation:accept` establish the business terms
- `federation:inventory-offer` shares inventory selectively (filtered by domain, content type, trust floor)
- `federation:match-request` / `federation:match-confirm` handle cross-operator matches
- `federation:reconcile` settles accounts on the agreed rail (bilateral-credit or x402)
- Trust: behavioral scoring per counterparty, new-node dual guard, 30-day graduation

**Infrastructure required:** Always-on hosting for each exchange (campfire-hosting or self-hosted). Can't federate from a laptop that sleeps.

**E2E proof needed:** Two independent exchanges, federation proposal, cross-operator match, settlement.

---

### Mode 5: Global

**Open federation across unrelated operators. Discovery via the agentic internet.**

Same protocol as org-wide, but discovery and trust operate at internet scale via the agentic internet infrastructure:

- **Discovery:** Directory service convention (AIETF WG-1). Exchanges publish beacons, register in directories. Agents discover exchanges by domain, not by knowing campfire IDs.
- **Naming:** `cf://acme.exchange.dontguess` — exchanges are addressable via naming URIs. Cross-root resolution connects independent networks.
- **Routing:** Path-vector routing via `routing:beacon`. Multi-hop reachability without direct peering.
- **Trust:** Portable reputation (AIETF WG-4). Trust scores travel across campfires. Local-first trust model — agents are the root, not platforms.
- **Agent profiles:** Exchanges advertise capabilities, cache hit rates, specialization via agent profile convention.
- **Settlement:** x402 (USDC) required. No bilateral credit with strangers.
- **Specialization emerges:** A code exchange federates with an analysis exchange. Buyers benefit from combined inventory. `inventory_scope` filters in `federation:propose` control what's shared.

**Infrastructure required:** Always-on hosting. Participation in agentic internet directory. x402 settlement rail.

**E2E proof needed:** Exchange discoverable via directory query, cross-network match via routing, x402 settlement.

---

## Infrastructure Layer Ownership

DontGuess does not build any of this. It composes infrastructure built at three layers:

| Layer | Repo | What it provides |
|-------|------|------------------|
| **Transport** | `campfire` | Filesystem, P2P HTTP, GitHub, long-poll, bridges, recursive composition, threshold signatures, beacons |
| **Hosting** | `campfire-hosting` | Hosted campfires (Azure ACA), SSE delivery, scale-to-zero, metering, wrapped-at-rest keys |
| **Fabric** | `agentic-internet` | Directory service, naming URIs, path-vector routing, trust model, agent profiles, peering conventions |

DontGuess owns only the **application layer**: exchange conventions (put, buy, match, settle), federation conventions (propose, accept, inventory-offer, match-request, reconcile), matching engine, pricing engine, scrip ledger.

---

## E2E Test Strategy

Each mode needs its own e2e test proving the seam works. The test does not re-prove that campfire's transport works — that's campfire's test suite. It proves that dontguess correctly operates over that transport.

| Mode | Test | Status |
|------|------|--------|
| Project | init → join → put → buy → match (filesystem) | **Passing** (`TestMode1_ProjectLocal`) |
| Individual | cross-project put/buy on single exchange | **Passing** (`TestMode2_UserLocal`) |
| Team | two identities, shared transport, admit → join → put → buy → match | **Passing** (`TestMode3_Team`) |
| Team (hosted) | serve via hosted campfire → put/buy across machines | Not started |
| Team (self-hosted) | serve via bridge → put/buy across machines | Not started |
| Organization | Two exchanges → federation propose/accept → cross-operator match | **Blocked** — federation conventions not embedded, engine has no federation handlers |
| Global | Directory discovery → cross-network match → x402 settle | **Blocked** — requires Organization first |

**Rule: if it doesn't have a passing e2e test, it doesn't go on the website.**

---

## Shared Pattern

This is the same story for every campfire application:

| App | Convention | What it trades |
|-----|-----------|----------------|
| **DontGuess** | `exchange:*` | Cached inference results |
| **Social** | `social:*` | Posts, replies, reputation |
| **Ready** | `ready:*` | Work items, dependencies, status |

Each gets the same five deployment modes. Each composes the same infrastructure layers. The progression from project-local to global is a property of the platform, not the application.
