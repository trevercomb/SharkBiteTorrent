package main

import (
	"strings"
	"testing"

	"sharkbite/internal/engine"
)

func TestFinishedMessage(t *testing.T) {
	for _, c := range []struct {
		rec         scanRecord
		title, body string
	}{
		{scanRecord{Engine: "Microsoft Defender", Clean: true}, "Download finished", "found no threats"},
		{scanRecord{Engine: "Microsoft Defender", Threats: []string{"Trojan:X"}}, "⚠ Threat found", "Don't open"},
		{scanRecord{Engine: "ClamAV", Clean: true, TooLarge: []string{"big.iso"}}, "Download finished", "too large"},
		{scanRecord{Engine: "Microsoft Defender", Clean: true, SFV: &engine.SFVStatus{Bad: []string{"a.r00"}}}, "⚠ Download finished with errors", ".sfv"},
		{scanRecord{Engine: "ClamAV", Error: "scanning is turned off in Settings"}, "Download finished", "“Movie” is ready."},
	} {
		title, body := finishedMessage("Movie", &c.rec)
		if title != c.title || !strings.Contains(body, c.body) {
			t.Errorf("%+v: %q, %q", c.rec, title, body)
		}
	}
}
