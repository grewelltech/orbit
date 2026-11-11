# NGAP-RAN-CONFIG-UPDATE-ACK

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: V15.0.0 (`38413-f00.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`) and V19.4.0 (`38413-j40.zip`), the latest per release.

## TS 38.413 8.7.2.2 (RAN Configuration Update, Successful Operation), descriptive

> "The NG-RAN node initiates the procedure by sending a RAN CONFIGURATION UPDATE message to the AMF including an appropriate set of updated configuration data that it has just taken into operational use. The AMF responds with a RAN CONFIGURATION UPDATE ACKNOWLEDGE message to acknowledge that it successfully updated the configuration data."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Strength:** descriptive. The text says "responds", not "shall respond": the ACKNOWLEDGE is described, not required.
- **Changes:** the two sentences are unchanged from V15.0.0 to V18.11.0. In V15.13.0 a third sentence follows them in the same paragraph ("If an information element is not included in the RAN CONFIGURATION UPDATE message, the AMF shall interpret that the corresponding configuration data is not changed ..."); it is absent in V15.0.0 and is the subject of `NGAP-RAN-CONFIG-UPDATE-KEEPS-TA`, not of this check. In V19.4.0 the same words are a bullet under "If the RAN Configuration Update procedure is executed between the NG-RAN node and the AMF".

## Why there is nothing to grade (cited here only, not in the check's references)

- **TS 38.413 3.1 (Definitions), descriptive:** "Class 1: Elementary Procedures with response (success and/or failure)." Among the unsuccessful outcomes of a Class 1 procedure the same clause lists "On time supervision expiry (i.e., absence of expected response)". Read in all six versions.
- **TS 38.413 8.1, Table 8.1-1 (Class 1 procedures), descriptive:** the row "RAN Configuration Update | RAN CONFIGURATION UPDATE | RAN CONFIGURATION UPDATE ACKNOWLEDGE | RAN CONFIGURATION UPDATE FAILURE", the same in all six versions. The table names the procedure's responses; it does not oblige the AMF to send one.
- **TS 38.413 8.7.2.3 (RAN Configuration Update, Unsuccessful Operation), shall:** "If the AMF cannot accept the update, it shall respond with a RAN CONFIGURATION UPDATE FAILURE message and appropriate cause value." The sentence is in all six versions. It binds only an AMF that cannot accept the update. Whether an AMF that sent nothing accepted the update or could not accept it cannot be observed on N2, so a missing answer cannot be scored against this shall, and a FAILURE satisfies it. The check therefore does not cite it: an informational check cites no requirement, and this one grades none.

## Verdict

- **Verdict class:** INFO (informational). The check reports and never gates: it gives no PASS, FAIL or DEVIATION, and it cannot be put on an expected-failures list.
- **Stimulus:** on an association that has completed NG Setup, a RAN CONFIGURATION UPDATE (stream 0) whose only IE is the Supported TA List the NG Setup presented: the same TAC, PLMN and slices, so the update changes nothing and every value in it is one the AMF has already accepted.
- **INFO:** a RAN CONFIGURATION UPDATE ACKNOWLEDGE within 5 s. Observed: `RAN CONFIGURATION UPDATE ACKNOWLEDGE`.
- **INFO-DIFF:** anything else. Observed says which:
  - `RAN CONFIGURATION UPDATE FAILURE` (the cause is in the detail);
  - `ERROR INDICATION` (the cause is in the detail);
  - `no answer; association survived`: neither outcome within 5 s, the absence inferred within that bound;
  - `association reset`: the association was lost before an answer.

  The detail carries: "no RAN CONFIGURATION UPDATE ACKNOWLEDGE (class-1 procedure, Table 8.1-1). TS 38.413 8.7.2.2 (descriptive; Rel-15 onward). Likely cause: AMF does not implement RAN Configuration Update."
- **After an INFO-DIFF** a fresh NG Setup is tried and the detail records its result. When it does not complete, Observed ends with `; a fresh NG Setup afterwards did not complete`. The verdict stays INFO-DIFF: an informational check never fails a core, and a core that stopped answering fails the graded checks that run after this one.
- **ERROR:** the stimulus could not be built or sent, or NG Setup did not complete, so nothing was observed. An ERROR gates as it does for every check.
- **SKIP:** category not selected.
- PDUs of other procedures read while waiting are passed over; the detail counts them.
