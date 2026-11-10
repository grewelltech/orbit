# N3-DL-QFI-CONTAINER

Source: the 3GPP archive only:

- [https://www.3gpp.org/ftp/Specs/archive/29_series/29.281/](https://www.3gpp.org/ftp/Specs/archive/29_series/29.281/)
- [https://www.3gpp.org/ftp/Specs/archive/38_series/38.415/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.415/)

Versions read:

- **TS 29.281:** V15.2.0 (`29281-f20.zip`), V15.5.0 (`29281-f50.zip`), V15.7.0 (`29281-f70.zip`), V16.2.0 (`29281-g20.zip`), V17.5.0 (`29281-h50.zip`), V18.4.0 (`29281-i40.zip`), V19.2.0 (`29281-j20.zip`).
- **TS 38.415:** V15.0.0 (`38415-f00.zip`), V15.3.0 (`38415-f30.zip`), V16.6.0 (`38415-g60.zip`), V17.1.0 (`38415-h10.zip`), V18.2.0 (`38415-i20.zip`), V19.1.0 (`38415-j10.zip`). V15.0.0, V15.3.0 and V16.6.0 are .doc files and were converted with LibreOffice headless. The DL frame figure was read as text cells (field labels), not as a rendered drawing.

## TS 29.281 5.2.2.7 (PDU Session Container), shall

> "This extension header shall be transmitted in: - G-PDUs over the N3 and N9 user plane interfaces, between 5G-AN and UPF, or between two UPFs;" (V19.2.0)

The Rel-15 and Rel-16 text reads: "This extension header shall be transmitted in a G-PDU over the N3 and N9 user plane interfaces, between NG-RAN and UPF, or between two UPFs."

- **Since:** Rel-15, V15.5.0 (`29281-f50.zip`), as "shall". The clause was introduced in V15.2.0 (`29281-f20.zip`) as "may be transmitted"; V15.0.0 and V15.1.0 lack it.
- **Changes:** V15.2.0 to V15.4.0 say "may" (not normative). V15.5.0 through Rel-16 have the single-sentence "shall" form. From Rel-17 V17.3.0 (V17.2.0 still has the sentence form) it is a bulleted list that adds N3mb/N19mb and End Marker bullets, with the same "shall" for N3 and N9 G-PDUs. Rel-18 V18.4.0 is unchanged from Rel-17. From Rel-19 V19.1.0 "between NG-RAN and UPF" reads "between 5G-AN and UPF".
- **Role:** the obligation the check asserts, for the downlink direction, which is the UPF's.
- **Release:** the check treats Rel-15 as applicable, although a core built against V15.2.0 to V15.4.0 was only allowed, not required, to send the container.

## TS 38.415 5.4.1.1 (Transfer of DL PDU Session Information, Successful operation), descriptive

> "The DL PDU SESSION INFORMATION frame includes a QoS Flow Identifier (QFI) field associated with the transferred packet. The NG-RAN shall use the received QFI to determine the QoS flow and QoS profile which are associated with the received packet."

- **Since:** Rel-15, V15.0.0 (`38415-f00.zip`).
- **Changes:** V15.0.0 writes "DL PDU Session Information frame" (mixed case); "DL PDU SESSION INFORMATION" by V15.3.0 (editorial). Otherwise the wording and the clause numbers (5.4.1.1, 5.5.2.1, 5.5.3.3) are unchanged Rel-15 through Rel-19.
- **Strength:** TS 38.415 has no "shall include QFI" sentence. "Includes" is descriptive, and the "shall" in this clause binds the NG-RAN's use of the QFI, not the UPF. The normative "shall" for carrying the container on downlink N3 G-PDUs is TS 29.281 5.2.2.7.

## TS 38.415 5.5.2.1 (DL PDU SESSION INFORMATION (PDU Type 0)), descriptive

- The frame format, Figure 5.5.2.1-1. The QFI is a fixed field of it in every version read (its coding is 5.5.3.3). That is why a G-PDU that carries the container carries a QFI.
- **Changes:** wording and clause number unchanged Rel-15 through Rel-19.

## What the check does

It registers a UE with one PDU session and starts a 1 Mbps UDP flow from the UE to the `--traffic` echo peer on the data network. The peer's echoes are the downlink. The check samples every downlink G-PDU the gNB's N3 demux delivers to the UE's tunnel, until 50 have arrived or 3 s have passed, and reads from each whether it had a PDU Session Container extension header and which QFI it gave.

The check uses the UE's own N3 tunnel, not the `--upf-n3`/`--n3-bind` socket of the other GTP-U check.

## Verdicts

- **PASS:** every sampled downlink G-PDU carries the container.
- **FAIL:** any sampled downlink G-PDU lacks it.
- **ERROR:** nothing was sampled because the echo peer returned nothing, the harness could not run the check (a fixed-port `--bind`, a registration or PDU session that did not complete), or the per-test timeout ended the sampling.
- **SKIP:** no UE configured, no `--traffic` echo peer (the check has no other source of downlink), or a target release older than Rel-15.
- **Detail:** how many G-PDUs were sampled, how many with and without the container, and the QFI values seen (recorded, not asserted). It never includes key material.

Failure message: `downlink G-PDU on N3 without a PDU Session Container. TS 29.281 5.2.2.7 (Rel-15 V15.5.0 onward; V15.2.0-V15.4.0 said "may"). Likely cause: UPF not adding the extension header.`
