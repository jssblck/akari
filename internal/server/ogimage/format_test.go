package ogimage

import (
	"testing"
)

func TestLevelFor(t *testing.T) {
	// The sqrt ramp: f = sqrt(v/max), stepped at 0.25/0.5/0.75. max = 100 makes the
	// boundaries land on round values (f>0.75 => v>56.25, etc).
	cases := []struct {
		v, max int64
		want   int
	}{
		{0, 100, 0},   // no usage
		{100, 0, 0},   // no ceiling
		{-5, 100, 0},  // guard against negatives
		{1, 100, 1},   // f=0.1 -> level 1
		{6, 100, 1},   // f=0.245 -> level 1
		{7, 100, 2},   // f=0.264 -> level 2
		{26, 100, 3},  // f=0.51 -> level 3
		{57, 100, 4},  // f=0.755 -> level 4
		{100, 100, 4}, // peak
	}
	for _, c := range cases {
		if got := levelFor(c.v, c.max); got != c.want {
			t.Errorf("levelFor(%d, %d) = %d, want %d", c.v, c.max, got, c.want)
		}
	}
}

func TestFmtScale(t *testing.T) {
	cases := map[int64]string{
		0:                 "0",       // raw below 1000
		42:                "42",      // raw
		999:               "999",     // raw, just under the first suffix
		1000:              "1K",      // exact unit, no trailing zeros
		1500:              "1.5K",    // one decimal, trailing zeros trimmed
		1696:              "1.696K",  // three decimals
		12345:             "12.345K", //
		1_999:             "1.999K",  //
		1_000_000:         "1M",      // exact million
		2_000_000:         "2M",      //
		1_050_000:         "1.05M",   // interior zero kept, trailing zero trimmed
		355_300_000:       "355.3M",  //
		1_000_000_000:     "1B",      // exact billion
		12_137_766_601:    "12.137B", // rounds down (drops .766601)
		12_137_999_999:    "12.137B", // still rounds down, never up
		1_000_000_000_000: "1T",      // exact trillion
		1_500_000_000_000: "1.5T",    //
	}
	for in, want := range cases {
		if got := fmtScale(in); got != want {
			t.Errorf("fmtScale(%d) = %q, want %q", in, got, want)
		}
	}
}
