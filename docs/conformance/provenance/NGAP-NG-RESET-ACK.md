# NGAP-NG-RESET-ACK

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: V15.0.0 (`38413-f00.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`), V19.0.0, V19.4.0 (`38413-j40.zip`).

## TS 38.413 8.7.4.2.2 (NG Reset initiated by the NG-RAN node), shall

> "After the AMF has released all assigned NG resources and the UE NGAP IDs for all indicated UE associations which can be used for new UE-associated logical NG-connections over the NG interface, the AMF shall respond with the NG RESET ACKNOWLEDGE message."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Changes:** the quoted sentence is word-for-word the same from Rel-15 through Rel-19. Rel-19 (V19.0.0) restructures the surrounding text into "If the NG Reset procedure is executed between the NG-RAN node and the AMF:" bullets and adds AIOTF variants.

## Message clauses (cited here only, not in the check's references)

- NG RESET is TS 38.413 **9.2.6.11** and NG RESET ACKNOWLEDGE is **9.2.6.12**: numbering verified V15.0.0 to V19.4.0; table contents not compared. Earlier citations of 9.2.6.4 and 9.2.6.5 were wrong (those are RAN CONFIGURATION UPDATE and its ACKNOWLEDGE).
- "Reset all" is CHOICE Reset Type > NG interface > Reset All, ENUMERATED (Reset all, ...), in 9.2.6.11. With reset-all, the UE-associated Logical NG-connection List IE is not expected in the ACK; it is O in 9.2.6.12.
