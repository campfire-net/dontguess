# ClankerOS Handoff: DontGuess Live-Fire Testing

## What you're proving

DontGuess is a token-work exchange — agents buy and sell cached inference results via campfire messages. The exchange engine (`dontguess serve`) is running and has been smoke-tested with manual seller/buyer Go programs. Now we need to prove it works under real autonomous agent operation, and prove ClankerOS can orchestrate that.

Your success condition: **ClankerOS automata execute all 27 test scenarios in `~/projects/dontguess/test/live-fire/scenarios.md` against the live exchange engine, and all pass.** This is the proof that both systems work — DontGuess handles real agent traffic correctly, and ClankerOS can drive that traffic.

## What exists

### In the dontguess repo (`~/projects/dontguess/`)

**Exchange engine:**
- `cmd/dontguess/serve.go` — starts the engine, polls campfire, auto-accepts puts, syncs transport→store
- `cmd/dontguess/init.go` — bootstraps exchange campfire with convention declarations
- Start with: `go run ./cmd/dontguess init --convention-dir ./docs/convention --force && go run ./cmd/dontguess serve`
- Engine prints `EXCHANGE_CAMPFIRE` and `OPERATOR_KEY` on startup

**Agent specs** (teach automata how to interact with the exchange):
- `.claude/agents/exchange-seller.md` — how to send `exchange:put` messages, payload format, tag convention, full Go code pattern for signing and writing to transport+store
- `.claude/agents/exchange-buyer.md` — how to send `exchange:buy`, handle match results, preview flow, accept/reject/complete/dispute, full settlement paths
- `.claude/agents/exchange-attacker.md` — 5 categories of attacks (authorization, payload, replay, state manipulation, economic), each with expected rejection behavior

**Chart configs** (ClankerOS automaton definitions):
- `charts/seller.toml` — ReadyWorkSource, identity, budget, agent spec reference
- `charts/buyer.toml` — same pattern for buyer role
- `charts/attacker.toml` — same pattern for adversarial role

The `[exchange]` section in each chart has empty `campfire_id` and `operator_key` — fill these from the engine's startup output before booting automata.

**Test scenarios** (`test/live-fire/scenarios.md`):
- 27 scenarios across 8 phases
- Each specifies: assignee (seller/buyer/attacker), dependencies, actions, pass condition
- Phases: core happy paths, scrip flows, authorization attacks, payload attacks, state manipulation, dynamic pricing, reputation, engine resilience

**Proven Go programs** (working seller/buyer agents):
- `/tmp/seller-agent/main.go` — sends `exchange:put` messages, verified working
- `/tmp/buyer-agent/main.go` — sends `exchange:buy`, reads match results, verified working
- These are the seed code the agent specs reference

### How the exchange works

Messages are signed Ed25519 campfire messages written to filesystem transport (`/tmp/campfire/<campfire-id>/`). The engine syncs transport→SQLite store every 500ms and processes messages tagged `exchange:put`, `exchange:buy`, `exchange:settle`. The engine writes response messages (match, put-accept, deliver) back to the same campfire.

The Go code pattern for sending a message:
1. Load Ed25519 identity from file
2. Marshal JSON payload
3. `message.NewMessage(privKey, pubKey, payload, tags, antecedents)`
4. Add provenance hop from campfire state
5. `transport.WriteMessage(campfireID, msg)` — filesystem
6. `store.AddMessage(rec)` — SQLite

All imports from `github.com/campfire-net/campfire/pkg/...` — the dontguess repo has a `replace` directive pointing to `~/projects/campfire`.

## What you need to do

### Step 1: Get the exchange engine running

```bash
cd ~/projects/dontguess
go run ./cmd/dontguess init --convention-dir ./docs/convention --force
go run ./cmd/dontguess serve
```

Note the EXCHANGE_CAMPFIRE and OPERATOR_KEY from the output. The engine must stay running throughout testing.

### Step 2: Boot automata

For each role (seller, buyer, attacker):
1. Generate an Ed25519 identity at the `key_file` path in the chart
2. Set the `[exchange]` campfire_id and operator_key in the chart (or pass via env)
3. `bang start --chart ~/projects/dontguess/charts/<role>.toml`

The automata poll their ReadyWorkSource campfire for work items. When a scenario is posted, the assigned automaton claims it, reads the agent spec, and executes the test against the live exchange.

### Step 3: Post test scenarios as work items

Each scenario in `test/live-fire/scenarios.md` becomes a Ready work item on the shared campfire. The work item title is the scenario ID (S01, S02, ...), the context is the full scenario description including actions and pass conditions.

Post them respecting dependency order — S02 depends on S01, S03 depends on S02, etc. Scenarios without dependencies can run in parallel.

### Step 4: Trial → Investigate → Fix loop

As automata execute scenarios:

- **If a scenario passes:** Move to the next one. Log the pass.
- **If a scenario fails because ClankerOS couldn't execute it** (boot failure, work routing error, agent couldn't construct the Go program, etc.): This is a ClankerOS bug. Investigate, fix ClankerOS, retry.
- **If a scenario fails because the exchange rejected a valid operation** (engine bug — correct message sent, wrong response): File a bug in the dontguess repo (`rd create "Bug: <description>" --type bug`). The dontguess maintainer will fix it. Skip the scenario and continue.
- **If a scenario passes but the exchange ACCEPTED an attack** (attacker scenario where the exchange should have rejected): This is a critical exchange security bug. File immediately.

The goal is: all 27 scenarios pass. Some failures will be ClankerOS issues (your problem to fix), some will be exchange issues (file and skip). The ratio tells us where both systems need work.

### What "pass" looks like for ClankerOS

ClankerOS succeeds when:
1. All 3 automata boot from their charts and poll for work
2. Work items are claimed by the correct role (seller scenarios by seller, etc.)
3. Each automaton reads its agent spec and constructs correct exchange messages
4. Messages hit the live exchange and produce the expected campfire responses
5. The automaton reads the response, evaluates the pass condition, and reports result
6. All 27 scenarios attempted, failures attributed to the correct system

This is not about all 27 passing on the first try. It's about the loop working: try, observe what broke, fix it (in ClankerOS or file it against dontguess), try again.

## Key gotchas

1. **The exchange campfire ID changes on every `dontguess init --force`.** Charts must be updated.
2. **Messages require provenance hops.** The Go code must read campfire state and add a hop — raw `cf send` won't work because the exchange engine verifies the provenance chain.
3. **The engine syncs transport→store every 500ms.** Messages written to transport appear in the engine within 1 second. Allow 2s for response.
4. **The `budget` field in buy payloads is authoritative.** The engine also accepts `max_price` as an alias, but the convention spec says `budget`.
5. **The go.mod uses a local replace:** `replace github.com/campfire-net/campfire => ~/projects/campfire`. Any Go programs the automata write need the same replace directive.
6. **Operator-only operations** (put-accept, match, deliver, all scrip messages) require the operator's Ed25519 key. Non-operator senders are silently ignored — no error response, just nothing happens.
