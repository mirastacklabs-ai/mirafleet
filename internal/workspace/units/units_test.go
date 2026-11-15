package units

import (
	"testing"
	"time"
)

func TestSize(t *testing.T) {
	for in, want := range map[string]int{"512MiB": 512, "4GiB": 4096, "4 GiB": 4096, "1TiB": 1 << 20, "300": 300} {
		got, err := ParseSizeMB(in)
		if err != nil || got != want {
			t.Errorf("ParseSizeMB(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "-1", "4GB", "1.5GiB", "99999999999", "2000000000GiB"} {
		if _, err := ParseSizeMB(in); err == nil {
			t.Errorf("ParseSizeMB(%q) accepted", in)
		}
	}
	for mb, want := range map[int]string{512: "512MiB", 4096: "4GiB", 1 << 20: "1TiB", 1500: "1500MiB"} {
		if got := FormatSizeMB(mb); got != want {
			t.Errorf("FormatSizeMB(%d) = %q want %q", mb, got, want)
		}
	}
}

func TestCPU(t *testing.T) {
	if m, err := ParseCPUMilli("0.5", 1024); err != nil || m != 500 {
		t.Fatalf("0.5 -> %d %v", m, err)
	}
	for _, in := range []string{"", "0", "NaN", "-1", "0.0001", "2000", "x"} {
		if _, err := ParseCPUMilli(in, 1024); err == nil {
			t.Errorf("ParseCPUMilli(%q) accepted", in)
		}
	}
	if FormatCPUMilli(2000) != "2" || FormatCPUMilli(500) != "0.5" {
		t.Fatal("FormatCPUMilli")
	}
}

func TestDuration(t *testing.T) {
	if d, err := ParseSeconds("24h", 48*time.Hour); err != nil || d != 24*time.Hour {
		t.Fatalf("24h -> %v %v", d, err)
	}
	for _, in := range []string{"", "0s", "1500ms", "49h", "abc", "-5s"} {
		if _, err := ParseSeconds(in, 48*time.Hour); err == nil {
			t.Errorf("ParseSeconds(%q) accepted", in)
		}
	}
	for d, want := range map[time.Duration]string{24 * time.Hour: "24h", 90 * time.Minute: "90m", 2 * time.Minute: "2m", 45 * time.Second: "45s", 90 * time.Second: "90s"} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q want %q", d, got, want)
		}
	}
}
