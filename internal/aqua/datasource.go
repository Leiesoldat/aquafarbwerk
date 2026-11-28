package aqua

// Data-source ("sensor-controlled") lighting on the D5 Next pump ring, the D5 Next RGBpx LED
// output and the LEAKSHIELD, decoded 2026-11-28 from six aquasuite captures
// (testdata/datasource_writes.txt, re/PROTOCOL.md). All three effect blocks share one layout;
// offsets below are relative to the block start (pump 663, RGBpx 243, LEAKSHIELD 577):
//
//	+1      control flags: 0x40 = brightness (Bar graph: rotation) follows the source,
//	        0x80 = saturation follows the source
//	+3..4   data source ID, u16 BE (0xffff = none)
//	+7..10  range from / to, u16 BE, in the source's own units (°C, rpm, ...)
//	+13..16 second copy of the range (aquasuite always writes both)
//
// aquasuite sets the source's default range whenever a source is chosen. These devices get
// no software sensor feed in any capture, so only their own sensors are offered.

import "fmt"

const (
	dsOffFlags  = 1
	dsOffSource = 3
	dsOffFrom   = 7
	dsOffTo     = 9
	dsOffFrom2  = 13
	dsOffTo2    = 15

	dsBrightness = 0x40
	dsSaturation = 0x80
	dsNoSource   = 0xffff
)

// DataSource is a sensor an effect can follow.
type DataSource struct {
	Name     string // CLI / mode name
	Label    string // CoolerControl label
	ID       uint16 // block +3..4
	From, To uint16 // aquasuite's default range, in Unit
	Unit     string
}

// D5NextSources are the D5 Next's own sensors usable as a data source (pump ring and RGBpx).
var D5NextSources = []DataSource{
	{"water", "Water Temp", 0x0000, 20, 70, "°C"},
}

// LeakshieldSources are the LEAKSHIELD's own sensors usable as a data source.
var LeakshieldSources = []DataSource{
	{"pumpspeed", "Pump Speed", 0x0004, 0, 2500, "rpm"},
	{"pressure", "Pressure", 0x0000, 0, 1000, "raw"},
}

// dsKind is how a data-source mode uses its source.
type dsKind struct {
	suffix, label string
	effect        string // captured effect block it starts from
	flags         byte   // +1
	colour        bool   // takes the CoolerControl colour (Single colour slot 0)
}

var (
	dsBright = dsKind{"bright", "Brightness", "static", dsBrightness, true}
	dsSat    = dsKind{"sat", "Saturation", "static", dsSaturation, true}
	dsBar    = dsKind{"bar", "Bar Graph", "bargraph", 0, false}
	dsSwitch = dsKind{"switch", "Colour Switch", "colorswitch", 0, false}
	dsGrad   = dsKind{"gradient", "Colour Gradient", "colorgradient", 0, false}
)

// dsMode is one CoolerControl mode: a source and a way of using it.
type dsMode struct {
	src  DataSource
	kind dsKind
}

func dsModes(srcs []DataSource, kinds ...dsKind) map[string]dsMode {
	m := map[string]dsMode{}
	for _, s := range srcs {
		for _, k := range kinds {
			m[s.Name+"-"+k.suffix] = dsMode{s, k}
		}
	}
	return m
}

func dsLightingModes(srcs []DataSource, kinds ...dsKind) []LightingMode {
	var ms []LightingMode
	for _, s := range srcs {
		for _, k := range kinds {
			var n uint32
			if k.colour {
				n = 1
			}
			ms = append(ms, LightingMode{s.Name + "-" + k.suffix, s.Label + ": " + k.label, n, n})
		}
	}
	return ms
}

// Which data-source modes each output offers. The D5 Next pump only has Single colour with a
// source in aquasuite; the RGBpx LED adds Bar graph; the LEAKSHIELD has all of them.
var (
	d5PumpDSModes  = dsModes(D5NextSources, dsBright, dsSat)
	d5RGBpxDSModes = dsModes(D5NextSources, dsBright, dsSat, dsBar)
	lsDSModes      = dsModes(LeakshieldSources, dsBright, dsSat, dsGrad, dsSwitch, dsBar)
)

func get16(b []byte, i int) uint16    { return uint16(b[i])<<8 | uint16(b[i+1]) }
func put16(b []byte, i int, v uint16) { b[i], b[i+1] = byte(v>>8), byte(v) }

// dsSetRange writes a range into both copies.
func dsSetRange(blk []byte, from, to uint16) {
	put16(blk, dsOffFrom, from)
	put16(blk, dsOffTo, to)
	put16(blk, dsOffFrom2, from)
	put16(blk, dsOffTo2, to)
}

// dsApply turns a freshly written effect block (blk, 67 bytes) into mode m. prev is the block
// as it was before: if it already followed the same source, its range is kept (so a range set
// with the CLI survives re-selecting the mode); otherwise the source's default range is used,
// as aquasuite does.
func dsApply(blk, prev []byte, m dsMode) {
	blk[dsOffFlags] = m.kind.flags
	put16(blk, dsOffSource, m.src.ID)
	if prev != nil && get16(prev, dsOffSource) == m.src.ID {
		if from, to := get16(prev, dsOffFrom), get16(prev, dsOffTo); to > from {
			dsSetRange(blk, from, to)
			return
		}
	}
	dsSetRange(blk, m.src.From, m.src.To)
}

// DataSourceState describes a block's data-source settings.
type DataSourceState struct {
	Source     string // source name, "none" or "unknown 0x...."
	Brightness bool
	Saturation bool
	From, To   uint16
	Unit       string
}

func dsRead(blk []byte, srcs []DataSource) DataSourceState {
	st := DataSourceState{
		Brightness: blk[dsOffFlags]&dsBrightness != 0,
		Saturation: blk[dsOffFlags]&dsSaturation != 0,
		From:       get16(blk, dsOffFrom),
		To:         get16(blk, dsOffTo),
	}
	id := get16(blk, dsOffSource)
	switch {
	case id == dsNoSource:
		st.Source = "none"
	default:
		st.Source = fmt.Sprintf("unknown 0x%04x", id)
		for _, s := range srcs {
			if s.ID == id {
				st.Source, st.Unit = s.Name, s.Unit
			}
		}
	}
	return st
}

// dsRangeOn sets the range of the block at blk if it follows a source.
func dsRangeOn(blk []byte, from, to float64) error {
	if get16(blk, dsOffSource) == dsNoSource {
		return fmt.Errorf("this output doesn't follow a sensor; pick a sensor mode first")
	}
	if from < 0 || to > 65535 || to <= from {
		return fmt.Errorf("bad range %g-%g (need 0 <= from < to <= 65535)", from, to)
	}
	dsSetRange(blk, uint16(from), uint16(to))
	return nil
}
