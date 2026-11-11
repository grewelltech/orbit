# NGAP-HANDOVER-CANCEL-ACK

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: V15.0.0 (`38413-f00.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`) and V19.4.0 (`38413-j40.zip`), the latest per release.

## TS 38.413 8.4.5.2 (Handover Cancellation, Successful Operation), descriptive, primary

> "The source NG-RAN node initiates the procedure by sending a HANDOVER CANCEL message to the AMF."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Strength:** descriptive. This is the whole of 8.4.5.2 besides its figure: it names the source's message and says nothing of the AMF.
- **Changes:** the sentence is the same in all six versions. V15.0.0 has no text under 8.4.5.4; from V15.13.0 through V19.4.0 it reads "If the source NG-RAN node becomes aware of the fact that an expected HANDOVER CANCEL ACKNOWLEDGE message is missing, the source NG-RAN node shall consider the Handover Cancellation procedure as successfully terminated." That shall binds the source NG-RAN node, not the AMF. 8.4.5.3 (Unsuccessful Operation) is "Not applicable."
- **Not read:** Figure 8.4.5.2-1 is an image.

## TS 38.413 Table 8.1-1 (Class 1 procedures), descriptive

- The row "Handover Cancellation | HANDOVER CANCEL | HANDOVER CANCEL ACKNOWLEDGE", the same in all six versions. Clause 3.1 defines the class: "Class 1: Elementary Procedures with response (success and/or failure)." The table names the procedure's response; it does not oblige the AMF to send one.

## TS 38.413 9.2.3.12 (HANDOVER CANCEL ACKNOWLEDGE), descriptive

> "This message is sent by the AMF to the source NG-RAN node to confirm that the ongoing handover was cancelled."

- The same in all six versions. It says what the message is for.

## Why there is nothing to grade

No version read has a sentence of the form "the AMF shall (or should) send a HANDOVER CANCEL ACKNOWLEDGE". The acknowledge appears only in the three places above, and 8.4.5.4 has the source treat a missing one as success. With no shall and no should addressed to the AMF, the check is a characterisation.

## What the check does

It registers a UE with one PDU session on gNB A and prepares an N2 handover to gNB B: Handover Required, the AMF's Handover Request at gNB B, gNB B's Handover Request Acknowledge, and the HANDOVER COMMAND at gNB A. It then sends HANDOVER CANCEL from gNB A (cause radio network "handover cancelled") instead of completing the handover, and waits 5 s for the acknowledge on gNB A's association.

gNB B's association stays open through the wait and up to 500 ms more, so that a UE Context Release Command the AMF sends for the context it prepared there is answered with UE Context Release Complete. Whether that release came is recorded. The UE never leaves gNB A and is deregistered there, and a fresh NG Setup is then tried.

## Verdict

- **Class:** characterisation: no shall or should, so no PASS, FAIL or DEVIATION. The check needs the N2 handover preparation, which `NGAP-HANDOVER-COMMAND-TRANSFER` owns.
- **Stimulus:** a HANDOVER CANCEL from gNB A, cause radio network "handover cancelled", after the HANDOVER COMMAND of a prepared N2 handover to gNB B.
- **INFO** `acknowledged`: HANDOVER CANCEL ACKNOWLEDGE within the 5 s. Observed: `HANDOVER CANCEL ACKNOWLEDGE after <time>`.
- **INFO** `not-acknowledged`: `no HANDOVER CANCEL ACKNOWLEDGE within 5s`, the absence inferred within that bound of the check (TS 38.413 sets no timer for the acknowledge). 8.4.5.4 has the source take a missing acknowledge as a successful cancellation, so no conformant peer is broken and no state is lost: none of the WARN criteria applies. The detail carries: "no HANDOVER CANCEL ACKNOWLEDGE (class-1 procedure, Table 8.1-1; 9.2.3.12). TS 38.413 8.4.5 (descriptive; Rel-15 onward). Likely cause: AMF does not answer Handover Cancel."
- **WARN (availability)** `association-lost`: `association lost after the HANDOVER CANCEL`: gNB A's association ended before an acknowledge, and with it the UE's NG connection.
- **CRASH:** the fresh NG Setup after the check does not complete: the core did not survive the HANDOVER CANCEL. Observed is `AMF unresponsive after the message (<what was observed>)`. While a population loads the core the same silence is ERROR.
- **BLOCKED:** the core did not prepare the handover (a HANDOVER PREPARATION FAILURE, whose cause Observed names, no answer, or another one), so there was nothing to cancel and no HANDOVER CANCEL was sent; the result names `NGAP-HANDOVER-COMMAND-TRANSFER`. Also BLOCKED: the core did not complete the UE's registration, its PDU session establishment, or gNB B's NG Setup.
- **ERROR:** the harness could not run the check (a fixed-port `--bind`), a setup step failed for a reason on this host, a message could not be built or sent, the cancel could not be sent or waited for, or the per-test timeout expired during the preparation.
- **SKIP:** no UE configured, or a target release older than Rel-15.
- **Detail:** other PDUs read on gNB A's association while waiting (an Error Indication, for example), whether the AMF released the target, and the result of the fresh NG Setup. It never includes key material.
- **State left behind:** the UE is deregistered (switch-off) from gNB A and both associations are closed. After a lost association or a dead core nothing is signalled for the UE.
