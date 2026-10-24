package reconcile

import "testing"

func TestClamp(t *testing.T) {
	tests := []struct {
		name string
		v    int
		lo   int
		hi   int
		want int
	}{
		{"in range", 2, 1, 4, 2},
		{"below lo", 0, 1, 4, 1},
		{"above hi", 5, 1, 4, 4},
		{"at lo", 1, 1, 4, 1},
		{"at hi", 4, 1, 4, 4},
		{"equal bounds", 2, 4, 4, 4},
		{"zero range", 0, 0, 0, 0},
		{"empty range returns hi", 2, 3, 1, 1},
		{"negative below", -5, -2, 0, -2},
		{"negative above", 5, -2, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Clamp(tt.v, tt.lo, tt.hi); got != tt.want {
				t.Errorf("Clamp(%d, %d, %d) = %d, want %d", tt.v, tt.lo, tt.hi, got, tt.want)
			}
		})
	}
}
