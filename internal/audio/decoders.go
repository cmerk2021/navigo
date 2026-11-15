package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/hajimehoshi/go-mp3"
	"github.com/jfreymuth/oggvorbis"
	"github.com/mewkiz/flac"
)

// Decoder produces interleaved stereo float32 frames at its native rate.
type Decoder interface {
	// Read decodes up to len(dst)/2 frames into dst and returns the number of
	// frames written. It returns io.EOF at the end of the stream.
	Read(dst []float32) (int, error)
	SampleRate() int
	// Seek positions the decoder at the given frame.
	SeekFrame(frame int64) error
	// Length returns the total number of frames, or -1 if unknown.
	Length() int64
	Close() error
}

// Source is the byte source of a track, typically a growing cache file.
type Source interface {
	io.ReadSeekCloser
	// Complete reports whether all data is available.
	Complete() bool
	// WaitComplete blocks until all data is available.
	WaitComplete(ctx context.Context) error
	// Interrupt aborts blocked and future reads.
	Interrupt()
}

// interrupter is implemented by decoders that can abort blocking reads.
type interrupter interface{ Interrupt() }

// ErrUnsupported is returned for formats without a decoder.
var ErrUnsupported = errors.New("unsupported audio format")

// Open detects the container format and returns a decoder. hint is the file
// suffix reported by the server and is used only as a fallback.
func Open(src Source, hint string) (Decoder, error) {
	var hdr [12]byte
	n, err := io.ReadFull(src, hdr[:])
	if err != nil && n < 4 {
		return nil, fmt.Errorf("read header: %w", err)
	}
	h := hdr[:n]
	format := strings.ToLower(hint)
	id3Len := int64(0)
	switch {
	case string(h[:4]) == "fLaC":
		format = "flac"
	case string(h[:4]) == "OggS":
		format = "ogg"
	case n >= 12 && string(h[:4]) == "RIFF" && string(h[8:12]) == "WAVE":
		format = "wav"
	case string(h[:3]) == "ID3" && n >= 10:
		id3Len = 10 + int64(h[6]&0x7f)<<21 | int64(h[7]&0x7f)<<14 | int64(h[8]&0x7f)<<7 | int64(h[9]&0x7f)
		if h[5]&0x10 != 0 {
			id3Len += 10
		}
		var sig [4]byte
		if _, err := src.Seek(id3Len, io.SeekStart); err == nil {
			if _, err := io.ReadFull(src, sig[:]); err == nil && string(sig[:]) == "fLaC" {
				format = "flac"
			} else {
				format = "mp3"
			}
		}
	case h[0] == 0xFF && h[1]&0xE0 == 0xE0:
		format = "mp3"
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	switch format {
	case "flac":
		return newFLAC(src)
	case "ogg", "oga":
		return newVorbis(src)
	case "wav", "wave":
		return newWAV(src)
	case "mp3":
		return newMP3(src)
	}
	return nil, fmt.Errorf("%w: %s", ErrUnsupported, hint)
}

// NativeFormat reports whether a file with this suffix can be decoded
// without server-side transcoding.
func NativeFormat(suffix string) bool {
	switch strings.ToLower(suffix) {
	case "flac", "mp3", "ogg", "oga", "wav", "wave":
		return true
	}
	return false
}

// readOnly hides Seek from libraries that would otherwise scan the whole file.
type readOnly struct{ r io.Reader }

func (r readOnly) Read(p []byte) (int, error) { return r.r.Read(p) }

// ---- FLAC ----

type flacDec struct {
	src    Source
	s      *flac.Stream
	pend   []float32 // decoded but unread stereo samples
	scale  float32
	rate   int
	length int64
	pos    int64 // frames decoded
	rg     ReplayGain
}

func newFLAC(src Source) (Decoder, error) {
	var rg ReplayGain
	if start, err := skipID3(src); err == nil {
		rg = readFLACReplayGain(src, start)
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	s, err := flac.NewSeek(src)
	if err != nil {
		return nil, fmt.Errorf("flac: %w", err)
	}
	bps := int(s.Info.BitsPerSample)
	if bps == 0 {
		bps = 16
	}
	length := int64(s.Info.NSamples)
	if length == 0 {
		length = -1
	}
	return &flacDec{src: src, s: s, scale: 1 / float32(int64(1)<<(bps-1)), rate: int(s.Info.SampleRate), length: length, rg: rg}, nil
}

func (d *flacDec) ReplayGain() ReplayGain { return d.rg }
func (d *flacDec) SampleRate() int        { return d.rate }
func (d *flacDec) Length() int64          { return d.length }
func (d *flacDec) Interrupt()             { d.src.Interrupt() }
func (d *flacDec) Close() error           { return d.src.Close() }

func (d *flacDec) Read(dst []float32) (int, error) {
	for len(d.pend) == 0 {
		f, err := d.s.ParseNext()
		if err != nil {
			if err != io.EOF && d.length > 0 && d.pos >= d.length {
				// All samples were decoded; what follows (an ID3v1 tag,
				// padding) isn't audio.
				return 0, io.EOF
			}
			return 0, err
		}
		if len(f.Subframes) == 0 {
			continue
		}
		ns := len(f.Subframes[0].Samples)
		if cap(d.pend) < ns*2 {
			d.pend = make([]float32, 0, ns*2)
		}
		d.pend = d.pend[:ns*2]
		l := f.Subframes[0].Samples
		r := l
		if len(f.Subframes) > 1 {
			r = f.Subframes[1].Samples
		}
		for i := 0; i < ns; i++ {
			d.pend[2*i] = float32(l[i]) * d.scale
			d.pend[2*i+1] = float32(r[i]) * d.scale
		}
		d.pos += int64(ns)
	}
	n := copy(dst, d.pend)
	n &^= 1
	d.pend = d.pend[n:]
	return n / 2, nil
}

func (d *flacDec) SeekFrame(frame int64) error {
	d.pend = d.pend[:0]
	start, err := d.s.Seek(uint64(frame))
	if err != nil {
		return err
	}
	d.pos = int64(start)
	skip := frame - int64(start)
	buf := make([]float32, 4096)
	for skip > 0 {
		k := int64(len(buf) / 2)
		if skip < k {
			k = skip
		}
		n, err := d.Read(buf[:k*2])
		if err != nil {
			return err
		}
		skip -= int64(n)
	}
	return nil
}

// ---- Ogg Vorbis ----

type vorbisDec struct {
	src      Source
	r        *oggvorbis.Reader
	seekable bool
	ch       int
	buf      []float32
	rg       ReplayGain
}

func newVorbis(src Source) (Decoder, error) {
	d := &vorbisDec{src: src}
	if err := d.open(src.Complete()); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *vorbisDec) open(seekable bool) error {
	if _, err := d.src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var in io.Reader = readOnly{d.src}
	if seekable {
		in = d.src
	}
	r, err := oggvorbis.NewReader(in)
	if err != nil {
		return fmt.Errorf("ogg: %w", err)
	}
	d.r, d.seekable, d.ch = r, seekable, r.Channels()
	d.rg = parseVorbisComments(r.CommentHeader().Comments)
	return nil
}

func (d *vorbisDec) ReplayGain() ReplayGain { return d.rg }
func (d *vorbisDec) SampleRate() int        { return d.r.SampleRate() }
func (d *vorbisDec) Interrupt()             { d.src.Interrupt() }
func (d *vorbisDec) Close() error           { return d.src.Close() }

func (d *vorbisDec) Length() int64 {
	if d.seekable && d.r.Length() > 0 {
		return d.r.Length()
	}
	return -1
}

func (d *vorbisDec) Read(dst []float32) (int, error) {
	frames := len(dst) / 2
	if d.ch == 2 {
		n, err := d.r.Read(dst[:frames*2])
		return n / 2, err
	}
	need := frames * d.ch
	if cap(d.buf) < need {
		d.buf = make([]float32, need)
	}
	n, err := d.r.Read(d.buf[:need])
	nf := n / d.ch
	for i := 0; i < nf; i++ {
		l := d.buf[i*d.ch]
		r := l
		if d.ch > 1 {
			r = d.buf[i*d.ch+1]
		}
		dst[2*i], dst[2*i+1] = l, r
	}
	return nf, err
}

func (d *vorbisDec) SeekFrame(frame int64) error {
	if !d.seekable {
		// Bisection seeking needs the whole file; wait for the download.
		if err := d.src.WaitComplete(context.Background()); err != nil {
			return err
		}
		if err := d.open(true); err != nil {
			return err
		}
	}
	return d.r.SetPosition(frame)
}

// ---- WAV ----

type wavDec struct {
	src        Source
	rate       int
	ch         int
	bits       int
	float      bool
	dataStart  int64
	dataLen    int64
	blockAlign int
	pos        int64 // frames
	raw        []byte
}

func newWAV(src Source) (Decoder, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(src, hdr[:]); err != nil {
		return nil, err
	}
	d := &wavDec{src: src}
	off := int64(12)
	gotFmt := false
	for {
		var ch [8]byte
		if _, err := io.ReadFull(src, ch[:]); err != nil {
			return nil, fmt.Errorf("wav: %w", err)
		}
		size := int64(binary.LittleEndian.Uint32(ch[4:]))
		off += 8
		switch string(ch[:4]) {
		case "fmt ":
			if size < 16 || size > 4096 {
				return nil, fmt.Errorf("wav: invalid fmt chunk size %d", size)
			}
			b := make([]byte, size)
			if _, err := io.ReadFull(src, b); err != nil {
				return nil, err
			}
			if len(b) < 16 {
				return nil, errors.New("wav: short fmt chunk")
			}
			tag := binary.LittleEndian.Uint16(b[0:])
			d.ch = int(binary.LittleEndian.Uint16(b[2:]))
			d.rate = int(binary.LittleEndian.Uint32(b[4:]))
			d.blockAlign = int(binary.LittleEndian.Uint16(b[12:]))
			d.bits = int(binary.LittleEndian.Uint16(b[14:]))
			if tag == 0xFFFE && len(b) >= 26 {
				tag = binary.LittleEndian.Uint16(b[24:])
			}
			switch tag {
			case 1:
			case 3:
				d.float = true
			default:
				return nil, fmt.Errorf("%w: wav codec %d", ErrUnsupported, tag)
			}
			if d.ch < 1 || d.ch > 8 || d.rate < 1000 || d.rate > 768000 ||
				(d.bits != 8 && d.bits != 16 && d.bits != 24 && d.bits != 32) || d.blockAlign != d.ch*d.bits/8 {
				return nil, fmt.Errorf("wav: invalid format (%d ch, %d Hz, %d bit)", d.ch, d.rate, d.bits)
			}
			gotFmt = true
		case "data":
			if !gotFmt || d.ch == 0 || d.blockAlign == 0 {
				return nil, errors.New("wav: data before fmt")
			}
			d.dataStart = off
			d.dataLen = size
			if size == 0 || size == 0xFFFFFFFF {
				d.dataLen = -1
			}
			return d, nil
		default:
			if _, err := src.Seek(size+size&1, io.SeekCurrent); err != nil {
				return nil, err
			}
		}
		off += size + size&1
		if string(ch[:4]) == "fmt " && size&1 == 1 {
			src.Seek(1, io.SeekCurrent)
		}
	}
}

func (d *wavDec) SampleRate() int { return d.rate }
func (d *wavDec) Interrupt()      { d.src.Interrupt() }
func (d *wavDec) Close() error    { return d.src.Close() }

func (d *wavDec) Length() int64 {
	if d.dataLen < 0 {
		return -1
	}
	return d.dataLen / int64(d.blockAlign)
}

func (d *wavDec) Read(dst []float32) (int, error) {
	frames := len(dst) / 2
	if l := d.Length(); l >= 0 && d.pos+int64(frames) > l {
		frames = int(l - d.pos)
		if frames <= 0 {
			return 0, io.EOF
		}
	}
	need := frames * d.blockAlign
	if cap(d.raw) < need {
		d.raw = make([]byte, need)
	}
	n, err := io.ReadFull(d.src, d.raw[:need])
	nf := n / d.blockAlign
	bps := d.bits / 8
	for i := 0; i < nf; i++ {
		base := i * d.blockAlign
		l := d.sample(d.raw[base:], bps)
		r := l
		if d.ch > 1 {
			r = d.sample(d.raw[base+bps:], bps)
		}
		dst[2*i], dst[2*i+1] = l, r
	}
	d.pos += int64(nf)
	if err == io.ErrUnexpectedEOF {
		err = nil
		if nf == 0 {
			err = io.EOF
		}
	}
	return nf, err
}

func (d *wavDec) sample(b []byte, bps int) float32 {
	switch {
	case d.float && bps == 4:
		return math.Float32frombits(binary.LittleEndian.Uint32(b))
	case d.float && bps == 8:
		return float32(math.Float64frombits(binary.LittleEndian.Uint64(b)))
	case bps == 1:
		return (float32(b[0]) - 128) / 128
	case bps == 2:
		return float32(int16(binary.LittleEndian.Uint16(b))) / 32768
	case bps == 3:
		v := int32(uint32(b[0])<<8|uint32(b[1])<<16|uint32(b[2])<<24) >> 8
		return float32(v) / 8388608
	case bps == 4:
		return float32(int32(binary.LittleEndian.Uint32(b))) / 2147483648
	}
	return 0
}

func (d *wavDec) SeekFrame(frame int64) error {
	if _, err := d.src.Seek(d.dataStart+frame*int64(d.blockAlign), io.SeekStart); err != nil {
		return err
	}
	d.pos = frame
	return nil
}

// ---- MP3 ----

// mp3Frame is one entry of the MP3 frame index.
type mp3Frame struct {
	offset  int64
	samples int64 // cumulative samples before this frame
}

type mp3Dec struct {
	src   Source
	dec   *mp3.Decoder
	rate  int
	raw   []byte
	index []mp3Frame
	// scan state for incremental index building
	scanOff     int64
	scanSamples int64
	scanDone    bool
	dataStart   int64
	rg          ReplayGain
}

func newMP3(src Source) (Decoder, error) {
	d := &mp3Dec{src: src, rg: readID3ReplayGain(src)}
	start, err := skipID3(src)
	if err != nil {
		return nil, err
	}
	d.dataStart, d.scanOff = start, start
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	dec, err := mp3.NewDecoder(readOnly{src})
	if err != nil {
		return nil, fmt.Errorf("mp3: %w", err)
	}
	d.dec, d.rate = dec, dec.SampleRate()
	return d, nil
}

func skipID3(r io.ReadSeeker) (int64, error) {
	var h [10]byte
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil
	}
	if string(h[:3]) != "ID3" {
		return 0, nil
	}
	n := 10 + int64(h[6]&0x7f)<<21 | int64(h[7]&0x7f)<<14 | int64(h[8]&0x7f)<<7 | int64(h[9]&0x7f)
	if h[5]&0x10 != 0 {
		n += 10
	}
	return n, nil
}

func (d *mp3Dec) ReplayGain() ReplayGain { return d.rg }
func (d *mp3Dec) SampleRate() int        { return d.rate }
func (d *mp3Dec) Length() int64          { return -1 }
func (d *mp3Dec) Interrupt()             { d.src.Interrupt() }
func (d *mp3Dec) Close() error           { return d.src.Close() }

func (d *mp3Dec) Read(dst []float32) (int, error) {
	frames := len(dst) / 2
	need := frames * 4
	if cap(d.raw) < need {
		d.raw = make([]byte, need)
	}
	n, err := io.ReadFull(d.dec, d.raw[:need])
	nf := n / 4
	for i := 0; i < nf; i++ {
		dst[2*i] = float32(int16(binary.LittleEndian.Uint16(d.raw[4*i:]))) / 32768
		dst[2*i+1] = float32(int16(binary.LittleEndian.Uint16(d.raw[4*i+2:]))) / 32768
	}
	if err == io.ErrUnexpectedEOF {
		err = nil
		if nf == 0 {
			err = io.EOF
		}
	}
	return nf, err
}

// mp3Header parses a frame header and returns frame length and samples.
func mp3Header(h []byte) (length int, samples int, ok bool) {
	if h[0] != 0xFF || h[1]&0xE0 != 0xE0 {
		return 0, 0, false
	}
	ver := (h[1] >> 3) & 3   // 0=2.5, 2=2, 3=1
	layer := (h[1] >> 1) & 3 // 1=III, 2=II, 3=I
	bri := int(h[2] >> 4)
	sri := int((h[2] >> 2) & 3)
	pad := int((h[2] >> 1) & 1)
	if ver == 1 || layer == 0 || bri == 0 || bri == 15 || sri == 3 {
		return 0, 0, false
	}
	rates := [3]int{44100, 48000, 32000}
	sr := rates[sri]
	switch ver {
	case 2:
		sr /= 2
	case 0:
		sr /= 4
	}
	var br int
	if ver == 3 {
		switch layer {
		case 3:
			br = []int{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448}[bri]
		case 2:
			br = []int{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384}[bri]
		case 1:
			br = []int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}[bri]
		}
	} else {
		if layer == 3 {
			br = []int{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256}[bri]
		} else {
			br = []int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}[bri]
		}
	}
	br *= 1000
	switch layer {
	case 3: // Layer I
		return (12*br/sr + pad) * 4, 384, true
	case 2: // Layer II
		return 144*br/sr + pad, 1152, true
	default: // Layer III
		if ver == 3 {
			return 144*br/sr + pad, 1152, true
		}
		return 72*br/sr + pad, 576, true
	}
}

