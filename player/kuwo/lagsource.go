package kuwo

import (
	"time"

	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

// lagReader is the seek-lag source the corrected source reads after the core:
// *pos.LagReader, or a fake in tests. Read returns the #131 seek-lag sample and
// the #135 output state in one pass; the output state is Valid false on a failed
// read and never carries the error.
type lagReader interface {
	Read() (pos.LagSample, pos.OutputState, error)
	Close()
}

// correctedSource filters the open/seek output transient off the raw core
// position (#135) and then deducts the seek lag (#131)
// (doc/design/kuwo-output-transient-design.md §3.2, §3.8;
// doc/design/kuwo-seek-lag-design.md §3.3, §3.4). The filter and the corrector
// both live as long as the core reader. runSession keeps this reader through a
// song switch, the bridge's no-song and settling moments included, and has it
// observe the output on the ticks with no core sample, so the next song's
// opening segment is seen from its start (#198,
// doc/design/kuwo-switch-start-transient-design.md §5.1); it closes the reader
// once the session clears, and a song after that opens a new one, with a fresh
// zero-value filter and corrector.
type correctedSource struct {
	core   positionSource
	lag    lagReader
	filter pos.TransientFilter
	c      pos.LagCorrector
	pollMs int   // for the filter's effective poll interval
	warned *bool // the connection's: its first drift is a warning, later ones are details
}

// newCorrectedSource returns a positionSource whose Read reads the core (a
// failure is returned as it is), then the seek lag and output state (a failure
// is an invalid lag sample and no transient evidence), filters the open/seek
// transient off the core position and returns it corrected. Close closes both
// readers.
func newCorrectedSource(core positionSource, lag lagReader, warned *bool, pollMs int) positionSource {
	return &correctedSource{core: core, lag: lag, warned: warned, pollMs: pollMs}
}

// trackNow stamps an output-only observation (#198). A package var so tests can
// run the filter on their fake's clock.
var trackNow = time.Now

func (s *correctedSource) Read(rid string, playing bool) (pos.Sample, error) {
	sample, err := s.core.Read(rid, playing)
	if err != nil {
		// A failed core read (the core still on the previous song, a torn sample)
		// still observes the output, for the next song's opening (#198 A-2).
		s.observeOutput()
		return sample, err
	}
	// Neither the transient filter nor the correction may fail the session. A
	// failed lag read is an invalid lag sample (lagFailAfter after the last valid
	// one the corrector stops deducting) and no transient evidence (the filter
	// passes the core position through), so behaviour falls back to #131 alone,
	// which with no evidence is the raw core position.
	lag, out, err := s.lag.Read()
	if err != nil {
		lag, out = pos.LagSample{}, pos.OutputState{}
	}
	// Filter the raw core C_raw to P_f before the corrector: PositionMs becomes
	// P_f, CoreMs stays C_raw (the three CoreMs consumers must see the raw core),
	// TransientResync is set on the tick DirectSound starts playing.
	filtered, _ := s.filter.Apply(sample, out, playing, EffectivePollInterval(s.pollMs))
	if filtered.TransientEngaged {
		// One Detail per segment as it first enters filtering (design §3.8): the
		// filter has started changing the public position, so record which device
		// and method engaged and at what base.
		method := "A (DirectSound time anchor)"
		if out.Device == pos.DeviceWaveOut {
			method = "B (WaveOut header counts)"
		}
		log.Detail("Kuwo output transient filter engaged: method %s, base %d ms", method, out.Base)
	}
	// One Detail when a song's opening DirectSound segment, after a song change,
	// passes a multi-report through (#198 A-5): for the real-device acceptance to
	// tell why, and how often, the switch was not filtered.
	if p, ok := s.filter.TakeOpenPass(); ok {
		switch p.Reason {
		case pos.OpenPassCrossingNotAfter:
			log.Detail("Kuwo output transient not filtered at the song start: the Play crossing at %s is not after the previous song's last core sample at %s, base %d ms",
				p.CrossAt.Format("15:04:05.000"), p.LastSongAt.Format("15:04:05.000"), p.Base)
		case pos.OpenPassLongPoll:
			log.Detail("Kuwo output transient not filtered at the song start: the poll interval is too long to anchor, base %d ms", p.Base)
		case pos.OpenPassPassedAbove:
			log.Detail("Kuwo output transient not filtered at the song start: a value above the base already went out uncapped, so the Play crossing at %s is not taken, base %d ms",
				p.CrossAt.Format("15:04:05.000"), p.Base)
		default:
			log.Detail("Kuwo output transient not filtered at the song start: no fresh Play crossing was observed, base %d ms", p.Base)
		}
	}
	corrected, note := s.c.Apply(filtered, lag)
	k := note.Key
	switch note.Kind {
	case pos.LagNoteAdopted:
		log.Detail("Kuwo seek-lag correction adopted: %.1f ms, segment ctx 0x%08X ring 0x%08X base %d ms", float64(note.EUs)/1000, k.Ctx, k.Ring, k.Base)
	case pos.LagNoteDrift:
		if !*s.warned {
			*s.warned = true
			log.Warn("Kuwo seek-lag correction stopped for this song: the lag drifted to %.1f ms, segment ctx 0x%08X ring 0x%08X base %d ms", float64(note.EUs)/1000, k.Ctx, k.Ring, k.Base)
		} else {
			log.Detail("Kuwo seek-lag correction stopped for this song: the lag drifted to %.1f ms, segment ctx 0x%08X ring 0x%08X base %d ms", float64(note.EUs)/1000, k.Ctx, k.Ring, k.Base)
		}
	}
	return corrected, nil
}

// outputReader is a lagReader that reads the output state alone (#198):
// *pos.LagReader, and the fakes of the tests that cover the switch.
type outputReader interface {
	ReadOutput() pos.OutputState
}

// observeOutput reads the output state on a tick with no core sample and gives it
// to the filter's output tracking only (#198 A-2): neither the filter's state
// machine nor the #131 corrector sees the tick, and the seek-lag fields are not
// read. A failed read is an invalid state, which the tracking skips.
func (s *correctedSource) observeOutput() {
	if r, ok := s.lag.(outputReader); ok {
		s.filter.Track(r.ReadOutput(), trackNow())
	}
}

func (s *correctedSource) Close() {
	s.core.Close()
	s.lag.Close()
}

// withLag returns core corrected when openLag succeeds, otherwise core itself:
// the correction stays off and the session goes on. It runs on every core
// open, so a lag reader that could not open (for example in_ffaudio not loaded
// yet) is tried again when the core reader is reopened. pollMs is the player's
// poll interval, for the #135 filter's effective poll interval.
func withLag(core positionSource, openLag func() (lagReader, string, error), warned *bool, pollMs int) positionSource {
	lag, build, err := openLag()
	if err != nil {
		log.Detail("Kuwo seek-lag correction disabled: %v", err)
		return core
	}
	log.Detail("Kuwo seek-lag correction enabled: build %s", build)
	return newCorrectedSource(core, lag, warned, pollMs)
}
