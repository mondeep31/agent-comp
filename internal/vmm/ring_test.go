package vmm

import "testing"

func TestRing(t *testing.T) {
	tests := []struct {
		name   string
		size   int
		writes []string
		want   string
	}{
		{name: "under capacity", size: 8, writes: []string{"abc", "de"}, want: "abcde"},
		{name: "evicts oldest", size: 5, writes: []string{"abcd", "efg"}, want: "cdefg"},
		{name: "single oversized write keeps tail", size: 3, writes: []string{"abcdef"}, want: "def"},
		{name: "exact fill", size: 4, writes: []string{"ab", "cd"}, want: "abcd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRing(tt.size)
			for _, w := range tt.writes {
				n, err := r.Write([]byte(w))
				if err != nil || n != len(w) {
					t.Fatalf("Write(%q) = %d, %v", w, n, err)
				}
			}
			if got := string(r.Bytes()); got != tt.want {
				t.Fatalf("Bytes() = %q, want %q", got, tt.want)
			}
		})
	}
}