// indexUntil extends the frame index until it covers sample target (or EOF).
func (d *mp3Dec) indexUntil(target int64) error {
	var h [4]byte
	for !d.scanDone && (len(d.index) == 0 || d.scanSamples <= target) {
		if _, err := d.src.Seek(d.scanOff, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.ReadFull(d.src, h[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				d.scanDone = true
				return nil
			}
			return err
		}
		l, s, ok := mp3Header(h[:])
		if !ok || l < 4 {
			d.scanOff++ // resync
			continue
		}
		d.index = append(d.index, mp3Frame{offset: d.scanOff, samples: d.scanSamples})
		d.scanOff += int64(l)
		d.scanSamples += int64(s)
	}
	return nil
}

func (d *mp3Dec) SeekFrame(frame int64) error {
	if err := d.indexUntil(frame); err != nil {
		return err
	}
	if len(d.index) == 0 {
		return errors.New("mp3: no frames")
	}
	// Find the frame containing the target sample.
	lo, hi := 0, len(d.index)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if d.index[mid].samples <= frame {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	// Start a few frames early so the bit reservoir and IMDCT overlap are primed.
	start := lo - 3
	if start < 0 {
		start = 0
	}
	if _, err := d.src.Seek(d.index[start].offset, io.SeekStart); err != nil {
		return err
	}
	dec, err := mp3.NewDecoder(readOnly{d.src})
	if err != nil {
		return fmt.Errorf("mp3 seek: %w", err)
	}
	d.dec = dec
	skip := frame - d.index[start].samples
	buf := make([]float32, 4096)
	for skip > 0 {
		k := int64(len(buf) / 2)
		if skip < k {
			k = skip
		}
		n, err := d.Read(buf[:k*2])
		if err != nil {
			return err
		}
		skip -= int64(n)
	}
	return nil
}
