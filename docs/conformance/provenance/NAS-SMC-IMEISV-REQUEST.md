# NAS-SMC-IMEISV-REQUEST

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/) and [https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/](https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/). Versions read (2026-11-10): TS 24.501 V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`); TS 33.501 V15.0.0 (`33501-f00.zip`), V15.20.0 (`33501-fk0.zip`), V16.20.0 (`33501-gk0.zip`), V17.16.0 (`33501-hg0.zip`), V18.12.0 (`33501-ic0.zip`), V19.7.0 (`33501-j70.zip`).

## TS 24.501 5.4.2.2 IMEISV request (NAS security mode control initiation by the network), descriptive

> "Additionally, the AMF may request the UE to include its IMEISV in the SECURITY MODE COMPLETE message."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`). The sentence is the same in all six versions.
- **Strength:** descriptive. It is a "may": the AMF is free to request the IMEISV or not. The IE's description (IMEISV request, in the SECURITY MODE COMMAND message content) says the same: "The AMF may include this information element to request the UE to send its IMEISV with the corresponding SECURITY MODE COMPLETE message" (V15.0.0, V15.7.0).

## TS 33.501 6.7.2 step 2b (NAS security mode command procedure), descriptive

> "The NAS Security Mode Complete message shall include PEI in case AMF requested it in the NAS Security Mode Command message."

- **Since:** Rel-15, V15.0.0 (`33501-f00.zip`), where it is in an unnumbered paragraph; step 2b from V15.20.0 through V19.7.0, the sentence unchanged.
- **Strength:** descriptive for this check. The shall is about what the UE's message contains. The stimulus breaks it on purpose.

## Also read, not cited

- **TS 24.501 5.4.2.3:** "If the AMF indicated in the SECURITY MODE COMMAND message that the IMEISV is requested, the UE shall include its IMEISV in the SECURITY MODE COMPLETE message" (V15.0.0, V15.7.0); from V16.15.0 the sentence is split by case (a UE that supports a 3GPP access technology, one that does not, a 5G-RG or W-AGF). The UE's obligation again.
- **TS 24.501 5.4.2.4** says nothing about the IMEISV.

## Why there is nothing to grade

Requesting the IMEISV is optional for the AMF, and no clause read says what the AMF does when a requested IMEISV is missing from the SECURITY MODE COMPLETE. The check is informational.

## What the check does

One conformant registration without a PDU session, except that the SECURITY MODE COMPLETE carries no IMEISV whether the SECURITY MODE COMMAND requested it or not. An IDENTITY REQUEST that follows is answered with the identity it asks for, so the registration can complete. The UE does not answer a second SECURITY MODE COMMAND: the registration is stopped there.

## Verdict

- **Verdict class:** INFO (informational). The check reports and does not grade: it gives no PASS or DEVIATION, FAIL only for a core that does not survive the stimulus, and it cannot be put on an expected-failures list.
- **Observed** has two halves: whether the SECURITY MODE COMMAND requested the IMEISV, and what the core did about its absence.
- **INFO:** `IMEISV requested in the SECURITY MODE COMMAND; left out, the core asks for it with an IDENTITY REQUEST`: the request 5.4.2.2 allows, and the identification procedure (5.4.3) for the identity that is missing.
- **INFO-DIFF:** anything else:
  - `SECURITY MODE COMMAND does not request the IMEISV; the omission was not exercised`: the second half does not apply, and the registration goes on as any other;
  - `IMEISV requested in the SECURITY MODE COMMAND; left out, the registration is accepted without it`;
  - `...; left out, REGISTRATION REJECT, 5GMM cause #n`;
  - `...; left out, IDENTITY REQUEST for another identity (IMEI)`: an identification procedure for another identity than the IMEISV, named in Observed;
  - `...; left out, a second SECURITY MODE COMMAND`;
  - `...; left out, UE CONTEXT RELEASE COMMAND`;
  - `...; left out, no answer`, the absence inferred within 10 s.
- **FAIL:** a fresh NG Setup after the check does not complete: the core did not survive. Observed is `AMF unresponsive after the message (<what was observed>)`, and it gates.
- **ERROR:** an Authentication Reject, a Registration Reject that cannot be read, or a registration that ended before any SECURITY MODE COMMAND.
- **SKIP:** no UE configured, or a target release older than Rel-15.
- **State left:** after an Accept the UE is deregistered (switch-off); otherwise the association is closed with the UE not registered. The detail says which.
