package service

import (
	"math"
	"testing"
)

func TestEncodeBase62(t *testing.T) {
	tests := []struct {
		id   int64
		want string
	}{
		{0, "0"},
		{9, "9"},
		{10, "a"},
		{61, "Z"},
		{62, "10"},
		{1_000_000, "4c92"},
		{math.MaxInt64, "aZl8N0y58M7"},
	}

	for _, tt := range tests {
		if got := EncodeBase62(tt.id); got != tt.want {
			t.Errorf("EncodeBase62(%d) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestEncodeBase62IsUnique(t *testing.T) {
	seen := make(map[string]int64)
	for id := range int64(100_000) {
		code := EncodeBase62(id)
		if previous, ok := seen[code]; ok {
			t.Fatalf("EncodeBase62(%d) = %q, same as id %d", id, code, previous)
		}
		seen[code] = id
	}
}
