# NAS-REG-REJECTED-NSSAI

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/).

Versions read (issue #139):
- orbit-architect, 2026-10-18: V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.0.0 to V16.4.0 (`24501-g00.zip` to `24501-g40.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`); bisect V16.4.0 to V16.14.0.
- orbit-conformance-test-writer, 2026-10-26: V15.7.0 (`24501-f70.zip`); V16.1.0 to V16.4.0, V16.5.1, V16.6.0 to V16.15.0 (`g10` to `g40`, `g51`, `g60` to `gf0`); V17.0.0 to V17.2.1 and V17.3.0 to V17.18.1 (`h00` to `h21`, `h30` to `hi1`, decimal-point versions h31, h41, h61, h71, ha1, hb1 not read); V18.0.0 to V18.14.0 (`i00` to `ie0`, i01, i21, i31, ib1 not read); V19.0.0, V19.4.0, V19.8.0 (`j00`, `j40`, `j80`); V20.1.0 (`24501-k10.zip`). Each unread version sits between two read versions with identical wording.

## TS 24.501 5.5.1.2.5 #62 (Initial registration not accepted by the network), shall

V16.2.0 (`24501-g20.zip`):

> "If the initial registration request is rejected due to the absence of allowed S-NSSAI(s), the network shall set the 5GMM cause value to #62 "No network slices available"."

V18.14.0 (`24501-ie0.zip`) to V20.1.0 (`24501-k10.zip`):

> "If the initial registration request is rejected because: a) all the S-NSSAI(s) included in the requested NSSAI are rejected; and b) the UE set the NSSAA bit in the 5GMM capability IE to: [...] 2) "Network slice-specific authentication and authorization not supported" and all default S-NSSAIs are either not allowed or are subject to network slice-specific authentication and authorization; [...] the network shall set the 5GMM cause value of the REGISTRATION REJECT message to #62 "No network slices available"."

- **Since:** Rel-16, V16.2.0 (`24501-g20.zip`). No Rel-15 version has a network-side #62 rule (V15.7.0 read in full).
- **Changes:**
  - Rel-16: V16.2.0 triggers on "the absence of allowed S-NSSAI(s)". By V16.5.1 the trigger is the a)/b) structure: all requested S-NSSAIs are rejected, and, depending on the UE's NSSAA bit, there are no subscribed S-NSSAIs marked as default or all of them are not allowed (or are subject to NSSAA). V16.4.0 to V16.5.1 add "and may include the rejected NSSAI". V16.15.0 adds case b) 1) iii) (NSSAA failed or revoked for every default S-NSSAI).
  - Rel-17: V17.18.1 says "default S-NSSAIs" instead of "subscribed S-NSSAIs marked as default", and condition a) becomes "all the S-NSSAI(s) included in the requested NSSAI are rejected".
  - Rel-18: from V18.0.0 the "there are no default S-NSSAIs" conditions (b) 1) i) and b) 2) i)) are void, and b) 2) reads "... not supported" and all default S-NSSAIs are either not allowed or are subject to NSSAA. V18.1.0 adds "of the REGISTRATION REJECT message" to the #62 sentence.
  - Rel-19 (V19.8.0) and Rel-20 (V20.1.0): unchanged from V18.14.0.
- **Use in the check:** the stimulus requests only one S-NSSAI, which the subscriber is not allowed, and the UE sets the NSSAA bit to "not supported" (b) 2), the one branch present in every release). Provenance and Detail name b) 2) as the condition exercised. Whether "all default S-NSSAIs are not allowed" holds depends on the core's configuration, which the check cannot see. An Accept whose allowed NSSAI excludes the stimulus means the core allowed a default S-NSSAI, so b) 2) did not hold: SKIP, with Observed saying a default slice was allowed. A Reject with a cause other than #62: FAIL.

## TS 24.501 5.5.1.2.5 rejected S-NSSAI in REGISTRATION REJECT, shall

V16.6.0 (`24501-g60.zip`) to V17.0.0 (`24501-h00.zip`):

> "If the UE had included requested NSSAI in the REGISTRATION REQUEST message, then the network shall include the rejected S-NSSAI(s) in the Rejected NSSAI IE of the REGISTRATION REJECT message. Otherwise, the network may include the rejected NSSAI."

