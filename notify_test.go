package main

import (
	"strings"
	"testing"

	"sharkbite/internal/engine"
)

func TestFinishedMessage(t *testing.T) {
	for _, c := range []struct {
		rec         scanRecord
		disguised   bool
		title, body string
	}{
		{scanRecord{Engine: "Microsoft Defender", Clean: true}, false, "Download finished", "found no threats"},
		{scanRecord{Engine: "Microsoft Defender", Threats: []string{"Trojan:X"}}, false, "⚠ Threat found", "Don't open"},
		{scanRecord{Engine: "ClamAV", Clean: true, TooLarge: []string{"big.iso"}}, false, "Download finished", "too large"},
		{scanRecord{Engine: "Microsoft Defender", Clean: true, SFV: &engine.SFVStatus{Bad: []string{"a.r00"}}}, false, "⚠ Download finished with errors", ".sfv"},
		{scanRecord{Engine: "ClamAV", Error: "scanning is turned off in Settings"}, false, "Download finished", "“Movie” is ready."},
		// A clean scan doesn't make a disguised program safe; a threat still comes first.
		{scanRecord{Engine: "Microsoft Defender", Clean: true}, true, "⚠ Check before opening", "disguised program"},
		{scanRecord{Engine: "Microsoft Defender", Threats: []string{"Trojan:X"}}, true, "⚠ Threat found", "Don't open"},
	} {
		title, body := finishedMessage("Movie", &c.rec, c.disguised)
		if title != c.title || !strings.Contains(body, c.body) {
			t.Errorf("%+v: %q, %q", c.rec, title, body)
		}
	}
}
