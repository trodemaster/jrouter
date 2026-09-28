package router

import (
	"net"
	"testing"

	"drjosh.dev/jrouter/aurp"
)

func TestIsSelfDI(t *testing.T) {
	local := aurp.IPDomainIdentifier(net.IPv4(192, 168, 234, 23).To4())
	tests := []struct {
		name string
		di   aurp.DomainIdentifier
		want bool
	}{
		{"same IP", aurp.IPDomainIdentifier([]byte{192, 168, 234, 23}), true},
		{"different IP", aurp.IPDomainIdentifier([]byte{71, 212, 59, 212}), false},
		{"null DI", aurp.NullDomainIdentifier{}, false},
	}
	for _, tc := range tests {
		if got := isSelfDI(tc.di, local); got != tc.want {
			t.Errorf("%s: isSelfDI = %v, want %v", tc.name, got, tc.want)
		}
	}
}