V18.1.0 (`24501-i10.zip`) to V20.1.0 (`24501-k10.zip`):

> "If the 5GMM cause value is set to #62 "No network slices available", the network shall include, in the rejected NSSAI of the REGISTRATION REJECT message, all the S-NSSAI(s) which were included in the requested NSSAI."
>
> "If the UE has set the ER-NSSAI bit to "Extended rejected NSSAI supported" in the 5GMM capability IE of the REGISTRATION REQUEST message, the rejected S-NSSAI(s) shall be included in the Extended rejected NSSAI IE of the REGISTRATION REJECT message. Otherwise, the rejected S-NSSAI(s) shall be included in the Rejected NSSAI IE of the REGISTRATION REJECT message."

- **Since:** Rel-16, V16.6.0 (`24501-g60.zip`). V16.2.0 to V16.3.0 say nothing on the rejected NSSAI; V16.4.0 to V16.5.1 have only "and may include the rejected NSSAI".
- **Changes:**
  - Rel-16: shall from V16.6.0, unchanged through V16.15.0.
  - Rel-17: from V17.1.0 (`24501-h10.zip`) the IE depends on the UE's ER-NSSAI bit (Extended rejected NSSAI IE when set, Rejected NSSAI IE otherwise). By V17.18.1 the "If the UE had included requested NSSAI in the REGISTRATION REQUEST message" condition is dropped, so the shall applies whenever #62 is sent ("Otherwise, the network may include ..." remains, without a matching "if").
  - Rel-18: from V18.1.0 the rejected NSSAI must list "all the S-NSSAI(s) which were included in the requested NSSAI".
  - Rel-19 (V19.8.0) and Rel-20 (V20.1.0): unchanged from V18.14.0.
- **Use in the check:** the UE includes requested NSSAI with only the stimulus S-NSSAI and does not set the ER-NSSAI bit, so the required IE is the Rejected NSSAI IE in every release from V16.6.0.
  - A #62 Reject whose Rejected NSSAI IE lists the stimulus: PASS. Detail records each rejected S-NSSAI's cause value without asserting it.
  - The stimulus appears only in a decoded Extended rejected NSSAI IE: FAIL (wrong IE), citing the "Otherwise, the rejected S-NSSAI(s) shall be included in the Rejected NSSAI IE" sentence, from V17.1.0.
  - The Rejected NSSAI IE lacks the stimulus and an IE the codec cannot decode is present: ERROR, naming the IEI, with the hex in Detail. The codec covers roughly Rel-15/16, so this may be our gap.
  - Neither IE lists the stimulus: FAIL.
  - A core built on V16.2.0 to V16.5.1 may legally omit the IE, and the release-level Since cannot express that, so the Detail on this FAIL says "required from V16.6.0".
  - From V18.1.0 the Reject must list every requested S-NSSAI. The check requests one, so Detail notes that this requirement and the single-S-NSSAI check coincide.

## TS 24.501 5.5.1.2.4 rejected NSSAI in REGISTRATION ACCEPT, descriptive

V16.15.0 (`24501-gf0.zip`):

> "The AMF may also include rejected NSSAI in the REGISTRATION ACCEPT message."

- **Since:** Rel-15. A "may" in every version read, V15.0.0 through V20.1.0.
- **Changes:** Rel-16 adds "shall not include" restrictions. From Rel-17 the IE depends on the ER-NSSAI bit, and onboarding in SNPN is excluded. Rel-19 adds a Partially rejected NSSAI IE.
- **Use in the check:** informational only. On the Accept (SKIP) path, Detail records the allowed NSSAI and any rejected NSSAI. No verdict depends on it.

## Stimulus and how the Reject is read

Formats checked by orbit-conformance-test-writer, 2026-10-26: V18.14.0 for the layouts, every version from V15.0.0 to V20.1.0 for the change points.

