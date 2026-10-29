package kuwo

import "Metabox-Nexus-PlayerCap/player/kuwo/pos"

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
// both live as long as the core reader: runSession closes the reader when there
// is no song and opens a new one, with a fresh zero-value filter and corrector,
// when a song plays again.
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

func (s *correctedSource) Read(rid string, playing bool) (pos.Sample, error) {
	sample, err := s.core.Read(rid, playing)
	if err != nil {
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
