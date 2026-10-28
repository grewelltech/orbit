package plan

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/bgrewell/orbit/internal/target"
)

// TargetFromProto inverts TargetProto, field for field, keys included: a
// target with every field set survives the wire both ways.
func TestTargetFromProto(t *testing.T) {
	tg := target.Target{Version: target.Version, Kind: target.Kind, Name: "lab1",
		Core: target.Core{AMF: "10.0.0.1:38412", PLMN: target.PLMN{MCC: "001", MNC: "01"}, TAC: 7,
			Slices: []target.Slice{{SST: 1, SD: "010203"}, {SST: 2}}, DNN: "ims", Profile: "sd-core", UPFN3: "192.0.2.30:2152",
			UEPools: []string{"10.250.0.0/16"}, UPFN6: "198.51.100.1", N6Subnet: "198.51.100.0/24"},
		Subscribers: []target.Pool{{Name: "main", FirstSUPI: "001010000000001", Count: 3, Ki: "k", OPc: "o", RoutingIndicator: "0001"},
			{Name: "second", FirstSUPI: "001010000000100"}},
		RAN: target.RAN{Node: "n", GNBName: "g", N2SourceIPs: []string{"192.0.2.20"}, N3IPs: []string{"192.0.2.21"},
			GNBIDs: target.GNBIDs{First: 100, Count: 4, Bits: 24}},
		App: target.App{Responders: []target.Responder{{Name: "n6", Control: "203.0.113.20:9551", Token: "tok", DataIP: "203.0.113.20"}}}}
	// Every field of the struct is set, so a field TargetProto carries and
	// TargetFromProto drops (or the reverse) fails here.
	var zero func(v reflect.Value, path string)
	zero = func(v reflect.Value, path string) {
		switch v.Kind() {
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				zero(v.Field(i), path+"."+v.Type().Field(i).Name)
			}
		case reflect.Slice:
			if v.Len() == 0 {
				t.Errorf("%s is empty in the test target", path)
			}
			for i := 0; i < v.Len(); i++ {
				if v.Index(i).Kind() == reflect.Struct && i == 0 {
					zero(v.Index(i), path+"[0]")
				}
			}
		default:
			if v.IsZero() {
				t.Errorf("%s is zero in the test target", path)
			}
		}
	}
	zero(reflect.ValueOf(tg), "target")
	got := TargetFromProto(TargetProto(tg))
	if !reflect.DeepEqual(got, tg) {
		t.Errorf("TargetFromProto(TargetProto(t)):\n got %+v\nwant %+v", got, tg)
	}
	p := TargetProto(tg)
	if again := TargetProto(TargetFromProto(p)); !proto.Equal(again, p) {
		t.Errorf("TargetProto(TargetFromProto(p)):\n got %v\nwant %v", again, p)
	}
}