- **Requested NSSAI:** one S-NSSAI only, the run's SST with SD `EEEEEE` (`DDDDDD` when the run's SD is `EEEEEE`).
- **5GMM capability (9.11.3.1):**
  - The UE sends octets 3 to 5, all zero.
  - NSSAA is octet 4 bit 7 (from V16.2.0) and ER-NSSAI is octet 5 bit 5 (from V17.0.0), so both are sent explicitly as "not supported".
  - The spec gives an omitted octet no meaning, so the check does not rely on omission.
- **REGISTRATION REJECT IEs (8.2.9.1):**
  - Rejected NSSAI is IEI 0x69, present from V16.2.0. Its layout (9.11.3.46) is a [length | cause] octet, then the SST and an optional SD.
  - Extended rejected NSSAI is IEI 0x68, present from V17.0.0. Its layout (9.11.3.75) is a series of partial lists:
    - each list starts with a [spare | type of list | number of elements minus one] octet;
    - type 001 adds a back-off timer octet;
    - each element is [length | cause], then the SST, an optional SD and an optional mapped HPLMN S-NSSAI (lengths 1, 2, 4, 5 or 8).
  - Every other IE listed in 8.2.9.1 (V20.1.0) is known and cannot carry an S-NSSAI. The list is: T3346 value (0x5F), T3502 value (0x16), EAP message (0x78), CAG information list (0x75), Disaster return wait range (0x2C), Extended CAG information list (0x71), Lower bound timer value (0x3A), the two Forbidden TAI(s) IEs (0x1D, 0x1E, and 0x3B/0x3C, which the table's NOTE says AMFs compliant with V17.7.0 or V17.8.0 may have used), N3IWF identifier (0x3E), TNAN information (0x4D), Extended 5GMM cause (0x62) and Access technology utilization control (0x63).
  - Any IEI not listed in 8.2.9.1 (V20.1.0), or a slice IE that does not decode, is an IE Orbit cannot decode.
- **Reading the Reject:** it arrives after the Security Mode Command. Its MAC is verified through the UE's NAS security context, and its IEs are read from the plain message under the security header. That works because the engine selects only NEA0 today; #184 removes that dependency.

## Verdicts

A #62 Reject is judged in this order. Both slice IEs are decoded whenever present.
1. The stimulus is in the decoded part of the Rejected NSSAI IE: PASS. Once the stimulus is in a decoded Rejected NSSAI IE the shall is met; other IE errors are recorded in Detail.
2. Otherwise, a Rejected NSSAI or Extended rejected NSSAI IE that fails to decode: ERROR, with the IEI and hex.
3. Otherwise, the stimulus only in a fully decoded Extended rejected NSSAI IE: FAIL.
4. Otherwise, an IEI not listed in 8.2.9.1 (V20.1.0): ERROR.
5. Otherwise: FAIL, the stimulus in neither IE.

In more detail:

- **PASS:** a #62 Reject whose Rejected NSSAI IE lists the stimulus. The cause of each rejected S-NSSAI is recorded, not asserted.
- **FAIL:**
  - a Reject with any other 5GMM cause (5.5.1.2.5 #62, from V16.2.0);
  - the stimulus only in a decoded Extended rejected NSSAI IE (from V17.1.0);
  - a #62 Reject that lists the stimulus in neither IE. The detail says "required from V16.6.0".
- **ERROR:**
  - a #62 Reject without the stimulus in the decoded Rejected NSSAI IE but with an IE Orbit cannot decode, in the order above (IEI and hex in the detail);
  - a Registration Accept that does not decode. No deregistration is possible: the engine skips the undecodable Accept, so the attach never reaches REGISTERED and returns no handle. The detail says so;
  - an Accept whose allowed NSSAI contains the stimulus (a harness collision);
  - an Authentication Reject, a release without a Reject, no answer within 10 s, or a Reject that fails the integrity check.
- **SKIP:**
  - an Accept that allows a default S-NSSAI instead: "core allowed default S-NSSAI(s); the #62 rule did not apply". The allowed NSSAI and the Accept's rejected NSSAI (a may) are recorded, and the UE is deregistered.
  - no UE; a target release before Rel-16.
- From V18.1.0 the Reject must list every requested S-NSSAI. The check requests one, so that rule and this check coincide, and the detail says so.
- A core that then stops completing NG Setup is the crash FAIL.
