# NAS-RETRANSMIT-NEW-COUNT

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). Versions read (issue #252, which records the read of every version from V15.0.0 to V20.1.0; the 4.4.3.1 sentence was read again on 2026-11-10): V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`).

## TS 24.501 4.4.3.1 (NAS COUNT: the sender's increase), shall

> "After each new or retransmitted outbound SECURITY PROTECTED 5GS NAS MESSAGE message, the sender shall increase the NAS COUNT number by one, except for the initial NAS messages if the lower layers indicated the failure to establish the RRC connection (see 3GPP TS 38.331 [30])."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:**
  - Rel-15: V15.0.0 says "security protected NAS message"; from V15.1.0 it says "SECURITY PROTECTED 5GS NAS MESSAGE message". The increase by one after each new or retransmitted message is in every version.
  - Rel-16: from V16.5.1 the sentence that follows speaks of "the stored NAS overflow counter", and estimation rules for the receiver come after it.
  - Rel-18: "subclause" becomes "clause".
- **The exception** (initial NAS messages after an RRC connection establishment failure) applies to the UE only.
- **Use in the check:** a retransmission is an outbound protected message, so its NAS COUNT is higher than that of the transmission before it. A byte-identical resend carries the same COUNT and is FAIL.

## TS 24.501 5.5.1.2.8 c) (the stimulus: T3550 time-out), shall

> "On the first expiry of the timer, the AMF shall retransmit the REGISTRATION ACCEPT message and shall reset and restart timer T3550."

- **Since:** Rel-15, V15.0.0. The retransmission wording is unchanged through V19.8.0; V15.0.0 has the AMF enter 5GMM-DEREGISTERED on the abort, 5GMM-REGISTERED from V15.7.0 (see NAS-T3550-ACCEPT-RETRANSMIT).
- **Use in the check:** it produces the retransmissions. The UE withholds its REGISTRATION COMPLETE after the REGISTRATION ACCEPT, as NAS-T3550-ACCEPT-RETRANSMIT does. Whether, how often and when the AMF retransmits is graded there, not here: with no retransmission this check SKIPs.

## Out of scope

- **AUTHENTICATION REQUEST** (T3560) is sent before the security mode control procedure, as plain NAS with no COUNT.
- **SECURITY MODE COMMAND** (T3560): the text does not settle it. 5.4.2.2 (every version) has the AMF "reset the downlink NAS COUNT counter and use it to integrity protect the initial SECURITY MODE COMMAND message", and 5.4.2.3 lets the UE accept a command "successfully integrity checked using an estimated downlink NAS COUNT equal to 0". A retransmitted command could therefore legally restart at COUNT 0, so it is not tested.

## How the COUNT is read

A protected NAS PDU carries only the COUNT's sequence number (octet 7). For the first REGISTRATION ACCEPT and each retransmission the engine checks the MAC at two candidate COUNTs, without changing the UE's security context: the received sequence number with the stored downlink overflow counter, and with that counter plus one. Together they cover every COUNT from 255 below the stored one to 256 above it, so a repeat or a lower COUNT is found, and a wrap of the sequence number (255 to 0 with the overflow counter incremented) counts as an increase. The PDU is taken as received (`NASEvent.Raw`), whatever the security context made of it.

- **PASS:** every retransmission's COUNT is higher than the transmission before it. Observed lists each COUNT. The check stops at the fourth retransmission.
- **FAIL:** a retransmission's COUNT is the same as, or lower than, the one before it. The detail says when the two PDUs are byte-identical.
- **ERROR:** a retransmission verifies at neither candidate. The detail gives the PDU in hex and both candidates. After the first Accept, any protected downlink that verifies at neither candidate is taken for such a retransmission, since under NEA1 to NEA3 its type cannot be read.
- **ERROR:** no REGISTRATION ACCEPT that verifies within 10 s, or a reject.
- **SKIP:** no retransmission within 1.5 × 18 s (the largest T3550 in Table 10.2.2); no UE; or the core selected 5G-IA0, which the UE harness refuses at the security mode control procedure, so no protected REGISTRATION ACCEPT is seen.

## What the check leaves behind

Like NAS-T3550-ACCEPT-RETRANSMIT, the check never sends REGISTRATION COMPLETE, so the UE is not deregistered: the association is closed, and the AMF, which enters 5GMM-REGISTERED when it aborts on the fifth expiry, keeps the UE until it deregisters it implicitly.
