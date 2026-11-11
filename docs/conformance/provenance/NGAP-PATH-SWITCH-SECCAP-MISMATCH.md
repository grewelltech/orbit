# NGAP-PATH-SWITCH-SECCAP-MISMATCH

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/](https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/). Versions read (2026-11-10): V15.0.0 (`33501-f00.zip`), V15.20.0 (`33501-fk0.zip`), V16.20.0 (`33501-gk0.zip`), V17.16.0 (`33501-hg0.zip`), V18.12.0 (`33501-ic0.zip`), V19.7.0 (`33501-j70.zip`).

## TS 33.501 6.7.3.1 (Xn-handover), shall

V15.0.0:

> "In the Path-Switch message, the target gNB shall send the UE's 5G security capabilities received from the source gNB to the AMF. The AMF shall verify that the UE's 5G security capabilities received from the target gNB are the same as the UE's 5G security capabilities that the AMF has locally stored. If there is a mismatch, the AMF shall send its locally stored 5G security capabilities of the UE to the target gNB in the Path-Switch Acknowledge message. Additionally, the AMF may log the event and may take additional measures, such as raising an alarm."

V15.20.0 to V19.7.0:

> "The AMF shall verify that the UE's 5G security capabilities received from the target gNB/ng-eNB are the same as the UE's 5G security capabilities that the AMF has locally stored. If there is a mismatch, the AMF shall send its locally stored 5G security capabilities of the UE to the target gNB/ng-eNB in the Path-Switch Acknowledge message. The AMF shall support logging capabilities for this event and may take additional measures, such as raising an alarm."

- **Since:** Rel-15, V15.0.0 (`33501-f00.zip`).
- **Changes:** within Rel-15 the target becomes "gNB/ng-eNB", and the last sentence changes from "Additionally, the AMF may log the event and may take additional measures, such as raising an alarm" to "The AMF shall support logging capabilities for this event and may take additional measures, such as raising an alarm". The two sentences the check asserts ("shall verify", "shall send its locally stored ...") are otherwise the same in all six versions.
- **Not asserted:** the logging and the alarm. Neither is visible on N2.
- **The next paragraph** of 6.7.3.1 is the target's side: "If the target gNB/ng-eNB receives UE's 5G security capabilities from the AMF in the Path-Switch Acknowledge message, the target gNB/ng-eNB shall update the AS security context of the UE with these 5G security capabilities of the UE." It is why the stored copy has to come back.

## What the check does

It registers a UE with one PDU session on gNB A and reads the UE Security Capabilities the AMF supplied in the INITIAL CONTEXT SETUP REQUEST: the AMF's stored copy as the RAN was given it. It then hands the UE over to gNB B by Xn. The PATH SWITCH REQUEST from gNB B carries those capabilities with the bit of algorithm 3 inverted in the NR encryption and the NR integrity bitmaps, so they always differ from the stored ones in both. Only this check sends capabilities the AMF did not supply. It waits 5 s for the answer, deregisters the UE from the gNB it is then on, and tries a fresh NG Setup.

## Verdicts

- **PASS:** `PATH SWITCH REQUEST ACKNOWLEDGE carries the AMF's stored UE security capabilities`: its UE Security Capabilities IE equals, in all four bitmaps, what the AMF supplied at registration.
- **FAIL:**
  - `PATH SWITCH REQUEST ACKNOWLEDGE without UE Security Capabilities`;
  - `PATH SWITCH REQUEST ACKNOWLEDGE echoes the mismatching UE security capabilities`;
  - `PATH SWITCH REQUEST ACKNOWLEDGE carries UE security capabilities that are neither the stored nor the sent ones`;
  - `no PATH SWITCH REQUEST ACKNOWLEDGE`: a PATH SWITCH REQUEST FAILURE, another PDU, a lost association or no answer within 5 s. The detail says which. The stored capabilities can only come in the Acknowledge, so without one they were not sent. `NGAP-PATH-SWITCH-ACK-TRANSFER` shows whether the same core acknowledges a Path Switch Request with matching capabilities.
  - `AMF unresponsive after the message (...)`: the fresh NG Setup afterwards does not complete. The core did not survive the request.

  Message: "the AMF did not send its locally stored UE security capabilities in the PATH SWITCH REQUEST ACKNOWLEDGE after a PATH SWITCH REQUEST whose UE Security Capabilities differ from them. TS 33.501 6.7.3.1. Likely cause: the AMF does not compare the capabilities of the Path Switch Request with its stored copy."
- **ERROR:** the registration or the PDU session failed, the AMF supplied no UE Security Capabilities at registration (`no UE Security Capabilities in the INITIAL CONTEXT SETUP REQUEST`: nothing to compare with), the target's NG Setup failed, or the per-test timeout ended first.
- **SKIP:** no UE configured, or a target release older than Rel-15.
- **Detail:** the three sets of capabilities (supplied, sent, in the Acknowledge), each as its four bitmaps. It never includes key material.
- **State left:** the UE is deregistered (switch-off) from gNB B after an Acknowledge and from gNB A without one, and both associations are closed.
- **Budget:** up to 25 s.
