package aqua

import (
	"bufio"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// testdata/datasource_writes.txt: "<capture>-<n> <hex>" config writes from the 2026-11-28
// aquasuite captures: D5 Next pump Single colour (pump-*), D5 Next RGBpx Single colour
// (strip-*), LEAKSHIELD Colour switch / Bar graph / Colour gradient (lsswitch-*, lsbar-*,
// lsgrad-*).
func loadDS(t *testing.T) map[string][]byte {
	t.Helper()
	f, err := os.Open("testdata/datasource_writes.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m := map[string][]byte{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 4096)
	for sc.Scan() {
		k, h, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("%s: bad hex", k)
		}
		m[k] = b
	}
	return m
}

func dsCRC(b []byte) bool {
	n := len(b)
	return CRC16USB(b[1:n-2]) == uint16(b[n-2])<<8|uint16(b[n-1])
}

// Picking a sensor mode on the RGBpx LED reproduces aquasuite's writes: water temperature
// as the source with its default 20-70 range, plus the brightness or saturation switch.
func TestD5SensorModesReproduceCaptures(t *testing.T) {
	fx := loadDS(t)
	for k, b := range fx {
		if !dsCRC(b) {
			t.Errorf("%s: captured CRC invalid", k)
		}
	}
	for _, c := range []struct{ mode, want string }{
		{"water-bright", "strip-3"},
		{"water-sat", "strip-5"},
	} {
		got := append([]byte(nil), fx["strip-1"]...)
		if err := d5ApplyEffectOn(got, d5Strip, c.mode, false, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		d5FixCRC(got)
		if string(got) != string(fx[c.want]) {
			t.Errorf("RGBpx %s doesn't reproduce %s", c.mode, c.want)
		}
	}
	// Pump: source already water temperature (pump-1); turning brightness on keeps the range.
	// The colour at the time was white (CoolerControl always sends the picked colour).
	got := append([]byte(nil), fx["pump-1"]...)
	if err := d5ApplyEffectOn(got, d5Pump, "water-bright", true, 255, 255, 255); err != nil {
		t.Fatal(err)
	}
	d5FixCRC(got)
	if string(got) != string(fx["pump-2"]) {
		t.Error("pump water-bright doesn't reproduce pump-2")
	}
}

// LEAKSHIELD: choosing pump speed or pressure sets the source and its default range, as
// aquasuite does (Colour switch, Bar graph, Colour gradient).
func TestLeakshieldSensorModesReproduceCaptures(t *testing.T) {
	fx := loadDS(t)
	blk := func(b []byte) []byte { return b[lsOffEffect:lsOffBlockEnd] }
	// Whole-mode path: the captured default effect block plus the source.
	for _, c := range []struct{ from, mode, want string }{
		{"lsswitch-1", "pumpspeed-switch", "lsswitch-2"},
		{"lsbar-1", "pumpspeed-bar", "lsbar-2"},
	} {
		got := append([]byte(nil), fx[c.from]...)
		if err := lsApplyEffect(got, c.mode, false, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		lsFixCRC(got)
		if string(got) != string(fx[c.want]) {
			t.Errorf("%s from %s doesn't reproduce %s", c.mode, c.from, c.want)
		}
	}
	// Source change on a customised block (limits edited): only source + range change.
	for _, c := range []struct{ from, mode, want string }{
		{"lsgrad-8", "pumpspeed-gradient", "lsgrad-9"},
		{"lsswitch-15", "pressure-switch", "lsswitch-16"},
	} {
		b := append([]byte(nil), blk(fx[c.from])...)
		dsApply(b, blk(fx[c.from]), lsDSModes[c.mode])
		if string(b) != string(blk(fx[c.want])) {
			t.Errorf("%s on %s doesn't reproduce %s", c.mode, c.from, c.want)
		}
	}
}

// Re-selecting a mode keeps a custom range for the same source; a new source gets its default.
func TestDataSourceRangeKept(t *testing.T) {
	fx := loadDS(t)
	blk := append([]byte(nil), fx["pump-2"][offEffect:offEffect+d5BlockLen]...)
	if err := dsRangeOn(blk, 25, 45); err != nil {
		t.Fatal(err)
	}
	prev := append([]byte(nil), blk...)
	dsApply(blk, prev, d5PumpDSModes["water-sat"])
	if get16(blk, dsOffFrom) != 25 || get16(blk, dsOffTo) != 45 || get16(blk, dsOffFrom2) != 25 || get16(blk, dsOffTo2) != 45 {
		t.Errorf("custom range lost: %d-%d", get16(blk, dsOffFrom), get16(blk, dsOffTo))
	}
	if blk[dsOffFlags] != dsSaturation {
		t.Errorf("flags 0x%02x, want saturation", blk[dsOffFlags])
	}
	ls := append([]byte(nil), fx["lsswitch-2"][lsOffEffect:lsOffBlockEnd]...) // pump speed 0-2500
	prev = append([]byte(nil), ls...)
	dsApply(ls, prev, lsDSModes["pressure-bright"])
	if get16(ls, dsOffFrom) != 0 || get16(ls, dsOffTo) != 1000 {
		t.Errorf("new source kept the old range: %d-%d", get16(ls, dsOffFrom), get16(ls, dsOffTo))
	}
	none := append([]byte(nil), fx["strip-1"][offStripEffect:offStripEffect+d5BlockLen]...)
	if err := dsRangeOn(none, 10, 20); err == nil {
		t.Error("range accepted on an output without a sensor")
	}
	if err := dsRangeOn(blk, 50, 40); err == nil {
		t.Error("backwards range accepted")
	}
}

// Every sensor mode in the CoolerControl lists resolves, and only Single colour ones take a
// colour.
func TestSensorModeLists(t *testing.T) {
	check := func(name string, list []LightingMode, modes map[string]dsMode) {
		n := 0
		for _, m := range list {
			dm, ok := modes[m.Name]
			if !ok {
				continue
			}
			n++
			if (m.MaxColors > 0) != dm.kind.colour {
				t.Errorf("%s %s: colours %d, colour=%v", name, m.Name, m.MaxColors, dm.kind.colour)
			}
		}
		if n != len(modes) {
			t.Errorf("%s: %d of %d sensor modes listed", name, n, len(modes))
		}
	}
	check("pump", PumpLightingModes(), d5PumpDSModes)
	check("rgbpx", StripLightingModes(), d5RGBpxDSModes)
	check("leakshield", LeakshieldLightingModes(), lsDSModes)
	if _, ok := d5PumpDSModes["water-bar"]; ok {
		t.Error("pump offers Bar graph, which aquasuite doesn't")
	}
}
