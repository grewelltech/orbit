# NGAP-NG-SETUP

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: V15.0.0 (`38413-f00.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`) and V19.4.0 (`38413-j40.zip`), the latest per release.

## TS 38.413 8.7.1.2 (NG Setup, Successful Operation), descriptive, primary

> "The NG-RAN node initiates the procedure by sending an NG SETUP REQUEST message including the appropriate data to the AMF. The AMF responds with an NG SETUP RESPONSE message including the appropriate data."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Strength:** descriptive. The text says "responds", and it describes the successful case only: nothing obliges an AMF to accept a setup.
- **Changes:** the two sentences are unchanged from V15.0.0 to V18.11.0. In V19.4.0 the same words are a bullet under "If the NG Setup procedure is executed between the NG-RAN node and the AMF".

## TS 38.413 8.7.1.3 (NG Setup, Unsuccessful Operation), should

> "If the AMF cannot accept the setup, it should respond with an NG SETUP FAILURE message and appropriate cause value."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Changes:** none; the sentence is the same in all six versions. V19.4.0 adds a second figure and a sentence applying the text to an AIOTF.

## Verdict

- **Class:** characterisation with one should. The check owns the procedure "NG Setup": a check whose own NG Setup the core does not complete reports BLOCKED and names this check.
- **Stimulus:** one association from the suite's N2 source address and one NG SETUP REQUEST with the run's own identity (PLMN, tracking area, slices) and the check's own gNB ID. The answer is awaited for 4 s.
- **INFO** `accepted`: NG SETUP RESPONSE, the answer 8.7.1.2 describes.
- **WARN (availability)** `rejected`: NG SETUP FAILURE, with its cause in the detail. No requirement is violated: 8.7.1.3 lets an AMF that cannot accept the setup reject it, and this is the answer it asks for. The criterion is availability: the core cannot complete the procedure for the gNB the run presents.
- **DEVIATION** `unanswered`, `other-pdu`, `association-lost`: the AMF neither accepted the setup nor answered with NG SETUP FAILURE. An AMF that sends no NG SETUP RESPONSE has not accepted the setup, and 8.7.1.3 says it should then respond with NG SETUP FAILURE. `unanswered` is inferred from the absence of both messages within 4 s, a bound of the check: TS 38.413 sets no timer for the NG Setup outcome.
- **Not completed:** every outcome but `accepted` reports the NG Setup as not completed, so the BLOCKED results of dependent checks in the same run are counted under this one.
- **ERROR:** the association could not be opened, the request could not be built or sent, or the per-test timeout expired before the 4 s wait.
- **SKIP:** a target release older than Rel-15.
- **Limits:** the run's preflight completes an NG Setup before any check runs, with the same identity and another gNB ID. This check therefore reports a core that accepted that setup and does not accept this one. The content of the NG SETUP RESPONSE is not asserted.
