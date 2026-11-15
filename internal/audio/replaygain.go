package audio

import (
	"encoding/binary"
	"io"
	"math"
	"strconv"
	"strings"
)

// ReplayGainMode selects which ReplayGain value is applied.
type ReplayGainMode int32

const (
	ReplayGainOff ReplayGainMode = iota
	ReplayGainTrack
	ReplayGainAlbum
)

// ReplayGain holds ReplayGain values in dB (gain) and linear full scale
// (peak). The Has* fields report which gains are known.
type ReplayGain struct {
	TrackGain, TrackPeak float64
	AlbumGain, AlbumPeak float64
	HasTrack, HasAlbum   bool
}

// Merge fills values missing from rg with those from o.
func (rg ReplayGain) Merge(o ReplayGain) ReplayGain {
	if !rg.HasTrack && o.HasTrack {
		rg.TrackGain, rg.TrackPeak, rg.HasTrack = o.TrackGain, o.TrackPeak, true
	}
	if !rg.HasAlbum && o.HasAlbum {
		rg.AlbumGain, rg.AlbumPeak, rg.HasAlbum = o.AlbumGain, o.AlbumPeak, true
	}
	return rg
}

// Factor returns the linear gain for mode. Album mode falls back to the
// track gain and vice versa; without any gain the factor is 1. The gain is
// reduced when the peak would otherwise clip.
func (rg ReplayGain) Factor(mode ReplayGainMode) float32 {
	var g, peak float64
	switch {
	case mode == ReplayGainOff:
		return 1
	case mode == ReplayGainAlbum && rg.HasAlbum, mode == ReplayGainTrack && !rg.HasTrack && rg.HasAlbum:
		g, peak = rg.AlbumGain, rg.AlbumPeak
	case rg.HasTrack:
		g, peak = rg.TrackGain, rg.TrackPeak
	default:
		return 1
	}
	lin := math.Pow(10, g/20)
	if peak > 0 && lin*peak > 1 {
		lin = 1 / peak
	}
	if math.IsNaN(lin) || math.IsInf(lin, 0) || lin <= 0 {
		return 1
	}
	return float32(lin)
}

// tagged is implemented by decoders that found ReplayGain tags in the file.
type tagged interface{ ReplayGain() ReplayGain }

// setTag applies a single "KEY=value" style tag to rg.
func (rg *ReplayGain) setTag(key, val string) {
	key = strings.ToUpper(strings.TrimSpace(key))
	if !strings.HasPrefix(key, "REPLAYGAIN_") {
		return
	}
	v, ok := parseRGValue(val)
	if !ok {
		return
	}
	switch key {
	case "REPLAYGAIN_TRACK_GAIN":
		rg.TrackGain, rg.HasTrack = v, true
	case "REPLAYGAIN_TRACK_PEAK":
		rg.TrackPeak = v
	case "REPLAYGAIN_ALBUM_GAIN":
		rg.AlbumGain, rg.HasAlbum = v, true
	case "REPLAYGAIN_ALBUM_PEAK":
		rg.AlbumPeak = v
	}
}

// parseRGValue parses values like "-6.48 dB", "+1.2dB" or "0.988".
func parseRGValue(s string) (float64, bool) {
	s = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(s), "\x00"))
	end := 0
	for end < len(s) && strings.IndexByte("+-0123456789.,eE", s[end]) >= 0 {
		end++
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s[:end], ",", "."), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 100 {
		return 0, false
	}
	return v, true
}

func parseVorbisComments(comments []string) ReplayGain {
	var rg ReplayGain
	for _, c := range comments {
		if k, v, ok := strings.Cut(c, "="); ok {
			rg.setTag(k, v)
		}
	}
	return rg
}

// maxTagBlock bounds the size of metadata read into memory.
const maxTagBlock = 1 << 20

// readFLACReplayGain reads the VORBIS_COMMENT block of a FLAC stream that
// starts at offset start. The read position is left undefined.
func readFLACReplayGain(r io.ReadSeeker, start int64) ReplayGain {
	if _, err := r.Seek(start, io.SeekStart); err != nil {
		return ReplayGain{}
	}
	var sig [4]byte
	if _, err := io.ReadFull(r, sig[:]); err != nil || string(sig[:]) != "fLaC" {
		return ReplayGain{}
	}
	for i := 0; i < 128; i++ {
		var h [4]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return ReplayGain{}
		}
		last, typ := h[0]&0x80 != 0, h[0]&0x7f
		size := int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3])
		if typ == 4 {
			if size > maxTagBlock {
				return ReplayGain{}
			}
			b := make([]byte, size)
			if _, err := io.ReadFull(r, b); err != nil {
				return ReplayGain{}
			}
			return parseVorbisComments(vorbisCommentList(b))
		}
		if last {
			break
		}
		if _, err := r.Seek(size, io.SeekCurrent); err != nil {
			return ReplayGain{}
		}
	}
	return ReplayGain{}
}

