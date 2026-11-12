# NGAP-UE-CTX-RELEASE-ON-REQUEST

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: V15.0.0 (`38413-f00.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.1.0 (`38413-i10.zip`), V18.11.0 (`38413-ib0.zip`), V19.4.0 (`38413-j40.zip`).

## TS 38.413 8.3.2.2 (UE Context Release Request, Successful Operation, "Interactions with UE Context Release procedure"), should, primary

> "The UE Context Release procedure should be initiated upon reception of a UE CONTEXT RELEASE REQUEST message with the Cause IE set to a value different than "User inactivity". The UE Context Release procedure should be initiated upon reception of a UE CONTEXT RELEASE REQUEST message with the Cause IE set to "User inactivity" and there is no downlink signaling, as specified in TS 23.502 [10]."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`), in the form "The UE Context Release procedure should be initiated upon reception of a UE CONTEXT RELEASE REQUEST message."
- **Changes:** Rel-15 to Rel-17 have the unconditional "should" sentence, unchanged. Rel-18 V18.1.0 (`38413-i10.zip`) splits it by cause, adding the "User inactivity" plus no-downlink-signalling condition. In Rel-19 the quoted text is unchanged; V19.1.0 adds a GW Context Release Indication sentence.
- **Strength:** "should", not "shall". An AMF that sends no UE CONTEXT RELEASE COMMAND misses a recommendation, so the check's worst verdict is DEVIATION.

## The stimulus

The request's Cause IE is radio network "release due to NG-RAN generated reason". It is never "User inactivity": from V18.1.0 the AMF is expected to release on that cause only when there is no downlink signalling, which the check cannot know. With any other cause the "should" is unconditional in every version read.

The UE is registered without a PDU session, so the release concerns the NG connection alone.

## What the check does

It registers the UE on its gNB, sends the UE CONTEXT RELEASE REQUEST, and waits 5 s for the UE CONTEXT RELEASE COMMAND, which it answers with UE CONTEXT RELEASE COMPLETE.

A UE whose context was released is still registered in the core, with no NG connection. The check then deregisters it the way such a UE does: a switch-off Deregistration Request sent as an initial NAS message (integrity protected, not ciphered), in an Initial UE Message that carries the UE's 5G-S-TMSI under a new RAN UE NGAP ID. The AMF's release of that connection is answered under the IDs it names. This needs the 5G-GUTI from the Registration Accept; a core that assigned none leaves the UE registered, and the detail says so. A UE the AMF did not release is deregistered over its NG connection as usual.

## Verdict

- **Class:** conformance, one should (8.3.2.2): the check's worst verdict is DEVIATION, never FAIL. The check owns the procedure "AN release": a check that needs a UE moved to CM-IDLE and whose release the core does not complete reports BLOCKED and names this check.
- **Stimulus:** as above; the UE CONTEXT RELEASE COMMAND is awaited for 5 s.
- **PASS** `release-command`: a UE CONTEXT RELEASE COMMAND within the 5 s. Its cause is recorded, not asserted.
- **DEVIATION** `no-release-command` (8.3.2.2): none within the 5 s, a bound of the check (TS 38.413 sets no timer for the command); the absence is inferred within it.
- **DEVIATION** `association-lost` (8.3.2.2): the association was lost while the check waited for the command.
- **Not completed:** `no-release-command` and `association-lost` report the AN release as not completed, so the BLOCKED results of dependent checks in the same run are counted under this one.
- **BLOCKED** `blocked-registration`: the core did not complete the UE's registration (`NAS-INITIAL-REGISTRATION` owns whether it completes); `blocked-ng-setup`: nor the NG Setup before it.
- **CRASH:** after no command or a lost association, a fresh NG Setup from another gNB ID does not complete.
- **ERROR:** the harness could not be set up, the request could not be sent, or the per-test timeout expired during the wait.
- **SKIP:** no UE configured (`skip-no-ue`), or a target release older than Rel-15.
- **Detail:** the command's cause, other PDUs read while waiting (an Error Indication, for example), and how the cleanup went. It never includes key material.
- **Limits:** no fresh NG Setup is tried afterwards, so the check reports no CRASH.

Message for a missing command: `no UE CONTEXT RELEASE COMMAND after UE CONTEXT RELEASE REQUEST. TS 38.413 8.3.2.2 (should; Rel-18 V18.1.0: split by cause). Likely cause: AMF ignores RAN-initiated release.`
