# NAS-BADMAC-REGISTRATION-AFTER-SMC

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). Versions read (issue #253, which records the read of every version from V15.0.0 to V20.1.0; the V15.0.0 and V19.8.0 texts quoted below were read again on 2026-11-10): V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.3.0 (`24501-g30.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.0.0 (`24501-i00.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`).

## TS 24.501 4.4.4.3 (Integrity checking of NAS signalling messages in the AMF), shall, primary

The clause has the same four parts in every version.

1. > "Except the messages listed below, no NAS signalling messages shall be processed by the receiving 5GMM entity in the AMF or forwarded to the 5GSM entity, unless the secure exchange of NAS messages has been established for the NAS signalling connection"

2. > "Once a current 5G NAS security context exists, until the secure exchange of NAS messages has been established for the NAS signalling connection, the receiving 5GMM entity in the AMF shall process the following NAS signalling messages, even if the MAC included in the message fails the integrity check or cannot be verified, as the 5G NAS security context is not available in the network"

   The list includes REGISTRATION REQUEST.

3. Message-specific rules, with no scope words of their own. For the check's stimulus (V19.8.0):

   > "If a REGISTRATION REQUEST message for mobility and periodic registration update fails the integrity check and the UE provided EPS NAS message container IE which was successfully verified by the source MME, the AMF may create a mapped 5G NAS security context and initiate a security mode control procedure to take the new mapped 5G NAS security context into use; otherwise if the UE has only a non-emergency PDU session established, the AMF shall initiate a primary authentication and key agreement procedure to create a new native 5G NAS security context. Additionally, the AMF shall initiate a security mode control procedure, and include the Additional 5G security information IE with the RINMR bit set to "Retransmission of the initial NAS message requested" in the SECURITY MODE COMMAND message as specified in clause 5.4.2.2. If authentication procedure is not successful the AMF shall maintain, if any, the 5GMM-context and 5G NAS security context unchanged. For the case when the UE has an emergency PDU session see clause 5.5.1.3.3 and clause 5.4.1.3.5."

4. > "Once the secure exchange of NAS messages has been established for the NAS signalling connection, the receiving 5GMM entity in the AMF shall not process any NAS signalling messages unless they have been successfully integrity checked by the NAS. If any NAS signalling message, having not successfully passed the integrity check, is received, then the NAS in the AMF shall discard that message."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:**
  - Rel-15: V15.0.0 to V16.2.0 have one rule for every REGISTRATION REQUEST: "If a REGISTRATION REQUEST message fails the integrity check and the UE provided EPS NAS message container IE ...". V15.0.0 says "only a PDU session for non-emergency services" and has a HASHAMF IE where later versions have the RINMR bit. The same clause also says: "If a REGISTRATION REQUEST message fails the integrity check and it is not an registration request for emergency services, the AMF shall authenticate the subscriber before processing the registration request any further." (the grammar is the specification's).
  - Rel-16: V16.3.0 splits the rule into initial registration and mobility and periodic registration update.
  - Rel-18: V18.0.0 adds "If authentication procedure is not successful the AMF shall maintain, if any, the 5GMM-context and 5G NAS security context unchanged."; "subclause" becomes "clause".
  - Parts 1, 2 and 4 are unchanged from V15.0.0 through V19.8.0.

## No branch lets the AMF reject before authenticating

- The only alternative to authentication in part 3 is the mapped-context branch, which needs an EPS NAS message container. The check's UE sends none.
- The only failure branch, "If authentication procedure is not successful ...", follows an authentication.
- The emergency branch does not apply: the UE has no emergency PDU session.
- None of the clause's NOTEs (1 to 4) allows a reject.
- V15.0.0 to V16.2.0 say so outright: "the AMF shall authenticate the subscriber before processing the registration request any further".

## The ambiguity, and the grading rule

The text does not say whether the rules of part 3 apply only inside part 2's window ("until the secure exchange of NAS messages has been established") or at any time.

- **Reading A:** after secure exchange only part 4 applies. The message is discarded.
- **Reading B:** part 3 applies at any time. The AMF authenticates again before processing the request.

**A response is FAIL only if both readings forbid it. If either reading allows it, it is SKIP (the alternative the text permits), and Observed names the ambiguity. A release is neither reading's answer: WARN for security.** Applied to each response:

| Response after the stimulus | Reading A | Reading B | Verdict |
|---|---|---|---|
| Nothing within 5 s | required | allowed (the request is not processed) | PASS |
| UE CONTEXT RELEASE COMMAND, or the association goes, with no answer | not the discard | not the authentication | WARN (security) |
| AUTHENTICATION REQUEST | forbidden | required | SKIP |
| IDENTITY REQUEST | forbidden | allowed: the AMF may ask for the SUCI it then authenticates (the identification procedure before a primary authentication) | SKIP |
| REGISTRATION ACCEPT with no AUTHENTICATION REQUEST before it | forbidden | forbidden (authentication first) | FAIL |
| REGISTRATION REJECT | forbidden | forbidden (not the required authentication) | FAIL |
| SECURITY MODE COMMAND with no authentication first | forbidden | forbidden | FAIL |
| 5GMM STATUS | forbidden | forbidden | FAIL |
| AUTHENTICATION REJECT | forbidden | forbidden (no authentication ran) | FAIL |
| DEREGISTRATION REQUEST (network-initiated) | forbidden | forbidden | FAIL |
| Any other 5GMM message (CONFIGURATION UPDATE COMMAND, DL NAS TRANSPORT, ...) | not an answer to a registration | not an answer to a registration | recorded, not judged; the watch goes on |
| A downlink NAS PDU that does not verify or decode, and no classified answer | | | ERROR: the answer cannot be classified |

The first classified answer decides. The IDENTITY REQUEST row is this check's own application of the rule (issue #253 lists AUTHENTICATION REQUEST, REGISTRATION REJECT, 5GMM STATUS and SECURITY MODE COMMAND and leaves the rest to the rule).

## The check

- **Precondition:** the UE registers, the security mode control procedure completes, and one PDU SESSION ESTABLISHMENT REQUEST is answered by a session being set up (a PDU SESSION RESOURCE SETUP REQUEST or a PDU SESSION ESTABLISHMENT ACCEPT). The part 3 shall depends on "only a non-emergency PDU session established", so a rejected or unanswered request is BLOCKED on the PDU session establishment, and a UE that does not register BLOCKED on the registration. A core that selects 5G-IA0 SKIPs for the missing input "NAS integrity protection".
- **Stimulus:** on the same connection, a REGISTRATION REQUEST with 5GS registration type "mobility registration updating", the UE's 5G-GUTI, the ngKSI of the current context (read from the SECURITY MODE COMMAND), the UE security capability and the session's slice as requested NSSAI, and no EPS NAS message container. It is protected under the current context at the next uplink NAS COUNT with security header type "integrity protected and ciphered", like the UE's other uplinks on the connection, and its four MAC octets are inverted.
- **Not answered by the UE:** an AUTHENTICATION REQUEST gets no response. The AMF then retransmits it on T3560 and aborts.
- **Cleanup:** a PDU SESSION RELEASE REQUEST for the session and a switch-off DEREGISTRATION REQUEST under the old context, best effort, up to 3 s for the UE CONTEXT RELEASE COMMAND, and then the association is dropped. The detail says whether the core released the UE. After a re-authentication the AMF may not accept the deregistration, and then keeps the UE until T3560 runs out or its own timers expire. After a release only the association is dropped.

## Verdict

- **Class:** conformance, one shall (4.4.4.3), with the grading rule above for its two readings.
- **PASS** `discarded`: nothing answered the stimulus within 5 s. The discard is inferred from that absence, within the check's bound.
- **FAIL** `processed` (4.4.4.3): an answer both readings forbid, with no authentication first.
- **SKIP (permitted alternative)** `reauthenticated`, `identity-requested` (4.4.4.3): an AUTHENTICATION REQUEST, or an IDENTITY REQUEST that may start one. The authentication rule of 4.4.4.3 has the AMF authenticate a mobility REGISTRATION REQUEST that fails the integrity check; whether it still applies after secure exchange is the ambiguity above.
- **WARN (security)** `released`: the AMF released the UE, or the association went, with no answer. Neither reading has it: it is not the discard and not the authentication. No shall forbids a release, and the core acts on a message that failed the integrity check.
- **BLOCKED** (PDU session establishment): the PDU SESSION ESTABLISHMENT REQUEST was rejected or not answered within 10 s, or the UE was released before the stimulus. **BLOCKED** (registration): the UE did not register.
- **CRASH:** the AMF no longer completes an NG Setup after the stimulus.
- **ERROR:** the stimulus could not be built or sent, the NAS probe could not be opened, a downlink NAS PDU after the stimulus did not verify or decode so the answer cannot be classified, or a local socket failed.
- **SKIP:** no UE configured; the core selected 5G-IA0; a target release older than Rel-15.