// vorbisCommentList decodes a little-endian Vorbis comment structure.
func vorbisCommentList(b []byte) []string {
	next := func() (string, bool) {
		if len(b) < 4 {
			return "", false
		}
		n := binary.LittleEndian.Uint32(b)
		b = b[4:]
		if uint64(n) > uint64(len(b)) {
			return "", false
		}
		s := string(b[:n])
		b = b[n:]
		return s, true
	}
	if _, ok := next(); !ok { // vendor
		return nil
	}
	if len(b) < 4 {
		return nil
	}
	count := binary.LittleEndian.Uint32(b)
	b = b[4:]
	var out []string
	for i := uint32(0); i < count; i++ {
		s, ok := next()
		if !ok {
			break
		}
		out = append(out, s)
	}
	return out
}

// readID3ReplayGain reads REPLAYGAIN_* TXXX frames from an ID3v2 tag at the
// start of r. The read position is left undefined.
func readID3ReplayGain(r io.ReadSeeker) ReplayGain {
	var rg ReplayGain
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return rg
	}
	var h [10]byte
	if _, err := io.ReadFull(r, h[:]); err != nil || string(h[:3]) != "ID3" {
		return rg
	}
	ver, flags := h[3], h[5]
	if ver < 2 || ver > 4 || (flags&0x80 != 0 && ver < 4) {
		return rg // unsupported version or tag-wide unsynchronisation
	}
	end := 10 + syncsafe(h[6:10])
	pos := int64(10)
	if ver >= 3 && flags&0x40 != 0 { // extended header
		var eh [4]byte
		if _, err := io.ReadFull(r, eh[:]); err != nil {
			return rg
		}
		n := int64(binary.BigEndian.Uint32(eh[:])) + 4
		if ver == 4 {
			n = syncsafe(eh[:])
		}
		pos += n
	}
	idLen, hdrLen := 4, 10
	if ver == 2 {
		idLen, hdrLen = 3, 6
	}
	fh := make([]byte, hdrLen)
	for pos+int64(hdrLen) <= end {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return rg
		}
		if _, err := io.ReadFull(r, fh); err != nil {
			return rg
		}
		if fh[0] == 0 {
			break // padding
		}
		id := string(fh[:idLen])
		var size int64
		var fflags uint16
		switch ver {
		case 2:
			size = int64(fh[3])<<16 | int64(fh[4])<<8 | int64(fh[5])
		case 3:
			size = int64(binary.BigEndian.Uint32(fh[4:8]))
			fflags = binary.BigEndian.Uint16(fh[8:10])
		case 4:
			size = syncsafe(fh[4:8])
			fflags = binary.BigEndian.Uint16(fh[8:10])
		}
		pos += int64(hdrLen)
		if size <= 0 || pos+size > end {
			break
		}
		// Skip compressed, encrypted or (v2.4) unsynchronised frames.
		skip := (ver == 3 && fflags&0x00C0 != 0) || (ver == 4 && fflags&0x000E != 0)
		if (id == "TXXX" || id == "TXX") && !skip && size <= maxTagBlock {
			b := make([]byte, size)
			if _, err := io.ReadFull(r, b); err != nil {
				return rg
			}
			if ver == 4 && fflags&0x0001 != 0 && len(b) >= 4 { // data length indicator
				b = b[4:]
			}
			if desc, val, ok := splitTXXX(b); ok {
				rg.setTag(desc, val)
			}
		}
		pos += size
	}
	return rg
}

func syncsafe(b []byte) int64 {
	return int64(b[0]&0x7f)<<21 | int64(b[1]&0x7f)<<14 | int64(b[2]&0x7f)<<7 | int64(b[3]&0x7f)
}

// splitTXXX splits a TXXX frame body into description and value.
func splitTXXX(b []byte) (string, string, bool) {
	if len(b) < 2 {
		return "", "", false
	}
	enc, b := b[0], b[1:]
	if enc == 1 || enc == 2 {
		// UTF-16: find the double-NUL terminator on an even boundary.
		for i := 0; i+1 < len(b); i += 2 {
			if b[i] == 0 && b[i+1] == 0 {
				return utf16ASCII(b[:i]), utf16ASCII(b[i+2:]), true
			}
		}
		return "", "", false
	}
	i := strings.IndexByte(string(b), 0)
	if i < 0 {
		return "", "", false
	}
	return string(b[:i]), string(b[i+1:]), true
}

// utf16ASCII decodes the ASCII subset of UTF-16 (with or without BOM), which
// is all ReplayGain keys and values use.
func utf16ASCII(b []byte) string {
	le := false
	if len(b) >= 2 {
		switch {
		case b[0] == 0xFF && b[1] == 0xFE:
			le, b = true, b[2:]
		case b[0] == 0xFE && b[1] == 0xFF:
			b = b[2:]
		}
	}
	var sb strings.Builder
	for i := 0; i+1 < len(b); i += 2 {
		u := binary.BigEndian.Uint16(b[i:])
		if le {
			u = binary.LittleEndian.Uint16(b[i:])
		}
		if u == 0 {
			break
		}
		if u < 0x80 {
			sb.WriteByte(byte(u))
		}
	}
	return sb.String()
}
