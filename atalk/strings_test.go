package atalk

import "testing"

func TestChecksum(t *testing.T) {
	tests := []struct {
		in   string
		want uint16
	}{
		{in: "", want: 0xFFFF},
		{in: "Maclab House", want: 0xEC38},
		{in: "jrouter", want: 0x6CC8},
		{in: "TimeZone", want: 0xBBFA},
		{in: "ASDFASDFASDF", want: 0xD08C},
	}
	for _, test := range tests {
		if got, want := Checksum(test.in), test.want; got != want {
			t.Errorf("Checksum(%q) = 0x%X, want 0x%X", test.in, got, want)
		}
	}
}
