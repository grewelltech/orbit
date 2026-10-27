package target

import "fmt"

// PLMNHint is what to check after an unknown-PLMN NG Setup Failure (misc
// 4, TS 38.413 9.3.1.2), as a suffix for the cause; "" for any other cause.
func PLMNHint(cause, mcc, mnc string, tac uint32) string {
	if cause != "misc(4)" {
		return ""
	}
	return fmt.Sprintf(" (unknown PLMN); core.plmn %s/%s or core.tac %d is not one the AMF serves", mcc, mnc, tac)
}
