package provider

import "testing"

func TestSameAddresses(t *testing.T) {
	tests := []struct {
		a, b []string
		want bool
	}{
		{[]string{"10.0.0.4"}, []string{"10.0.0.4/32"}, true},
		{[]string{"10.0.0.8-10.0.0.15"}, []string{"10.0.0.8/29"}, true},
		{[]string{"10.0.0.4", "10.0.0.5"}, []string{"10.0.0.4-10.0.0.5"}, true},
		{[]string{"10.0.0.9/29"}, []string{"10.0.0.8-10.0.0.15"}, true},
		{[]string{"10.0.0.8/29", "10.0.0.1"}, []string{"10.0.0.1", "10.0.0.8/29"}, true},
		{[]string{"255.255.255.254-255.255.255.255", "255.255.255.255"}, []string{"255.255.255.254/31"}, true},
		{[]string{"10.0.0.4"}, []string{"10.0.0.5"}, false},
		{[]string{"10.0.0.8/29"}, []string{"10.0.0.8/28"}, false},
		{[]string{"not-an-address"}, []string{"not-an-address"}, true},
		{[]string{"not-an-address"}, []string{"10.0.0.4"}, false},
		{nil, []string{}, true},
	}
	for _, tt := range tests {
		if got := sameAddresses(tt.a, tt.b); got != tt.want {
			t.Errorf("sameAddresses(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}
