# SD-Core registration throughput — findings

Tracks **performance** defects ORBIT has found in the Aether SD-Core
(omec-project) two-node testbed (ATB-01), separate from the conformance/interop
issues in [`sdcore.md`](sdcore.md). Everything in the *Proven* section was
measured against the live core and traced to a specific line of upstream code;
everything in *Suspects* is a lead with supporting evidence but no experiment
yet.

**Problem.** ORBIT's first real integration benchmark put SD-Core's registration
ceiling at **~10 attach/s** (`docs/DESIGN.md` §Phase-2). That number is the
integration-capability bound on every scale claim ORBIT makes, and it is far
below operator-grade. This document is the record of where it actually went.

> **Do not quote the improved numbers as SD-Core's capability.** Everything
> above ~22 attach/s was measured against **locally patched binaries** running
> from a hostPath mount on ATB-01. None of those patches are upstream.
> ORBIT's published integration-capability figure stays at the stock number
> until they land — see [Recheck list](#recheck-list-when-the-fixes-land).
>
> | Configuration | Ceiling | Status |
> |---|---|---|
> | Stock SD-Core, as shipped | ~10 attach/s | **This is the quotable number.** Matches `docs/DESIGN.md`. |
> | \+ MongoDB CPU cap lifted | ~22 attach/s | Deployment config only, no code change. Quotable with that caveat. |
> | \+ patched `openapi` and `udr` | ~190 attach/s | **Not quotable.** Requires unlanded patches. |

Deployed versions (from `atb01-sdcore-config`): AMF rel-3.1.0, SMF rel-4.1.0,
AUSF/UDM/UDR/NRF/NSSF/PCF rel-3.0.0, `omec-project/openapi/v2 v2.0.0`,
`omec-project/util v1.7.3`.

---

## Headline numbers

Registration only (no PDU session), `orbit load`, 2000 UEs across 4 gNBs unless
noted. "Ceiling" = throughput flat while latency grows linearly with
concurrency (Little's Law holds at every measured point).

| Stage | Ceiling | Serial reg latency | Needs unlanded patch? |
|---|---|---|---|
| Baseline as found | ~10/s | 400 ms | no — stock |
| \+ MongoDB CPU cap lifted (F1) | ~22/s | 288 ms | no — deployment config |
| \+ nrfcache UDR filter (F3) | ~185/s | 256 ms | **yes** — `openapi` `2211d72` (+ `53e1d00`) |
| \+ UDR race fix (F5) — made 2000-UE runs survivable | 162–208/s (mean ~188) | 376 ms @ conc 64 | **yes** — `openapi` + uncommitted `udr` fix |

Final sweep, 2000/2000 UEs successful on every run: conc 64 → 166/s;
conc 128 → 163 / 201 / 208; conc 256 → 162 / 204 / 187.

## Method

Attribution came from four independent instruments, which is why the negative
results below are trustworthy:

- **`orbit load`** concurrency sweeps — a flat throughput curve with linearly
  growing latency identifies saturation; the knee gives the serialized service
  time per attach.
- **Per-pod cgroup CPU deltas** (`usage_usec` / `throttled_usec` before and
  after a run) — attributes CPU and, critically, *quota throttling* per NF.
- **MongoDB `db.adminCommand({top:1})` deltas** — exact op counts per attach,
  per collection.
- **AMF `pprof`** on `debugProfilePort: 5001` — goroutine dumps under load show
  where in-flight attaches are parked. No other NF exposes pprof.

---

## Proven

### F1 — MongoDB ships with a 750m CPU cap and is throttled flat

**Evidence.** `mongodb-0`/`-1` carry `limits.cpu: 750m` (Bitnami chart default);
every other NF in the namespace has no CPU limit at all. During a 100-attach
run mongodb-0 consumed 8970 ms CPU over 12012 ms wall — 0.747 cores, exactly its
quota — and was **throttled for 16.7 s within that 12 s run**. At complete idle
it still burned 0.26 of its 0.75 cores and was throttled 2.19 s per 10 s, because
background NRF `NfProfile` polling and SMF `fseid` churn never stop.

**Fix.** Raised both to 8 CPU / 6Gi (StatefulSet `mongodb`, persisted).

**Effect: ~10 → ~22 attach/s.** Nothing has been CPU-throttled since.

**Note.** This is a deployment defect, not upstream code. It is also the only
one of these findings that a bigger machine would have hidden.

### F2 — nrfcache rejects profiles that declare no SUPI ranges

`openapi/v2 nrfcache/match_filters.go` — `MatchUdmProfile`, `MatchPcfProfile`,
`MatchAusfProfile`.

**Evidence.** With `enableNrfCaching: true`, the AMF logged `cache miss for
nftype AUSF|UDM|PCF` on *every* attach, always preceded by
`match found = false (no SUPI ranges)`. When a discovery query carries a SUPI,
the matcher required the cached profile to have a non-empty `SupiRanges` and
returned `false` otherwise. SD-Core registers one UDM/PCF serving all
subscribers with no ranges — confirmed on the wire, the NRF returns
`udmInfo:{"groupId":""}`.

**Why it is unambiguously a bug.** The NRF's own discovery filter matches those
profiles. `nrf/producer/nf_discovery.go`, `[Query-18] supi`, builds:

```
$or: [ { pcfinfo.supiranges: { $elemMatch: {start<=supi, end>=supi} } },
       { pcfinfo.supiranges: nil },
       { pcfinfo.supiranges: { $exists: false } } ]
```

An absent range means *unrestricted*. The cache therefore selected a different
set of profiles than the NRF it caches — a cached lookup and a live discovery
disagreed for the same query. This does not depend on reading 3GPP: the client
and server of the same interface contradict each other.

**Fix.** Commit `53e1d00` — treat "no SUPI ranges" as unrestricted, with
regression tests that fail without the change.

**Effect.** AMF cache misses 4/attach → 0; NRF CPU 76.8 → 50.7 ms/attach (-34%);
`NfProfile` queries 21.1 → 16.0 per attach. **Throughput unchanged (~22/s)** —
real waste removed, but not the ceiling.

### F3 — nrfcache has no match filter for UDR, so cached UDR discovery never returns anything

`openapi/v2 nrfcache/match_filters.go:31` (`matchFilters`) and
`nrfcache/nrfcache.go:235`.

**Evidence.** `matchFilters` registers SMF, AUSF, PCF, NSSF, UDM, AMF — not UDR.
The lookup path is:

```go
if cb, ok := matchFilters[element.nfProfile.NfType]; ok { ... }   // no else — profile dropped
```

A profile whose NfType has no registered filter is silently discarded. Cached
UDR discovery therefore *always* returned empty, which (a) forced a live NRF
query on every UDM/PCF subscriber data access and (b) triggered the
`EnableNrfCaching && empty result` fallback, firing a *second* direct NRF query.

This compounds with F4 (the miss path was serialized) and with the fact that
UDR is resolved on **every** subscriber data access — there are 3+ per
authentication alone (`QueryAuthSubsData`, `ModifyAuthenticationSubscription`
for the SQN write-back, `CreateAuthenticationStatus`), plus SDM AM-data, SMF
selection, and UECM.

**Fix.** Commit `2211d72` — add `MatchUdrProfile` mirroring the NRF's own UDR
supi filter and register it. Includes a test asserting the filter *is*
registered, so a future NF type cannot be silently dropped the same way.

**Effect: ~22 → ~185 attach/s (8x).** Mongo ops per attach 50.2 → 25.3;
`NfProfile` and `urilist` queries left the hot path entirely. This is the single
largest win found.

### F4 — nrfcache held the cache write lock across the NRF network round trip

`openapi/v2 nrfcache/nrfcache.go`, `handleLookup`.

**Evidence.** On a miss the code took `c.mutex.Lock()` and held it across
`nrfDiscoveryQueryCb` — a network call to the NRF. The upstream comment states
the intent: *"nrf discovery query is mutex protected."* The precise damage is
subtler than "misses serialize": Go's `sync.RWMutex` queues new readers behind a
*waiting* writer, so an in-flight discovery stalled every concurrent cache
**hit** for that NF type for a full round trip. With a permanently-missing NF
type (F3) that stall was continuous.

**Fix.** Commit `2df2b1b` — serialize discovery on a dedicated `discoveryMutex`;
take the cache lock only to read the entry and to store the result. The
thundering-herd guarantee is preserved (the goroutine that wins `discoveryMutex`
populates the cache; the rest find it on re-check), which the pre-existing
`TestCacheConcurrency` enforces — it asserts exactly one NRF callback for
concurrent identical lookups, and caught an earlier attempt that simply dropped
the lock.

**Effect: within measurement noise** (162–208/s spread vs 167–178/s before).
Once F3 made misses rare, this stopped binding. It is a correctness fix for cold
start and TTL expiry, not a throughput win.

### F5 — the UDR crashes under concurrent registration (`concurrent map writes`)

`udr/producer/data_repository.go`, `CreateSdmSubscriptionsProcedure`.

**Evidence.** Raising concurrency after F3 aborted the UDR process, exit code
134, with `fatal error: concurrent map writes` and this frame:

```
github.com/omec-project/udr/producer.CreateSdmSubscriptionsProcedure
github.com/omec-project/udr/producer.HandleCreateSdmSubscriptions
```

`UESubsData.SdmSubscriptions` is a plain `map` written with no lock. The same
function also does an unsynchronized `SdmSubscriptionIDGenerator++` (races *and*
can hand out duplicate subscription IDs), a check-then-act map initialization,
and a `Load`/`Store` pair where `LoadOrStore` is required. Four more
unsynchronized accesses to the same map exist in
`RemovesdmSubscriptionsProcedure`, `UpdatesdmsubscriptionsProcedure`, and
`QuerysdmsubscriptionsProcedure`.

The UDM creates an SDM subscription per registration, so concurrent attaches hit
it directly.

**This was masked for the entire investigation.** At 22/s the core never reached
the concurrency where the race fires; fixing F3 is what exposed it. Any attempt
to scale SD-Core would have hit this.

**Fix (not yet upstreamed).** `Mtx sync.RWMutex` on `UESubsData` guarding all
five access sites, `atomic.Int32` for the generator, `LoadOrStore` for the
collection. Regression test spawns 8 UEs × 64 concurrent creates and asserts no
lost rows and no duplicate IDs; passes under `-race`. Survives 2000-UE runs.

### F6 — one HTTP/2 connection per NF pair pins all SBI traffic to a single pod

**Evidence.** `netstat` inside the AMF pod shows exactly **one** established TCP
connection to each SBI peer (ausf, udm, nrf, pcf ClusterIPs). SBI is
`scheme: https`, so ALPN negotiates h2, and Go's shared `http.DefaultTransport`
multiplexes every request over that one connection — which conntrack pins to a
single backend pod.

Confirmed behaviourally: scaling `nrf` 1→4 and `ausf` 1→4 changed throughput
**not at all**. Under load 62 AMF goroutines sat in `Nausf_UEAuthentication`
round-trips while the AUSF pod used 0.11 cores — blocked on the pipe, not on
work. HTTP/2 streams were not exhausted (62 vs Go's 250 default), so this is
connection-level, not stream-level.

**Not fixed.** This is the structural blocker for horizontal scale-out: today
you cannot use more than one pod per NF regardless of replica count.

---

## Ruled out — measured, do not re-chase

| Hypothesis | Result |
|---|---|
| AMF `enableDBStore` writes (6 Mongo upserts/attach) | Disabling it: **no gain** (21.2 vs 22.2/s). Reverted. |
| NRF capacity | Scaling 1→4 replicas: no change. (Cause: F6.) |
| AUSF capacity | Scaling 1→4 replicas: no change. (Cause: F6.) |
| Per-SCTP-association limits in the AMF | `--gnb-count` 1/2/4 gave identical latency. |
| ORBIT being the bottleneck | Two independent load processes: 10.9/s each = 21.8/s aggregate, identical to one process. Ceiling is core-side. |
| MongoDB after F1 | `globalLock.currentQueue` 0/0, 16 active conns, no wiredTiger ticket exhaustion. |
| `ngap/service/service.go:262` `// TODO: concurrent on per-UE message` | Looks damning; is not. The SCTP read loop only decodes and hands off to a buffered(10) per-UE `EventChannel`. |
| `context/amf_ue.go:544` `time.Sleep(2 * time.Second)` | Detached cleanup goroutine; inflates goroutine count, does not block attaches. |

---

## Suspects — leads with evidence, no experiment yet

Ordered by expected impact.

1. **HTTP/2 connection pinning (F6) is the current binding constraint.**
   After F1/F3/F5 nothing is CPU-saturated (node 32 cores, well under half
   busy, zero throttling anywhere), yet throughput still caps. F6 is the best
   explanation and the obvious next experiment. Candidate fixes: headless
   Services with client-side load balancing, per-pod addressing, or forcing
   multiple connections per peer. **Test:** point the AMF at individual UDM/AUSF
   pod IPs and see whether aggregate throughput scales with pod count.

2. **UDR is a thin HTTPS proxy over MongoDB.**
   `udr/producer/data_repository.go:384 QueryAuthSubsDataProcedure` is a single
   indexed `RestfulAPIGetOne` wrapped in a TLS hop plus JSON marshal/unmarshal.
   Every subscriber data access pays a network round trip to reach a ~1 ms
   query, and there are 6+ per registration. Suspected large latency
   contributor, unquantified.

3. **AUSF has no discovery cache at all.**
   `ausf/producer/functions.go:279 GetUdmUrl()` calls
   `consumer.SendSearchNFInstances` on every authentication, and fires a *second*
   `SendNfDiscoveryToNrf` if the first returns empty.
   `ausf/consumer/nf_discovery.go` goes straight to the wire — there is no
   nrfcache layer, unlike the AMF and UDM.

4. **`getUdrURI` discards its own cache.**
   `udm/producer/ue_context_management.go:43` calls `SendNFInstancesUDR` in
   **both** branches — the `UdmUeFindBySupi` hit is thrown away and `ue.UdrUri`
   is written but never read back. After F3 these now hit the nrfcache, so the
   cost is a cache lookup rather than a network call, but the dead code path
   should go.

5. **Other NF types missing from `matchFilters` — same class as F3.**
   Only SMF/AUSF/PCF/NSSF/UDM/UDR/AMF are registered. Any NF type discovered
   with a filter and no matcher (NRF, CHF, SMSF, NSSF variants, BSF) is silently
   dropped and will never cache. Worth auditing before it bites the same way.

6. **The rest of the UDR shares F5's race pattern.**
   `UESubsData.EeSubscriptionCollection`, plus `EeSubscriptionIDGenerator`,
   `PolicyDataSubscriptionIDGenerator` and
   `SubscriptionDataSubscriptionIDGenerator`, are all plain fields with the same
   unsynchronized access shape. F5 fixed only the SDM path because that is what
   crashed. The EE path is likely a latent crash on any deployment that uses it.

7. **NRF re-decodes every profile on every discovery.**
   `nrf/producer/nf_discovery.go` runs `util.Decode` over the full result set per
   query and falls back to loading `urilist` wholesale when a filter misses —
   the source of the 7–9 `urilist` ops/attach seen before F3. Less relevant now
   that discovery is cached, but it makes any cache-cold burst expensive.

8. **Per-call SBI client construction.**
   Every omec consumer builds a fresh `NewAPIClient` per request (~50 service
   structs allocated). Connection pooling survives (`&http.Client{}` has a nil
   Transport, so it shares `http.DefaultTransport`), so this is allocation
   churn, not network cost. Low impact, easy win.

9. **`ToBsonM` double-marshals.**
   `amf/context/db.go:120` builds a bson document by `json.Marshal` →
   `json.Unmarshal` of the entire UE context. Runs 6x per attach when
   `enableDBStore` is on. Measured not to be the ceiling (see Ruled out), but
   it is pure waste.

10. **Kafka publish per GMM state transition.**
    `amf.PublishUeCtxtInfo()` is called from ~13 sites, several per attach, when
    `enableKafka: true`. Not isolated; low measured CPU (3.7 ms/attach).

---

## Reproducing

Requires the ATB-01 testbed and `ORBIT_KI`/`ORBIT_OPC` (from the `simapp`
configmap; never commit these).

```bash
# ceiling sweep — flat throughput + linear latency growth == saturation
for C in 64 128 256; do
  orbit load --amf 172.17.50.11:38412 --base-imsi 208930100007500 \
    --count 2000 --concurrency $C --gnb-count 4 \
    --mcc 208 --mnc 93 --tac 1 --sst 1 --sd 010203 \
    --ki "$ORBIT_KI" --opc "$ORBIT_OPC"
done
```

**Subscriber provisioning.** Only 100 IMSIs ship provisioned (…7500–7599), which
is far too few to saturate the core — a 100-UE run now completes in ~530 ms.
10,000 more were added by cloning the `imsi-208930100007500` documents across
the six collections that key on `ueId`:
`authentication.subscriptionData.authenticationData.authenticationSubscription`,
`aether.subscriptionData.provisionedData.{amData,smData,smfSelectionSubscriptionData}`,
and `aether.policyData.ues.{amData,smData}`. Cloning preserves Ki/OPc, so one
credential pair covers 208930100007500–208930100017599.

**Attribution tooling.** Per-pod CPU deltas read `cpu.stat` from each pod's
slice under `/sys/fs/cgroup/kubepods.slice`; Mongo op counts come from
`db.adminCommand({top:1})` deltas around a run; AMF goroutine dumps from
`http://127.0.0.1:5001/debug/pprof/goroutine?debug=1` inside the pod.

**Validating patched NFs without an image build.** Neither host has docker or
buildah (the node has only `ctr`). Patched NFs were run by building a static
binary (`go mod edit -replace` onto the patched library, `CGO_ENABLED=0 go
build`), copying it to `/opt/orbit-patch` on the node, and mounting it into the
pod via a hostPath volume plus a container `command` override. Reversible by
removing the volume/mount and restoring the original command.

---

## Upstream state

| Fix | Repo | State | Blocks |
|---|---|---|---|
| F2 — profiles without SUPI ranges are unrestricted | `omec-project/openapi` | committed `53e1d00`, **not pushed** | — |
| F3 — register a match filter for UDR | `omec-project/openapi` | committed `2211d72`, **not pushed** | the 8x |
| F4 — stop stalling cache hits behind an in-flight NRF query | `omec-project/openapi` | committed `2df2b1b`, **not pushed** | — |
| F5 — UDR concurrent map writes | `omec-project/udr` | **uncommitted**, tested under `-race` | any scale-out |
| F1 — MongoDB CPU cap | ATB-01 deployment | applied, persisted in StatefulSet | — |
| F6 — HTTP/2 connection pinning | not started | — | next ceiling |

F2/F3/F4 are one branch (`fix/nrfcache-unrestricted-supi-ranges`) against
`openapi` `main`, DCO signed, `gofmt`/`vet`/full suite clean. F5 is a crash bug
in a different repo and warrants its own PR.

<a id="recheck-list-when-the-fixes-land"></a>

## Recheck list — when the fixes land

Work items gated on the patches going upstream. Nothing here should be actioned
before then, and nothing above the stock number should be published until the
first three boxes are ticked.

**Gated on F2+F3+F4 released in an `omec-project/openapi` tag:**

- [ ] Confirm the released tag actually contains all three commits, and note the
      version here.
- [ ] Rebuild AMF/UDM/UDR from **stock upstream images** built against that tag —
      not the hostPath binaries — and re-run the ceiling sweep. The ~190 figure
      must be reproduced on unpatched images before it is quoted anywhere.
- [ ] Remove the hostPath patch mechanism from ATB-01 (drop the `orbit-patch`
      volume and mount from each deployment, restore `command` to
      `["/opt/<nf>-run.sh"]`), so the testbed is stock again.
- [ ] Re-run the full [ceiling sweep](#reproducing) and replace the numbers in
      the tables above, including the "quotable number" banner.

**Gated on F5 landing in `omec-project/udr`:**

- [ ] Verify the UDR survives a 2000-UE run on a stock image. Until then, any
      scale run on an unpatched UDR is expected to abort with exit 134.
- [ ] Audit the EE-subscription path for the same race (Suspect 6) and decide
      whether it ships in the same PR.

**ORBIT-side updates, gated on all of the above:**

All of the numbers below are **correct today** for stock SD-Core. They are
listed so they can be revisited when that stops being true — not because they
are wrong now.

- [ ] `docs/DESIGN.md:25` — pivotal decision §1, "SD-Core's own validated
      ceiling is ~5,000 UEs @ 10 attach/s" and "baseline 5k UEs / 10 aps".
- [ ] `docs/DESIGN.md:280` — Phase-2 verification checkpoint,
      "5,000 UEs @ 10 attach/s".
- [ ] `docs/DESIGN.md:283` — Phase-2 status. The narrative ("the core serializes
      attaches, it does not drop them") is now *explained* rather than
      contradicted — the serialization was the nrfcache lock (F3/F4), not
      anything intrinsic. Reword rather than delete.
- [ ] `docs/DESIGN.md:379` — risks table, "5k UEs / 10 aps is the stated SD-Core
      baseline".
- [ ] `README.md:40` — the "~10 attach/s" integration-capability line.
- [ ] `examples/fleet.yaml:47` — `attach_rate: 10/s`, chosen to match the
      measured ceiling.
- [ ] `docs/USAGE.md:170` — the `--slo-reg-p99 3s` example, sized for the old
      latency profile.
- [ ] Re-evaluate whether ORBIT is still the measurement bottleneck at the new
      rate. The two-process control (see Ruled out) was run at 22/s; it needs
      repeating near 200/s before ORBIT can be assumed transparent.

**Independent of upstream:**

- [ ] Keep findings in *this* file. Performance work on SD-Core belongs here, not
      in a new document per investigation. Promote a finding out of *Suspects*
      into *Proven* only when it has a measurement and an upstream file/line.

## Testbed state

ATB-01 is currently running **patched** AMF, UDM and UDR binaries via the
hostPath mechanism above, and MongoDB at 8 CPU / 6Gi. Restoring stock behaviour
means removing the `orbit-patch` volume and mount from each deployment and
restoring `command` to `["/opt/<nf>-run.sh"]`. The Mongo resource bump is a
StatefulSet edit and survives restarts by design.
