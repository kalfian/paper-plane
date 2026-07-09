package model

import "testing"

func TestStatusValid(t *testing.T) {
	tests := []struct {
		status Status
		want   bool
	}{
		{StatusActive, true},
		{StatusUnlinked, true},
		{Status(""), false},
		{Status("deleted"), false},
	}
	for _, tc := range tests {
		if got := tc.status.Valid(); got != tc.want {
			t.Errorf("Status(%q).Valid() = %v, want %v", tc.status, got, tc.want)
		}
	}
}
