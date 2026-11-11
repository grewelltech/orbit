# NGAP-DUPLICATE-RAN-NODE-ID

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: V15.0.0 (`38413-f00.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`) and V19.4.0 (`38413-j40.zip`), the latest per release.

## TS 38.413 8.7.1.2 (NG Setup, Successful Operation), descriptive, primary

> "The NG-RAN node initiates the procedure by sending an NG SETUP REQUEST message including the appropriate data to the AMF. The AMF responds with an NG SETUP RESPONSE message including the appropriate data."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Strength:** descriptive. The text says "responds", and it describes the successful case only.
- **Changes:** the two sentences are unchanged from V15.0.0 to V18.11.0. In V19.4.0 the same words are a bullet under "If the NG Setup procedure is executed between the NG-RAN node and the AMF".

## TS 38.413 9.3.1.5 (Global RAN Node ID), descriptive

> "This IE is used to globally identify an NG-RAN node (see TS 38.300 [8])."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Changes:** none; the sentence is the same in all six versions.

## TS 38.413 8.7.1.3 (NG Setup, Unsuccessful Operation), should

> "If the AMF cannot accept the setup, it should respond with an NG SETUP FAILURE message and appropriate cause value."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Changes:** none; the sentence is the same in all six versions (re-read in the extracted texts of all six for this check's declaration, 2026-11-11).
- **Use in the check:** whether an AMF can accept a duplicate is what the specification leaves open, so a rejection satisfies this sentence and an acceptance does not offend it. An AMF that sends no NG SETUP RESPONSE has not accepted the setup, and this sentence says it should then answer with NG SETUP FAILURE: no answer, another PDU or a lost second association does not follow it. This is the same reading `NGAP-NG-SETUP` gives a fresh NG SETUP REQUEST.

## What the specification does not say (cited here only, not in the check's references)

- **No rule for a duplicate.** None of the six versions says what an AMF does with an NG SETUP REQUEST whose Global RAN Node ID a live association already presents. A search of each version for "duplicate" and for "same Global RAN Node ID" finds only change-history entries on other subjects.
- **TS 38.413 8.7.1.1 (NG Setup, General), descriptive:** "This procedure erases any existing application level configuration data in the two nodes, replaces it by the one received and clears AMF overload state information at the NG-RAN node." Present in V15.13.0 to V19.4.0, absent in V15.0.0. It speaks of the two nodes of one NG-C interface instance; it does not say that a second association from another address is the same node.
- **TS 38.413 8.7.2.2 (RAN Configuration Update, Successful Operation), shall:** "If the Global RAN Node ID IE is included in the RAN CONFIGURATION UPDATE message, the AMF shall associate the TNLA to the NG-C interface instance using the Global RAN Node ID." Present in V15.13.0 to V19.4.0, absent in V15.0.0. This is how one NG-RAN node adds a TNL association to its NG-C interface instance: with a RAN CONFIGURATION UPDATE on the new association, not with a second NG SETUP REQUEST. So several associations may share one Global RAN Node ID, and the stimulus here, a second NG Setup, is the case the text does not cover.

## Verdict

- **Class:** characterisation with one should (8.7.1.3). The specification is silent on the duplicate itself.
- **Stimulus:** one association from the suite's N2 source address completes NG Setup. A second association is opened from the second N2 source address (the second address of the target's `ran.n2_source_ips`) and sends an NG SETUP REQUEST with the same Global RAN Node ID, PLMN, tracking area and slices. Its answer is awaited for 4 s; the first association is then watched for 2 s, at transport level: nothing is sent on it.
- **Observed** has two halves, joined by "; ":
  - what the second NG Setup got: `second NG Setup accepted` (NG SETUP RESPONSE), `second NG Setup rejected (NG SETUP FAILURE)` (the cause is in the detail), `second NG Setup unanswered` (neither within 4 s, the absence inferred within that bound), `second NG Setup answered with another PDU` (named in the detail), or `second association reset`;
  - what became of the first association: `first association kept` or `first association dropped`.
- **INFO** `accepted`: NG SETUP RESPONSE with the first association kept, the answer 8.7.1.2 describes with nothing else changed.
- **INFO** `rejected`: NG SETUP FAILURE with the first association kept. 8.7.1.3 lets an AMF that cannot accept a setup reject it, and the node is still served on the association it set up first, so none of the WARN criteria applies.
- **WARN (availability)** `accepted-first-dropped`, `rejected-first-dropped`: the duplicate was answered and the first association dropped. No requirement covers it; the node loses the association the AMF served it on.
- **DEVIATION** `unanswered`, `other-pdu`, `association-lost` (8.7.1.3): the AMF neither accepted the duplicate nor answered it with NG SETUP FAILURE, whatever became of the first association (Observed says). `unanswered` is inferred from the absence of both messages within 4 s, a bound of the check: TS 38.413 sets no timer for the NG Setup outcome.
- **CRASH:** both associations are closed and a fresh NG Setup with another gNB ID is tried; a core that no longer completes it did not survive the duplicate NG SETUP REQUEST. Observed is `AMF unresponsive after the message (<the two halves>)`. While a population loads the core the same silence is ERROR.
- **BLOCKED:** the check's first NG Setup did not complete (`blocked-ng-setup`; `NGAP-NG-SETUP` owns whether it completes).
- **ERROR:** the stimulus could not be built or sent; the second N2 source address is not on this host (`second N2 source address is not on this host`), a configuration fault; the second association could not be opened for another reason; or the per-test timeout expired during one of the waits.
- **SKIP:** no second N2 source address, or one that is the first (`skip-no-second-n2-source-address`); a target release older than Rel-15. In a run with a population the suite has one N2 source address, so the check SKIPs there.
- **Limits:**
  - "kept" means the first association was not ended within 2 s. An AMF that leaves it open but no longer serves it is reported as keeping it.
  - The check does not look at which association the AMF then uses for the gNB ID (a handover or a paging toward it). That needs a UE and a third gNB and is not built.
  - A second association the core refuses at the transport level cannot be told from a testbed that does not route the second address, so it is ERROR, not a verdict on the core.
